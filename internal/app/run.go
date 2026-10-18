package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"noskill/internal/agent"
	"noskill/internal/approvals"
	"noskill/internal/backends"
	"noskill/internal/config"
	"noskill/internal/ids"
	"noskill/internal/orchestrator"
	"noskill/internal/perms"
	"noskill/internal/skills"
	"noskill/internal/store"
	"noskill/internal/tools"
)

// RunRequest is one request to the engine, from the CLI, the proxy or a job.
type RunRequest struct {
	Messages       []backends.Message // the conversation; the last message is the user's
	Source         string             // cli | proxy | job | tray | delegate
	Client         string             // token name for the proxy
	ClientSkills   func(name string) bool
	ConversationFP string
	ParentID       string

	Model    string   // pin a model ("" or "noskill/auto" = route)
	TaskType string   // skip classification (delegation)
	Preset   string   // fast | balanced | best; "" = config
	Skills   []string // force these skills
	NoSkills bool     // plain model call, no tools (proxy passthrough, delegation)
	NoLearn  bool     // no trace, no memory, no outcome signals
	Think    *bool    // nil = off for internal steps and the answer; true when the client asked
	Final    *backends.Options
	Headless bool // a confirm tool parks the run instead of waiting
	Allow    []string

	// ApprovalMode for chat clients: "" (terminal/Prompt), "chat" (park; the user answers in the
	// next message), "wait" (hold the request open), "deny" (decline every confirm action).
	ApprovalMode string
	PreApproved  map[string]bool // approvals.CallKey values already granted (a resumed run)
	ResumeToken  json.RawMessage // stored on a parked run's approval
	cont         *Continuation   // set by ResumeRequest: continue a parked run
	// trial is a skill that isn't loaded (a draft being verified, a refinement on trial): it's the
	// run's only primary skill. tools, when set, wraps the run's tools (verification replays
	// recorded results through it).
	trial *skills.Skill
	tools func(agent.Tools) agent.Tools
	// exclude lists models already tried for this request: an escalated run (FR-O5) routes
	// around them and never escalates again.
	exclude []string
	// plusLearned (eval agent tasks): with Skills forced, the best-matching active learned skill
	// joins them, the way it would in a normal run. autoConfirm approves every confirm action
	// without asking (eval tasks in their own temporary folder; the broker still checks paths).
	plusLearned bool
	autoConfirm bool
	// OnStart is called once the plan is made and the run exists, before any model output.
	OnStart func(runID, model string, skills []string)

	MaxSteps, MaxTokens int
	OnEvent             func(agent.Event)
	Prompt              func(ctx context.Context, it approvals.Item) (bool, error) // ask at the terminal
	ApprovalWait        time.Duration
	QueueWait           time.Duration // how long to wait for the generation slot; 0 = no limit
}

type RunResult struct {
	RunID       string
	Answer      string
	Thinking    string
	Model       string
	Skills      []string
	Stop        agent.StopReason
	WaitingFor  string
	Steps       int
	Usage       backends.Usage
	TracePath   string
	Elapsed     time.Duration
	Class       orchestrator.Classification
	Explanation orchestrator.Explanation
	Swapped     bool
	Trimmed     bool
	// EscalatedFrom lists the runs that failed before this one answered, on other models (FR-O5).
	EscalatedFrom []string
}

// ErrBusy: the generation slot stayed taken past QueueWait (the proxy answers 503).
var ErrBusy = errors.New("noskill is busy with another request")

type slotKey struct{}

// acquire takes the generation slot. A sub-run (delegation) inherits its parent's. Background
// work (jobs) lets interactive requests that are waiting go first (PROXY.md "Concurrency").
func (a *App) acquire(ctx context.Context, wait time.Duration, background bool) (func(), context.Context, error) {
	if ctx.Value(slotKey{}) != nil {
		return func() {}, ctx, nil
	}
	if background {
		for a.interactive.Load() > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	} else {
		a.interactive.Add(1)
		defer a.interactive.Add(-1)
		a.preemptLearning() // LEARNING.md "Resources": a request takes the model back at once
	}
	var timer <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}
	select {
	case a.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-a.slots }) }, context.WithValue(ctx, slotKey{}, true), nil
	case <-ctx.Done():
		return nil, ctx, ctx.Err()
	case <-timer:
		return nil, ctx, ErrBusy
	}
}

func lastUser(msgs []backends.Message) (text string, images int) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == backends.RoleUser {
			return msgs[i].Content, len(msgs[i].Images)
		}
	}
	return "", 0
}

func fingerprint(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(s), " "))))
	return hex.EncodeToString(h[:8])
}

// plan is everything decided before the model starts.
type plan struct {
	models    []Model
	pick      Model
	decision  orchestrator.Decision
	class     orchestrator.Classification
	primaries []*skills.Skill
	selected  []*skills.Skill
	matches   []skills.Match
	pinned    bool
	// burst: the pick only fits because free memory grew the budget; its memory goes back soon
	// after the run (ADR-0012).
	burst bool
}

