package app

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"
	"unicode"

	"noskill/internal/approvals"
	"noskill/internal/learning"

	"noskill/internal/backends"
	"noskill/internal/decide"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// The decision judge (LEARNING.md §1, tier 4): after the reply is out, the resident decision model
// says whether it answered the request and how well. It only runs when a real decision model is
// installed; emulating it costs seconds on the chat model, so without one the LLM judge does this
// later, at idle (learning worker).

var judgeQuestions = map[string]decide.Question{
	"answered": {Type: decide.Noul, Instructions: "Does the reply actually do what the user asked (not a refusal, an error or a question back)?"},
	"quality": {Type: decide.Score, Instructions: "How good is the reply for the user's request, given the tool results?",
		Scale: []string{"Wrong or useless", "Poor", "Acceptable", "Good", "Excellent"}},
}

var followedQuestion = decide.Question{Type: decide.Noul, Instructions: "Did the run follow the selected skill's instructions?"}

// judgeAfter starts the judge in the background. App.Close waits for it (briefly).
func (a *App) judgeAfter(row store.Run, request, answer string, actions string, selected []*skills.Skill, models []Model) {
	d := a.judgeDecider(models)
	if d == nil || strings.TrimSpace(answer) == "" {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a.judge(ctx, d, row.ID, request, answer, actions, selected)
	}()
}

func (a *App) judgeDecider(models []Model) decide.Decider {
	if a.judgeOverride != nil {
		return a.judgeOverride
	}
	want := a.Cfg.Orchestrator.DecisionModel
	if want == "" {
		return nil
	}
	for _, m := range models {
		if m.Name == want && m.Has(backends.CapDecide) && m.NB.Kind == "ollama" {
			return decide.JEV{BaseURL: m.NB.URL, Model: m.Name}
		}
	}
	return nil
}

func (a *App) judge(ctx context.Context, d decide.Decider, runID, request, answer, actions string, selected []*skills.Skill) {
	ans, err := judgeAsk(ctx, d, request, answer, actions, selected)
	if err != nil {
		return
	}
	outcome, conf := a.judgeRecorded(ctx, runID, ans)
	a.recordJudged(ctx, runID, outcome, conf, "decision", "decision judge ("+ans.Model+")")
}

func judgeAsk(ctx context.Context, d decide.Decider, request, answer, actions string, selected []*skills.Skill) (decide.Answers, error) {
	state := map[string]any{"request": clipRunes(request, 1500), "reply": clipRunes(answer, 2000)}
	if actions != "" {
		state["tool_calls"] = clipRunes(actions, 1500)
	}
	qs := map[string]decide.Question{}
	for k, v := range judgeQuestions {
		qs[k] = v
	}
	if len(selected) > 0 {
		var names []string
		for _, s := range selected {
			names = append(names, s.Name+": "+s.Description)
		}
		state["skills"] = strings.Join(names, "\n")
		qs["followed_skill"] = followedQuestion
	}
	return d.Decide(ctx, state, qs)
}

// recordJudged sets a judge's verdict as the run's outcome, unless a check, an implicit signal or
// the user's own rating already know better.
func (a *App) recordJudged(ctx context.Context, runID, outcome string, conf float64, source, note string) bool {
	if run, err := a.DB.GetRun(ctx, runID); err != nil || (run.OutcomeSource != "" && run.OutcomeSource != "decision") {
		return false
	}
	if a.DB.SetOutcome(ctx, runID, outcome, conf, source) != nil {
		return false
	}
	a.traceOutcome(ctx, runID, outcome, conf, source, note)
	return true
}

// judgeRecorded reaches the verdict with the calibrated "answered" threshold for the judging model
// and stores that decision for calibration (calibrate.go).
func (a *App) judgeRecorded(ctx context.Context, runID string, ans decide.Answers) (string, float64) {
	th := a.decisionThreshold(ctx, DecisionJudge, ans.Model)
	p := ans.Answers["answered"].Noul
	answer := "no"
	if p >= th {
		answer = "yes"
	}
	a.recordDecision(ctx, store.Decision{RunID: runID, Name: DecisionJudge, Model: ans.Model, Emulated: ans.Emulated,
		Answer: answer, P: p, Threshold: th})
	return judgeVerdictAt(ans, th)
}