// Plan classifies the request, selects skills and picks a model without running anything. `route
// explain` prints it; Run executes it.
func (a *App) Plan(ctx context.Context, req RunRequest) (*plan, error) {
	models, errs := a.Models(ctx)
	var chat []Model
	for _, m := range models {
		if m.chat() {
			chat = append(chat, m)
		}
	}
	if len(chat) == 0 {
		if len(errs) > 0 || len(a.Backends) == 0 {
			return nil, &NoBackendError{Errs: errs}
		}
		return nil, errors.New("no chat model is installed; pull one, for example `ollama pull qwen3:4b`")
	}
	text, images := lastUser(req.Messages)
	p := &plan{models: models}

	// Skills first: a single selected skill can name the task type, which saves the decision call.
	var err error
	if p.primaries, p.matches, err = a.pickSkills(ctx, req, text, models); err != nil {
		return nil, err
	}
	if req.trial != nil {
		deps, err := a.Skills.Closure(req.trial.Meta.DependsOn...)
		if err != nil {
			return nil, err
		}
		p.selected = append(deps, req.trial)
	} else if p.selected, err = a.closure(p.primaries); err != nil {
		return nil, err
	}

	var pinned *Model
	if req.Model != "" && req.Model != "noskill/auto" {
		m, err := a.FindModel(ctx, req.Model)
		if err != nil {
			return nil, err
		}
		if !m.chat() {
			return nil, &UserError{fmt.Sprintf("%s is not a chat model (capabilities: %v)", m.Name, m.Capabilities)}
		}
		pinned = &m
	}

	r := orchestrator.Request{Text: text, Images: images, Tools: len(skills.Tools(p.selected)) > 0, InputTokens: estimateTokens(req.Messages)}
	switch {
	case req.TaskType != "":
		p.class = orchestrator.Classification{TaskType: req.TaskType, Confidence: 1, Source: "delegate", Difficulty: -1}
		if r.Tools {
			p.class.Needs = append(p.class.Needs, "tools")
		}
	case pinned != nil:
		p.class = orchestrator.Classification{TaskType: "chat", Confidence: 1, Source: "pinned", Difficulty: -1}
		if r.Tools {
			p.class.Needs = append(p.class.Needs, "tools")
		}
		if images > 0 {
			p.class.Needs = append(p.class.Needs, "vision")
		}
	default:
		if len(p.primaries) == 1 && len(p.primaries[0].Meta.TaskTypes) > 0 && strongSkill(req, p.matches) {
			r.SkillType = p.primaries[0].Meta.TaskTypes[0]
		}
		cl := orchestrator.Classifier{ThresholdFor: func(m string) float64 { return a.decisionThreshold(ctx, DecisionRouter, m) }}
		if a.routingNeedsTaskType(models) {
			cl.Decider = a.decider(ctx, models)
		}
		if p.class, err = cl.Classify(ctx, r); err != nil {
			return nil, err
		}
		if cl.Decider == nil && p.class.Source == "default" {
			// Nothing to decide: no rule can pick a different model for another task type, so the
			// classification call (about 2 s on a 4B model without a decision model) is skipped.
			p.class.Uncertain, p.class.Source = false, "not needed"
			p.class.Note = "no routing rule depends on the task type"
			if miss := a.missingRuleModels(models); len(miss) > 0 {
				p.class.Note = "task-type rules skipped: " + strings.Join(miss, "; ")
			}
		}
	}

	if pinned != nil {
		p.pick, p.pinned = *pinned, true
		p.decision = orchestrator.Decision{Model: pinned.Name, Swap: !pinned.Loaded,
			Explanation: orchestrator.Explanation{Class: p.class, Picked: pinned.Name, Why: "pinned by the request"}}
		return p, nil
	}

	cfg := a.Cfg.Orchestrator
	if req.Preset != "" {
		cfg.Preset = req.Preset
	}
	budget, base, note := a.liveBudget(models)
	st := orchestrator.State{Budget: budget, BudgetNote: note, Now: time.Now()}
	for _, m := range models {
		if !m.Loaded {
			continue
		}
		lm := backends.LoadedModel{Name: m.Name, SizeBytes: m.LoadedSz}
		if m.chat() && st.Primary == "" {
			st.Primary = m.Name
		} else if !m.chat() {
			st.Resident = append(st.Resident, lm)
		}
	}
	// A model `train lora` made is never picked on its own: an adopted one stands in for its base
	// below (ADR-0015).
	hidden, standIn := a.trainedRouting()
	router := orchestrator.Router{Cfg: cfg, Profiles: a.profiles(ctx, models), Exclude: append(slices.Clone(req.exclude), hidden...)}
	if ev, err := a.DB.BanditEvidence(ctx, p.class.TaskType, orchestrator.Bucket(p.class.Difficulty)); err == nil && len(ev) > 0 {
		router.Evidence = map[string]orchestrator.Evidence{}
		for m, e := range ev {
			router.Evidence[m] = orchestrator.Evidence{Alpha: e.Alpha, Beta: e.Beta}
		}
	}
	// Exploration: a few interactive requests only, never jobs or internal runs (FR-O4).
	if interactiveSource(req.Source) && !req.NoLearn && len(req.exclude) == 0 && rand.Float64() < cfg.Explore {
		router.Explore, router.Rand = true, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if req.Source == "job" {
		st.Background, st.Idle = true, a.idleFor(ctx)
	}
	d, err := router.Route(p.class, r, st)
	p.decision = d
	if err != nil {
		return p, fmt.Errorf("%w\n%s", err, d.Explanation.Text())
	}
	var resident int64
	for _, m := range models {
		if m.Name == d.Model {
			p.pick = m
		}
		if m.Loaded && !m.chat() {
			resident += m.LoadedSz
		}
	}
	a.standIn(p, models, standIn)
	p.burst = budget > base && resident+modelSize(p.pick) > base
	return p, nil
}

// modelSize is what a model takes loaded: measured when it is, else its file size.
func modelSize(m Model) int64 {
	if m.Loaded && m.LoadedSz > 0 {
		return m.LoadedSz
	}
	return m.SizeBytes
}

// burstHold is how long a model loaded on burst memory stays after its run, instead of Ollama's
// default: long enough for a follow-up, short enough to give the memory back (ADR-0012).
const burstHold = 2 * time.Minute

// releaseBurst shortens the keep-alive of a model that only fit thanks to free memory.
func (a *App) releaseBurst(m Model) {
	l, ok := m.NB.B.(backends.CtxLoader)
	if !ok {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		l.LoadCtx(ctx, m.Name, a.numCtx(m), burstHold) // same num_ctx, so Ollama keeps it loaded and only resets the timer
	}()
}

func strategyFor(m Model) backends.ToolStrategy {
	switch {
	case m.Strategy != "":
		return m.Strategy
	case m.Has(backends.CapTools):
		return backends.StrategyNative
	case m.Has(backends.CapSchema):
		return backends.StrategyConstrained
	default:
		return backends.StrategyPrompted
	}
}

// handoffTo applies swap rule 8 after a delegation ran on model to: when that pushed the driver
// out of memory, to finishes the run if it can drive it and isn't much worse at the task.
func (a *App) handoffTo(ctx context.Context, p *plan, to string, needCtx int) *agent.Driver {
	loaded, err := p.pick.NB.B.Loaded(ctx)
	if err != nil || slices.ContainsFunc(loaded, func(l backends.LoadedModel) bool { return l.Name == p.pick.Name }) {
		return nil // the driver is still there: nothing to swap back
	}
	models, _ := a.Models(ctx)
	i := slices.IndexFunc(models, func(m Model) bool { return m.Name == to })
	if i < 0 {
		return nil
	}
	next := models[i]
	prof := a.profiles(ctx, []Model{p.pick, next})
	if len(prof) != 2 {
		return nil
	}
	ev, _ := a.DB.BanditEvidence(ctx, p.class.TaskType, orchestrator.Bucket(p.class.Difficulty))
	conv := func(e store.Evidence) orchestrator.Evidence {
		return orchestrator.Evidence{Alpha: e.Alpha, Beta: e.Beta}
	}
	ok, why := orchestrator.Handoff(p.class, prof[0], prof[1], conv(ev[p.pick.Name]), conv(ev[to]), needCtx, a.Cfg.Orchestrator.MinSwapGain)
	if !ok {
		return nil
	}
	d := &agent.Driver{Backend: next.NB.B, Model: next.Name, Strategy: strategyFor(next), CanConstrain: next.Has(backends.CapSchema),
		AlwaysThinks: next.Has(backends.CapThinkAlways), SmallModel: a.smallModel(next), ContextTokens: a.numCtx(next),
		StepOutputTokens: agent.DefaultBudget().StepOutputTokens, Note: "Handing off: " + why}
	if d.AlwaysThinks {
		d.StepOutputTokens *= 2
	}
	return d
}

// deferPoll is how often a job waiting for idle time checks again (swap rule 6).
var deferPoll = time.Minute

// interactiveSource: someone is waiting for this run's answer.
func interactiveSource(src string) bool {
	return src == "" || src == "cli" || src == "proxy" || src == "tray"
}

// idleFor is how long nobody has been using noskill interactively (swap rule 6).
func (a *App) idleFor(ctx context.Context) time.Duration {
	if a.interactive.Load() > 0 {
		return 0
	}
	last, err := a.DB.LastInteractive(ctx)
	if err != nil || last.IsZero() {
		return 24 * time.Hour
	}
	return time.Since(last)
}

// routingNeedsTaskType: the task type only matters when a rule names one and that rule's model is
// installed. (Per-task benchmark scores will add a second reason in Phase 2.)
func (a *App) routingNeedsTaskType(models []Model) bool {
	for _, r := range a.Cfg.Orchestrator.Rules {
		if r.When.TaskType == "" {
			continue
		}
		for _, m := range models {
			if m.chat() && m.Name == r.Use {
				return true
			}
		}
	}
	return false
}

// missingRuleModels lists the task-type rules whose model isn't an installed chat model, so
// `route explain` can say why such a rule never fires (FR-O6).
func (a *App) missingRuleModels(models []Model) []string {
	var out []string
	for i, r := range a.Cfg.Orchestrator.Rules {
		if r.When.TaskType == "" {
			continue
		}
		if !slices.ContainsFunc(models, func(m Model) bool { return m.chat() && m.Name == r.Use }) {
			out = append(out, fmt.Sprintf("rule %d (%s → %s): %s isn't installed (`noskill models pull %s`)", i+1, r.When.TaskType, r.Use, r.Use, r.Use))
		}
	}
	return out
}

func estimateTokens(msgs []backends.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)/4 + 4 + len(m.Images)*800
	}
	return n
}

// Run executes a request end to end.
func (a *App) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	started := time.Now()
	if len(req.Messages) == 0 {
		return nil, errors.New("empty request")
	}
	base := ctx
	release, ctx, err := a.acquire(base, req.QueueWait, req.Source == "job")
	if err != nil {
		return nil, err
	}
	defer func() { release() }()

	p, err := a.Plan(ctx, req)
	// Swap rule 6: a job that needs another model waits, without holding the slot, until you've
	// been away long enough.
	for err == nil && p.decision.Defer {
		release()
		select {
		case <-base.Done():
			return nil, base.Err()
		case <-time.After(deferPoll):
		}
		if release, ctx, err = a.acquire(base, req.QueueWait, true); err != nil {
			return nil, err
		}
		p, err = a.Plan(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	runID := ids.New()
	if req.Source == "" {
		req.Source = "cli"
	}
	text, _ := lastUser(req.Messages)

	// Trace and run row.
	var tw *agent.TraceWriter
	tracePath := ""
	secretVals := a.secretValues(p.selected)
	if !req.NoLearn {
		if tw, err = agent.NewTraceWriter(a.Dirs.Traces(), runID, started, secretVals); err != nil {
			return nil, fmt.Errorf("can't write the trace: %w", err)
		}
		tracePath = tw.Path
		defer tw.Close()
	}
	var owner map[string]string // tool name as the model sees it → skill
	var pattern []string        // successful tool calls, in order (LEARNING.md §2)
	var finalStep, shown bool   // shown: answer text reached the client, so the run can't be redone
	emit := func(e agent.Event) {
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		switch e.Kind {
		case agent.EventStep:
			finalStep = e.Final
		case agent.EventDelta:
			shown = shown || (finalStep && e.Content != "")
		case agent.EventAnswer:
			shown = shown || e.Content != ""
		}
		if e.Kind == agent.EventToolResult && e.Err == "" {
			pattern = append(pattern, e.Tool)
		}
		if e.Skill == "" && e.Tool != "" && (e.Kind == agent.EventToolCall || e.Kind == agent.EventToolResult || e.Kind == agent.EventToolDenied) {
			e.Skill = owner[e.Tool]
		}
		if tw != nil {
			tw.Write(e)
		}
		if req.OnEvent != nil {
			req.OnEvent(e)
		}
	}
	row := store.Run{ID: runID, ParentID: req.ParentID, Source: req.Source, Client: req.Client, ConversationFP: req.ConversationFP,
		DriverModel: p.pick.Name, Status: "running", NoLearn: req.NoLearn, Pinned: p.pinned, TracePath: tracePath, StartedAt: started}
	if !p.pinned {
		row.TaskType, row.Bucket = p.class.TaskType, orchestrator.Bucket(p.class.Difficulty)
	}
	if err := a.DB.InsertRun(ctx, row); err != nil {
		return nil, err
	}
	a.recordRouting(ctx, runID, p.class)
	skillVersions := map[string]string{}
	var skillNames []string
	for _, s := range p.selected {
		skillVersions[s.Name] = s.Meta.Version
		skillNames = append(skillNames, s.Name)
	}
	a.DB.AddRunSkills(ctx, runID, skillVersions)
	if !req.NoLearn && req.ParentID == "" {
		a.noteReask(ctx, req.Client, text, runID, started)
	}

	emit(agent.Event{Kind: agent.EventRunStart, V: agent.TraceVersion, Content: text, Model: p.pick.Name,
		Source: req.Source, Parent: req.ParentID, Skills: skillNames, Tool: p.class.TaskType})
	reqContent := text
	if r := []rune(reqContent); len(r) > 400 {
		reqContent = string(r[:400]) + "…"
	}
	emit(agent.Event{Kind: agent.EventRoute, Model: p.pick.Name, Content: reqContent,
		Note: p.decision.Explanation.Text(), Tool: p.class.TaskType})
	emit(agent.Event{Kind: agent.EventSelect, Note: strings.Join(skillNames, ", ")})
	if req.OnStart != nil {
		req.OnStart(runID, p.pick.Name, skillNames)
	}

	if !p.pick.Loaded {
		emit(agent.Event{Kind: agent.EventProgress, Note: "Loading " + p.pick.Name + "…"})
	}
	swapped, err := a.prepareModel(ctx, p)
	if err != nil {
		a.finish(ctx, row, started, nil, err, req, "")
		return nil, err
	}

	// Tools.
	svc := &runServices{a: a, runID: runID, model: p.pick}
	exec := a.executor(runID, p, req, svc)
	owner = map[string]string{}
	for _, r := range exec.Offered {
		owner[r.Name] = r.Skill.Name
	}
	for _, w := range exec.Problems() {
		fmt.Fprintln(a.Opts.Stderr, "noskill:", w)
	}

	strategy := strategyFor(p.pick)
	small := a.smallModel(p.pick)
	var msgs []backends.Message
	trimmed := false
	startStep := 0
	var pending []backends.ToolCall
	if req.cont != nil {
		// The conversation as the model saw it when the run parked; the approved call runs first.
		msgs, pending, startStep = req.cont.Messages, req.cont.Pending, req.cont.Steps
		if req.cont.Strategy != "" {
			strategy = req.cont.Strategy
		}
		a.DB.SetRunStatus(ctx, req.cont.RunID, "resumed")
		emit(agent.Event{Kind: agent.EventProgress, Note: "Resuming run " + req.cont.RunID + " after approval"})
	} else {
		msgs, trimmed = a.buildMessages(ctx, req, p, small, exec)
	}

	b := agent.DefaultBudget()
	b.MaxSteps, b.MaxTokens = a.Cfg.Agent.MaxSteps, a.Cfg.Agent.MaxTokens
	if req.MaxSteps > 0 {
		b.MaxSteps = req.MaxSteps
	}
	if req.MaxTokens > 0 {
		b.MaxTokens = req.MaxTokens
	}
	b.ContextTokens = a.numCtx(p.pick)
	alwaysThinks := p.pick.Has(backends.CapThinkAlways)
	if alwaysThinks {
		// Reasoning counts against num_predict: 1024 cut qwen3:4b off before its tool call.
		b.StepOutputTokens *= 2
	}
	think := req.Think
	if think == nil {
		off := false
		think = &off
	}
	final := req.Final
	loop := &agent.Loop{Backend: p.pick.NB.B, OnEvent: emit}
	if len(exec.Offered) > 0 {
		loop.Tools = exec
		if req.tools != nil {
			loop.Tools = req.tools(exec)
		}
	}
	var handoff func(context.Context) *agent.Driver
	if !p.pinned && req.cont == nil && len(exec.Offered) > 0 {
		cur := p.pick.Name
		handoff = func(hctx context.Context) *agent.Driver {
			if to := svc.takeDelegated(); to != "" && to != cur {
				if d := a.handoffTo(hctx, p, to, b.ContextTokens); d != nil {
					cur = d.Model
					return d
				}
			}
			return nil
		}
	}
	res, runErr := loop.Run(ctx, agent.Request{
		RunID: runID, Model: p.pick.Name, Messages: msgs, Strategy: strategy, CanConstrain: p.pick.Has(backends.CapSchema),
		Think: think, AlwaysThinks: alwaysThinks, SmallModel: small, Budget: b, Final: final, Pending: pending, StartStep: startStep,
		Handoff: handoff,
	})
	driver := p.pick.Name
	if res.Model != "" {
		driver = res.Model
	}
	if entries, err := a.DB.UndoEntries(ctx, runID); err == nil {
		for _, e := range entries {
			emit(agent.Event{Kind: agent.EventFileEffect, Op: e.Op, From: e.From, To: e.To})
		}
	}
	emit(agent.Event{Kind: agent.EventEnd, Note: string(res.Stop), Tokens: res.Usage.Total(), Ms: time.Since(started).Milliseconds()})

	row.Steps, row.Tokens, row.Swaps = res.Steps, res.Usage.Total(), 0
	row.ToolCalls, row.ToolPattern = len(pattern), strings.Join(pattern, ">")
	if swapped {
		row.Swaps = 1
	}
	a.finish(ctx, row, started, &res, runErr, req, driver)
	if p.burst && driver == p.pick.Name {
		a.releaseBurst(p.pick)
	}
	a.capCaches(driver)
	if res.Stop == agent.StopWaiting && res.WaitingFor != "" && len(res.Pending) > 0 {
		names := make([]string, len(p.primaries))
		for i, s := range p.primaries {
			names[i] = s.Name
		}
		a.park(ctx, res.WaitingFor, Continuation{RunID: runID, Model: driver, Skills: names, Strategy: strategy,
			Messages: res.Messages, Pending: res.Pending, Steps: res.Steps, Source: req.Source, Client: req.Client,
			ConversationFP: req.ConversationFP, Allow: req.Allow, Think: req.Think, Final: req.Final, NoLearn: req.NoLearn})
	}
	if runErr == nil && !req.NoLearn && res.Stop == agent.StopAnswered && a.Cfg.Memory.AutoExtract {
		a.learnFacts(ctx, req.Messages)
	}
	if runErr == nil && !req.NoLearn && res.Stop == agent.StopAnswered {
		a.judgeAfter(row, text, res.Answer, actionsText(res.Messages), p.primaries, p.models)
	}
	out := &RunResult{RunID: runID, Answer: res.Answer, Thinking: res.Thinking, Model: driver, Skills: skillNames, Stop: res.Stop,
		WaitingFor: res.WaitingFor, Steps: res.Steps, Usage: res.Usage, TracePath: tracePath, Elapsed: time.Since(started),
		Class: p.class, Explanation: p.decision.Explanation, Swapped: swapped, Trimmed: trimmed}
	if why := a.escalation(ctx, req, p, res, runErr, runID, len(pattern), shown); why != "" {
		emit(agent.Event{Kind: agent.EventProgress, Note: "Escalating: " + why + "; trying the next-best model"})
		next := req
		next.exclude = append(slices.Clone(req.exclude), p.pick.Name)
		next.QueueWait = 0
		if res2, err2 := a.Run(ctx, next); err2 == nil || res2 != nil {
			res2.EscalatedFrom = append([]string{runID}, res2.EscalatedFrom...)
			return res2, err2
		}
	}
	return out, runErr
}

// escalation says why a finished run should be retried on the next-best model (ORCHESTRATOR.md
// "Escalation", FR-O5), or "". Only once per request, never for pinned or internal runs, and only
// when a retry can't repeat a side effect or show the client a second answer: nothing written, and no skill that writes, runs
// programs or asks before acting made a successful call.
func (a *App) escalation(ctx context.Context, req RunRequest, p *plan, res agent.Result, runErr error, runID string, calls int, shown bool) string {
	if shown || p.pinned || len(req.exclude) > 0 || req.trial != nil || req.cont != nil || req.Source == "learning" || ctx.Err() != nil {
		return ""
	}
	var why string
	switch {
	case runErr != nil && !errors.Is(runErr, context.Canceled) && res.Stop != agent.StopCancelled && res.Stop != agent.StopWaiting:
		why = "the run failed (" + clipRunes(runErr.Error(), 120) + ")"
	case res.Stop == agent.StopMaxSteps || res.Stop == agent.StopMaxTokens:
		why = "the run hit its " + string(res.Stop) + " limit"
	case runErr == nil && res.Stop == agent.StopAnswered && strings.TrimSpace(res.Answer) == "":
		why = "the answer was empty"
	default:
		return ""
	}
	if entries, err := a.DB.UndoEntries(ctx, runID); err != nil || len(entries) > 0 {
		return ""
	}
	if calls > 0 {
		for _, s := range p.selected {
			pm := s.Meta.Permissions
			if pm.Exec || pm.Subprocess || len(s.Meta.Confirm) > 0 || slices.ContainsFunc(pm.Filesystem, func(f string) bool { return strings.HasPrefix(f, "write:") }) {
				return ""
			}
		}
	}
	return why
}

// actionsText lists the tool calls in a conversation, one per line, for the judge.
func actionsText(msgs []backends.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			fmt.Fprintf(&b, "%s(%s)\n", c.Name, clipRunes(string(c.Args), 200))
		}
	}
	return b.String()
}