// judgeVerdict maps the answers to an outcome at the default threshold.
func judgeVerdict(ans decide.Answers) (string, float64) {
	return judgeVerdictAt(ans, decisionDefaults[DecisionJudge])
}

// judgeVerdictAt maps the answers to an outcome; the reply counts as answered when P(yes) ≥
// answeredMin. The confidence is the probability mass on that side of the quality scale when the
// model gives probabilities, else a conservative 0.6.
func judgeVerdictAt(ans decide.Answers, answeredMin float64) (string, float64) {
	q := ans.Answers["quality"]
	answered := ans.Answers["answered"].Noul
	scale := judgeQuestions["quality"].Scale
	var good, bad float64
	for i, label := range scale {
		switch p := q.Probabilities[label]; {
		case i >= 2:
			good += p
		default:
			bad += p
		}
	}
	conf := func(p float64) float64 {
		if p <= 0 {
			return 0.6
		}
		return p
	}
	switch {
	case answered < answeredMin || q.Score < 1.5:
		return "fail", conf(bad)
	case q.Score >= 2.5:
		return "success", conf(good)
	default:
		return "partial", conf(good)
	}
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// The LLM judge (LEARNING.md §1, tier 5): at idle, a chat model, from another family than the one
// that ran the task when one is installed, judges recent runs that have no outcome or only an
// unsure decision judge's. Emulated decisions give no probabilities, so its verdict alone is worth
// 0.6: enough for routing, not for synthesis. When both judges agree the confidence rises to at
// least 0.75; when they disagree, you're asked.
const (
	judgeUnsure     = learning.MinConfidence
	judgeWait       = 10 * time.Minute // give you time to rate it yourself first
	judgeWithin     = 3 * 24 * time.Hour
	judgePerPass    = 3
	judgeAgreeConf  = 0.75
	checkKind       = "outcome_check"
	checksPerDay    = 3
	checkRandomOdds = 50 // about 1 in 50 judged runs is offered for your rating anyway
	checkTimeout    = 7 * 24 * time.Hour
)

// llmJudge judges up to judgePerPass runs and returns how many got an outcome.
func (a *App) llmJudge(ctx context.Context) (int, error) {
	now := time.Now()
	runs, err := a.DB.UnjudgedRuns(ctx, now.Add(-judgeWait), now.Add(-judgeWithin), judgeUnsure, judgePerPass+len(a.judgeTried))
	if err != nil || len(runs) == 0 {
		return 0, err
	}
	models, _ := a.Models(ctx)
	release, ctx, err := a.acquire(ctx, 0, true)
	if err != nil {
		return 0, err
	}
	defer release()
	n, tries := 0, 0
	for _, run := range runs {
		if a.judgeTried[run.ID] || tries == judgePerPass || ctx.Err() != nil {
			continue
		}
		m := a.judgeModel(models, run.DriverModel)
		if m == nil {
			return n, nil
		}
		tries++
		tr, err := learning.ReadTrace(run.TracePath)
		if err != nil || strings.TrimSpace(tr.Answer) == "" {
			a.judgeTried[run.ID] = true
			continue
		}
		var selected []*skills.Skill
		for _, name := range tr.Skills {
			if sk, ok := a.Skills.Get(name); ok {
				selected = append(selected, sk)
			}
		}
		d := decide.Emulated{Backend: m.NB.B, Model: m.Name, NumCtx: a.numCtx(*m)}
		ans, err := judgeAsk(ctx, d, tr.Request, tr.Answer, tr.Compress(300), selected)
		if err != nil {
			if ctx.Err() == nil {
				a.judgeTried[run.ID] = true
			}
			continue
		}
		outcome, conf := a.judgeRecorded(ctx, run.ID, ans)
		if ans.Emulated {
			conf = 0.6 // one label, no real probabilities
		}
		agree := run.OutcomeSource == "decision" && run.Outcome == outcome
		if agree {
			conf = max(conf, run.OutcomeConf, judgeAgreeConf)
		}
		if !a.recordJudged(ctx, run.ID, outcome, conf, "llm_judge", "LLM judge ("+m.Name+")") {
			continue
		}
		n++
		if outcome == "success" && conf >= learning.MinConfidence {
			// The miner looked at this run before it had an outcome.
			if r, err := a.DB.GetRun(ctx, run.ID); err == nil {
				a.MineRun(ctx, r)
			}
		}
		switch {
		case run.OutcomeSource == "decision" && !agree:
			a.askOutcome(ctx, run, tr, fmt.Sprintf("the judges disagree: decision model %s, %s %s", run.Outcome, m.Name, outcome))
		case rand.IntN(checkRandomOdds) == 0:
			a.askOutcome(ctx, run, tr, "a random check of the judges")
		}
	}
	return n, nil
}

// judgeModel picks the strongest chat model that fits the memory budget, from another family than
// the run's model when there is one.
func (a *App) judgeModel(models []Model, driver string) *Model {
	budget, _, _ := a.liveBudget(models) // idle-time work: free memory may hold a bigger judge
	var best, other *Model
	better := func(m, cur *Model) bool {
		return cur == nil || m.ParamsB > cur.ParamsB || (m.ParamsB == cur.ParamsB && m.Loaded && !cur.Loaded)
	}
	for i := range models {
		m := &models[i]
		if !m.chat() || m.Has(backends.CapDecide) || (budget > 0 && m.SizeBytes > budget) {
			continue
		}
		if better(m, best) {
			best = m
		}
		if family(m.Name) != family(driver) && better(m, other) {
			other = m
		}
	}
	if other != nil {
		return other
	}
	return best
}

// family is a model name's family: "qwen" for qwen3:4b and qwen2.5-coder:7b, "llama" for llama3.2.
func family(name string) string {
	name = strings.ToLower(name[strings.LastIndex(name, "/")+1:])
	if i := strings.IndexFunc(name, func(r rune) bool { return !unicode.IsLetter(r) }); i > 0 {
		return name[:i]
	}
	return name
}

// askOutcome queues a quick "was this right?" question (never blocking, at most checksPerDay).
func (a *App) askOutcome(ctx context.Context, run store.Run, tr *learning.Trace, why string) {
	items, err := a.Approvals.List(ctx, true, 200)
	if err != nil {
		return
	}
	day := time.Now().Add(-24 * time.Hour)
	asked := 0
	for _, it := range items {
		if it.Kind == checkKind && it.CreatedAt.After(day) {
			asked++
		}
	}
	if asked >= checksPerDay {
		return
	}
	a.Approvals.Create(ctx, approvals.New{Source: "learning", Kind: checkKind, RunID: run.ID, Timeout: checkTimeout,
		Summary: "Was this answer right? " + clipRunes(tr.Request, 120),
		Details: map[string]any{"request": clipRunes(tr.Request, 400), "answer": clipRunes(tr.Answer, 600), "why": why}})
}

// applyOutcomeChecks turns answered "was this right?" questions into ratings: approve means it was
// right, skip means it wasn't; an expired question changes nothing.
func (a *App) applyOutcomeChecks(ctx context.Context) {
	items, err := a.Approvals.List(ctx, true, 200)
	if err != nil {
		return
	}
	for _, it := range items {
		if it.Kind != checkKind || it.RunID == "" || (it.Status != approvals.Approved && it.Status != approvals.Skipped) || strings.Contains(string(it.Details), `"used"`) {
			continue
		}
		a.Approvals.MarkUsed(ctx, it.ID)
		rating := "good"
		if it.Status == approvals.Skipped {
			rating = "bad"
		}
		if _, err := a.Rate(ctx, it.RunID, rating, "answered a check in the approvals queue"); err != nil {
			slog.Warn("couldn't apply an outcome check", "run", it.RunID, "err", err)
		}
	}
}