// finish writes the run's final state and its outcome signals.
func (a *App) finish(ctx context.Context, row store.Run, started time.Time, res *agent.Result, runErr error, req RunRequest, model string) {
	ctx = context.WithoutCancel(ctx)
	row.EndedAt = time.Now()
	row.Ms = row.EndedAt.Sub(started).Milliseconds()
	row.DriverModel = model
	switch {
	case res != nil && res.Stop == agent.StopWaiting:
		row.Status = "waiting"
	case runErr != nil && (errors.Is(runErr, context.Canceled) || (res != nil && res.Stop == agent.StopCancelled)):
		row.Status = "cancelled"
	case runErr != nil:
		row.Status = "failed"
	default:
		row.Status = "done"
	}
	a.DB.FinishRun(ctx, row)
	if !req.NoLearn {
		a.captureOutcome(ctx, row, res)
	}
}

// ---- model preparation ----

// prepareModel makes room and loads the chosen model at its fixed context size. It returns
// whether another model had to be swapped out.
func (a *App) prepareModel(ctx context.Context, p *plan) (bool, error) {
	m := p.pick
	swapped := false
	for _, o := range p.models {
		if !o.Loaded || o.Name == m.Name || o.NB.ID != m.NB.ID {
			continue
		}
		unloadResident := p.decision.UnloadResident && !o.chat()
		unloadOtherChat := o.chat() && (p.decision.Swap || p.decision.UnloadResident)
		if unloadResident || unloadOtherChat {
			if err := o.NB.B.Load(ctx, o.Name, 0); err != nil {
				return false, fmt.Errorf("couldn't unload %s to make room: %w", o.Name, err)
			}
			if o.chat() {
				swapped = true
			}
		}
	}
	if !m.Loaded {
		keep := 30 * time.Minute
		var err error
		if cl, ok := m.NB.B.(backends.CtxLoader); ok {
			err = cl.LoadCtx(ctx, m.Name, a.numCtx(m), keep)
		} else {
			err = m.NB.B.Load(ctx, m.Name, keep)
		}
		if err != nil {
			return swapped, fmt.Errorf("couldn't load %s: %w", m.Name, err)
		}
		a.invalidateModels()
	}
	return swapped, nil
}

func (a *App) smallModel(m Model) bool {
	switch a.Cfg.Agent.SmallModelMode {
	case "on":
		return true
	case "off":
		return false
	}
	return a.numCtx(m) <= 8192 || (m.ParamsB > 0 && m.ParamsB < 7)
}

// ---- skills ----

func (a *App) pickSkills(ctx context.Context, req RunRequest, text string, models []Model) (primaries []*skills.Skill, matches []skills.Match, err error) {
	if req.NoSkills {
		return nil, nil, nil
	}
	if req.trial != nil {
		return []*skills.Skill{req.trial}, []skills.Match{{Skill: req.trial, Score: 1, Method: "forced"}}, nil
	}
	usable, err := a.UsableSkills(ctx)
	if err != nil {
		return nil, nil, err
	}
	trialing := a.Cfg.Learning.Enabled && !req.NoLearn
	if trialing { // candidates on trial compete like any skill; gateTrials decides after; naming one
		// with --skill is a yes (FR-L4)
		onTrial, err := a.trialSkills(ctx)
		if err != nil {
			return nil, nil, err
		}
		usable = append(usable, onTrial...)
	}
	allowed := func(s *skills.Skill) bool { return req.ClientSkills == nil || req.ClientSkills(s.Name) }
	if len(req.Skills) > 0 {
		for _, n := range req.Skills {
			var found *skills.Skill
			for _, s := range usable {
				if s.Name == n {
					found = s
				}
			}
			if found == nil {
				if s, ok := a.Skills.Get(n); ok && s.NeedsApproval() {
					return nil, nil, &UserError{fmt.Sprintf("skill %s comes from this folder and isn't enabled: review it with `noskill skills show %s`, then `noskill skills enable %s`", n, n, n)}
				}
				return nil, nil, &UserError{fmt.Sprintf("no usable skill named %q (see `noskill skills list`)", n)}
			}
			if !allowed(found) {
				return nil, nil, &UserError{fmt.Sprintf("this client may not use skill %s", n)}
			}
			primaries = append(primaries, found)
		}
		if req.plusLearned {
			if m, ok, err := a.bestLearned(ctx, usable, primaries, text, allowed); err != nil {
				return nil, nil, err
			} else if ok {
				ms := []skills.Match{m}
				if trialing { // a candidate still asks first; a refinement on trial stands in
					ms = a.withRefinements(ctx, req, text, a.gateTrials(ctx, req, text, ms))
				}
				if len(ms) > 0 {
					primaries = append([]*skills.Skill{ms[0].Skill}, primaries...)
					matches = ms[:1]
				}
			}
		}
		return primaries, matches, nil
	}
	var cands []*skills.Skill
	for _, s := range usable {
		if allowed(s) {
			cands = append(cands, s)
		}
	}
	sel := &skills.Selector{Cache: a.DB}
	if em := a.embedModel(models); em != nil {
		sel.Embedder, sel.EmbedModel = em.NB.B, em.Name
	}
	k := 3
	if len(models) > 0 {
		k = 2 // small models do better with fewer skills in view
	}
	matches, err = sel.Select(ctx, cands, text, k)
	if err != nil {
		return nil, nil, err
	}
	if trialing {
		matches = a.withRefinements(ctx, req, text, a.gateTrials(ctx, req, text, matches))
	}
	for _, m := range matches {
		primaries = append(primaries, m.Skill)
	}
	return primaries, matches, nil
}

// bestLearned is the active learned skill the selector would show for text among the usable
// ones, if any, leaving out the skills already chosen.
func (a *App) bestLearned(ctx context.Context, usable, chosen []*skills.Skill, text string, allowed func(*skills.Skill) bool) (skills.Match, bool, error) {
	var cands []*skills.Skill
	for _, s := range usable {
		if s.Source == skills.SourceLearned && allowed(s) && !slices.Contains(chosen, s) {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return skills.Match{}, false, nil
	}
	sel := &skills.Selector{Cache: a.DB}
	if models, _ := a.Models(ctx); models != nil {
		if em := a.embedModel(models); em != nil {
			sel.Embedder, sel.EmbedModel = em.NB.B, em.Name
		}
	}
	ms, err := sel.Select(ctx, cands, text, 1)
	if err != nil || len(ms) == 0 {
		return skills.Match{}, false, err
	}
	return ms[0], true, nil
}

// strongSkill says whether the selected skill is a strong enough signal to name the task type
// without a decision call (ORCHESTRATOR.md "Classification"): the request forced it, a trigger
// phrase matched, or an embedding match is confident. A weak keyword match ("write … list" picking
// files for a code question) would otherwise hide a `code` rule (SPEC §6 criterion 4).
func strongSkill(req RunRequest, matches []skills.Match) bool {
	if len(req.Skills) > 0 {
		return true
	}
	if len(matches) != 1 {
		return false
	}
	switch m := matches[0]; m.Method {
	case "trigger", "forced":
		return true
	case "embedding":
		return m.Score >= strongEmbedding
	}
	return false
}

const strongEmbedding = 0.7

func (a *App) embedModel(models []Model) *Model {
	want := a.Cfg.Orchestrator.EmbedModel
	for i := range models {
		if models[i].Has(backends.CapEmbed) && (models[i].Name == want || strings.HasPrefix(models[i].Name, want+":")) {
			return &models[i]
		}
	}
	return nil
}

// closure adds the skills the primaries depend on, dependencies first. A dependency that isn't
// usable (disabled, missing) is an error: running with half a skill fails confusingly later.
func (a *App) closure(primaries []*skills.Skill) ([]*skills.Skill, error) {
	if len(primaries) == 0 {
		return nil, nil
	}
	names := make([]string, len(primaries))
	for i, s := range primaries {
		names[i] = s.Name
	}
	all, err := a.Skills.Closure(names...)
	if err != nil {
		return nil, err
	}
	// A primary may be a version that isn't loaded (a refinement on trial): keep it, not the
	// loaded version of the same name.
	for i, s := range all {
		for _, p := range primaries {
			if p.Name == s.Name {
				all[i] = p
			}
		}
	}
	return all, nil
}

// ---- tools ----

func (a *App) executor(runID string, p *plan, req RunRequest, svc tools.Services) *tools.Executor {
	primary := map[string]bool{}
	for _, s := range p.primaries {
		primary[s.Name] = true
	}
	res := a.resolver()
	// Scopes the user granted with --allow or [skills.<name>] allow, for primary skills only.
	userScopes := func(sk *skills.Skill) []string {
		out := append([]string(nil), req.Allow...)
		if v, ok := a.userSetting(sk.Name, "allow"); ok {
			out = append(out, splitList(v)...)
		}
		return out
	}
	e := &tools.Executor{
		RunID: runID, Offered: skills.Tools(p.selected), Registry: a.Registry, Svc: svc, Secrets: a.Secrets,
		Trash: a.Trash, Undo: a.DB, Runner: a.Runner, Interp: a.Interp,
		Setting: a.userSetting,
		Expand:  res.Expand,
		Perms: func(sk *skills.Skill) (perms.Set, error) {
			set, err := tools.SkillPerms(res, sk)
			if err != nil {
				return set, err
			}
			switch {
			case primary[sk.Name]:
				extra, err := res.Compile(userScopes(sk))
				if err != nil {
					return set, err
				}
				set.Files = append(set.Files, extra...)
			case len(sk.Meta.Permissions.Filesystem) == 0:
				// A library skill (files) declares no folders: it's "granted per call site", so
				// it gets the scopes of the primary skills that brought it in. Narrowing, not widening.
				for _, ps := range p.primaries {
					pset, err := tools.SkillPerms(res, ps)
					if err != nil {
						return set, err
					}
					extra, err := res.Compile(userScopes(ps))
					if err != nil {
						return set, err
					}
					set.Files = append(set.Files, pset.Files...)
					set.Files = append(set.Files, extra...)
				}
			}
			return set, nil
		},
	}
	source := "skill"
	switch req.Source {
	case "proxy", "job":
		source = req.Source
	}
	cf := &approvals.Confirmer{Q: a.Approvals, Source: source,
		Headless: req.Headless, Wait: req.ApprovalWait, ConversationFP: req.ConversationFP, Prompt: req.Prompt,
		Client: req.Client, Pre: req.PreApproved, Resume: req.ResumeToken, Auto: req.autoConfirm}
	switch req.ApprovalMode {
	case "chat":
		cf.Headless = true
	case "deny":
		cf.Deny = true
	}
	e.Confirm = cf.Confirm
	// Any network permission means an egress proxy.
	for _, s := range p.selected {
		if len(s.Meta.Permissions.Network) > 0 {
			if _, err := a.Egress(); err != nil {
				fmt.Fprintln(a.Opts.Stderr, "noskill: egress proxy:", err)
			}
			break
		}
	}
	return e
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolver expands path tokens. ${config.x} refuses values that came from a project config
// (SECURITY.md): a repository mustn't choose its own file access.
func (a *App) resolver() perms.Resolver {
	l := a.Opts.Loaded
	bound, _ := a.Volumes().Bindings(context.Background()) // label → UUID: refuses a look-alike drive
	return perms.Resolver{
		Config: func(key string) (string, bool, bool) {
			v, ok := config.Get(l.Config, key)
			if !ok {
				return "", false, false
			}
			return v, l.Sources[key] == config.LayerProject, true
		},
		Volumes:     a.Volumes(),
		VolumeUUIDs: bound,
	}
}

// userSetting reads [skills.<name>] values, ignoring any that came from a project config.
func (a *App) userSetting(skill, key string) (string, bool) {
	l := a.Opts.Loaded
	m := l.Config.Skills.Settings[skill]
	v, ok := m[key]
	if !ok {
		return "", false
	}
	if l.SkillSources[skill][key] == config.LayerProject {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, ","), true
	}
	return fmt.Sprint(v), true
}

func (a *App) secretValues(selected []*skills.Skill) []string {
	names := map[string]bool{}
	for _, n := range config.SecretNames(a.Cfg) {
		names[n] = true
	}
	for _, s := range selected {
		for _, sec := range s.Meta.Permissions.SecretList() {
			names[sec.Name] = true
		}
	}
	var out []string
	for n := range names {
		if v, ok := a.Secrets.Get(n); ok {
			out = append(out, v)
		}
	}
	return out
}
