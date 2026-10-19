package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"noskill/internal/agent"
	"noskill/internal/backends"
	"noskill/internal/docs"
	"noskill/internal/eval"
	"noskill/internal/ids"
	"noskill/internal/learning"
	"noskill/internal/platform"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// The checked-task harness (`noskill eval`, FR-B4, LEARNING.md "Evaluation"): each task runs through
// the normal run path on a pinned model, with no trace, memory or outcome signal, and a
// deterministic check decides. Results are kept per run so two runs can be compared.

const (
	evalTaskTimeout = 6 * time.Minute
	evalDirName     = "eval"
)

// EvalDir is where the user's pinned tasks live.
func (a *App) EvalDir() string { return filepath.Join(a.Dirs.Data, evalDirName) }

// EvalTasks returns the built-in tasks and the pinned ones, filtered.
func (a *App) EvalTasks(ids, types []string, quick bool) ([]eval.Task, error) {
	all, err := eval.Builtin()
	if err != nil {
		return nil, err
	}
	if !quick {
		pinned, err := eval.Pinned(a.EvalDir())
		if err != nil {
			return nil, err
		}
		all = append(all, pinned...)
	}
	return eval.Select(all, ids, types, quick), nil
}

// EvalOptions: what to run and on which model.
type EvalOptions struct {
	Model    string
	Tasks    []eval.Task
	Suite    string // a label for the report: all | quick | types…
	Note     string
	Progress func(string)
	// NoLearned keeps learned skills out of agent tasks (the "before" arm of a comparison).
	NoLearned bool
}

// EvalOutcome is a finished run with the previous comparable one (same model and suite).
type EvalOutcome struct {
	Run         store.EvalRun
	Results     []store.EvalResult
	Prev        *store.EvalRun
	PrevResults []store.EvalResult
	Regressions []eval.Regression
}

// Eval runs tasks on one model and records the results.
func (a *App) Eval(ctx context.Context, o EvalOptions) (*EvalOutcome, error) {
	m, err := a.FindModel(ctx, o.Model)
	if err != nil {
		return nil, err
	}
	if !m.chat() {
		return nil, &UserError{m.Name + " is not a chat model"}
	}
	if len(o.Tasks) == 0 {
		return nil, &UserError{"no tasks selected"}
	}
	say := func(f string, args ...any) {
		if o.Progress != nil {
			o.Progress(fmt.Sprintf(f, args...))
		}
	}
	state, _ := json.Marshal(a.skillState())
	run := store.EvalRun{ID: ids.New(), Model: m.Name, Suite: o.Suite, StartedAt: time.Now(), SkillState: string(state), Note: o.Note}
	if err := a.DB.StartEvalRun(ctx, run); err != nil {
		return nil, err
	}
	for i, t := range o.Tasks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		say("[%d/%d] %s", i+1, len(o.Tasks), t.ID)
		r := a.evalTask(ctx, m.Name, t, !o.NoLearned)
		r.RunID = run.ID
		if err := a.DB.AddEvalResult(ctx, r); err != nil {
			return nil, err
		}
	}
	if err := a.DB.FinishEvalRun(ctx, run.ID, time.Now()); err != nil {
		return nil, err
	}
	return a.evalOutcome(ctx, run.ID)
}

// evalOutcome loads a finished run and the one before it for comparison.
func (a *App) evalOutcome(ctx context.Context, id string) (*EvalOutcome, error) {
	runs, err := a.DB.EvalRuns(ctx, "", 200)
	if err != nil {
		return nil, err
	}
	var out EvalOutcome
	for i, r := range runs {
		if r.ID != id {
			continue
		}
		out.Run = r
		for _, p := range runs[i+1:] {
			if p.Model == r.Model && p.Suite == r.Suite {
				p := p
				out.Prev = &p
				break
			}
		}
	}
	if out.Run.ID == "" {
		return nil, &UserError{"no eval run " + id}
	}
	if out.Results, err = a.DB.EvalResults(ctx, id); err != nil {
		return nil, err
	}
	if out.Prev != nil {
		if out.PrevResults, err = a.DB.EvalResults(ctx, out.Prev.ID); err != nil {
			return nil, err
		}
		out.Regressions = eval.Compare(out.PrevResults, out.Results)
		// Many small slips across types can add up without any one type falling far.
		if d := eval.OverallDrop(out.PrevResults, out.Results); a.Cfg.Learning.RegressionSuccessDrop > 0 && d >= a.Cfg.Learning.RegressionSuccessDrop && len(out.Regressions) == 0 {
			out.Regressions = append(out.Regressions, eval.Regression{Type: fmt.Sprintf("overall (%.0f points)", d)})
		}
	}
	return &out, nil
}

// EvalRun returns a stored run (id "last" = the newest) with its comparison.
func (a *App) EvalRun(ctx context.Context, id string) (*EvalOutcome, error) {
	if id == "" || id == "last" {
		runs, err := a.DB.EvalRuns(ctx, "", 1)
		if err != nil || len(runs) == 0 {
			return nil, &UserError{"no eval runs yet: `noskill eval run`"}
		}
		id = runs[0].ID
	}
	return a.evalOutcome(ctx, id)
}

// skillState is what the eval gate compares between runs: learned skills' versions, and
// "tuned:<name>" → the tuned version a built-in uses (absent = its embedded instructions).
func (a *App) skillState() map[string]string {
	out := a.learnedState()
	a.tunedMu.Lock()
	for n, v := range a.tunedIn {
		out[tunedKey+n] = strconv.Itoa(v)
	}
	a.tunedMu.Unlock()
	return out
}

const tunedKey = "tuned:"

// learnedState maps each learned skill to its current version.
func (a *App) learnedState() map[string]string {
	out := map[string]string{}
	for _, s := range a.Skills.All() {
		if s.Source == skills.SourceLearned {
			out[s.Name] = s.Meta.Version
		}
	}
	return out
}

func (a *App) evalTask(ctx context.Context, model string, t eval.Task, learned bool) store.EvalResult {
	res, _ := a.evalTaskWith(ctx, model, t, learned, nil)
	return res
}

// evalTaskWith runs one task. trial, when set, is the version of the task's skill to use in place
// of the loaded one (skill optimisation, ADR-0014); calls are the tool calls made, compressed, for
// the optimiser's reflection.
func (a *App) evalTaskWith(ctx context.Context, model string, t eval.Task, learned bool, trial *skills.Skill) (store.EvalResult, string) {
	var calls strings.Builder
	ncalls := 0
	res := store.EvalResult{TaskID: t.ID, Type: t.Type, Source: t.Source}
	if t.NeedsBox && platform.NewSandbox().Strength().Files == platform.None {
		res.Skipped, res.Why = true, "no OS script sandbox on this platform"
		return res, ""
	}
	ctx, cancel := context.WithTimeout(ctx, evalTaskTimeout)
	defer cancel()

	prompt := t.Prompt
	req := RunRequest{Source: "eval", Model: model, NoLearn: true, ApprovalMode: "deny",
		MaxSteps: 8, Messages: []backends.Message{{Role: backends.RoleUser}}}
	var dir string
	var called, effects []string
	if t.Agent() {
		d, err := os.MkdirTemp("", "noskill-eval-")
		if err != nil {
			res.Why = err.Error()
			return res, ""
		}
		defer os.RemoveAll(d)
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		dir = d
		for name, body := range t.Files {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
				res.Why = err.Error()
				return res, ""
			}
		}
		if len(t.Docs) > 0 {
			ix, err := evalDocs(ctx, t.Docs)
			if err != nil {
				res.Why = err.Error()
				return res, ""
			}
			defer ix.cleanup()
			req.docs = ix.Index
		}
		prompt = strings.ReplaceAll(prompt, "{dir}", filepath.ToSlash(dir))
		req.Skills = t.Skills
		if trial != nil {
			req.Skills, req.trial = nil, trial
		}
		// The task names the skills it needs; a learned skill that matches joins them as it would
		// in normal use, so the suite measures what learning changed (LEARNING.md "Measuring").
		req.plusLearned = learned
		// Everything happens in the throwaway folder, so confirm actions (move_many) go ahead.
		req.ApprovalMode, req.autoConfirm = "", true
		req.Allow = []string{"read:" + dir}
		if t.Writes {
			req.Allow = append(req.Allow, "write:"+dir)
		}
		req.OnEvent = func(e agent.Event) {
			if e.Kind == agent.EventToolCall && !slices.Contains(called, e.Tool) {
				called = append(called, e.Tool)
			}
			if e.Kind == agent.EventFileEffect && !slices.Contains(effects, e.Op) {
				effects = append(effects, e.Op)
			}
			switch e.Kind {
			case agent.EventToolCall:
				ncalls++
				fmt.Fprintf(&calls, "%d. %s(%s)", ncalls, e.Tool, clipText(string(e.Args), 160))
			case agent.EventToolResult:
				if e.Err != "" {
					fmt.Fprintf(&calls, " → error: %s\n", clipText(e.Err, 120))
				} else {
					fmt.Fprintf(&calls, " → %s\n", clipText(e.Content, 120))
				}
			case agent.EventToolDenied:
				fmt.Fprintf(&calls, " → refused\n")
			}
		}
	} else {
		req.NoSkills = true
	}
	req.Messages[0].Content = prompt

	out, err := a.Run(ctx, req)
	if out != nil {
		res.Steps, res.Tokens, res.Ms = out.Steps, out.Usage.Total(), out.Elapsed.Milliseconds()
		for _, n := range out.Skills {
			if s, ok := a.Skills.Get(n); ok && s.Source == skills.SourceLearned {
				res.Learned = append(res.Learned, n)
			}
		}
	}
	switch {
	case err != nil:
		res.Why = err.Error()
	case out == nil:
		res.Why = "no result"
	default:
		res.Passed, res.Why = t.Check.Verdict(out.Answer)
		if res.Passed && !t.ToolsMet(called, a.learnedTools(called), effects) {
			res.Passed, res.Why = false, fmt.Sprintf("didn't call %s (called: %s)", strings.Join(t.Tools, " or "), orDash(called))
		}
		if res.Passed && dir != "" {
			res.Passed, res.Why = t.End.Verdict(dir)
		}
		if !res.Passed && out.Stop != agent.StopAnswered && res.Why != "" {
			res.Why += " (" + string(out.Stop) + ")"
		}
	}
	return res, calls.String()
}

func orDash(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// BenchScores runs the quick micro-eval and stores the model's quality per task type, which the
// router uses as the bandit's prior (FR-B4, FR-O4).
func (a *App) BenchScores(ctx context.Context, name string, progress func(string)) (map[string]float64, error) {
	tasks, err := a.EvalTasks(nil, nil, true)
	if err != nil {
		return nil, err
	}
	out, err := a.Eval(ctx, EvalOptions{Model: name, Tasks: tasks, Suite: "quick", Note: "models bench", Progress: progress})
	if err != nil {
		return nil, err
	}
	m, err := a.FindModel(ctx, name)
	if err != nil {
		return nil, err
	}
	scores := eval.Scores(out.Results)
	if err := a.DB.PutScores(ctx, m.ID, scores, time.Now()); err != nil {
		return scores, err
	}
	a.invalidateModels()
	return scores, nil
}

// PinEval saves a finished run as a task the user's setup must keep passing. The check is that the
// answer is not empty plus any text the answer must contain.
func (a *App) PinEval(ctx context.Context, runID, id string, contains []string) (eval.Task, error) {
	var run store.Run
	var err error
	if runID == "" || runID == "last" {
		run, err = a.DB.LatestRun(ctx, "")
	} else {
		run, err = a.DB.GetRun(ctx, runID)
	}
	if err != nil {
		return eval.Task{}, &UserError{"no such run"}
	}
	if run.TracePath == "" {
		return eval.Task{}, &UserError{"that run kept no trace (it ran with --no-learn)"}
	}
	tr, err := learning.ReadTrace(run.TracePath)
	if err != nil {
		return eval.Task{}, err
	}
	if tr.Request == "" {
		return eval.Task{}, &UserError{"the trace has no request"}
	}
	if id == "" {
		id = "pin-" + strings.ToLower(run.ID[len(run.ID)-6:])
	}
	t := eval.Task{ID: id, Type: orStr(run.TaskType, "chat"), Prompt: tr.Request, Skills: tr.Skills,
		Check: eval.Check{Answered: true, Contains: contains}}
	seen := map[string]bool{}
	for _, c := range tr.Calls() {
		if !seen[c.Tool] {
			seen[c.Tool] = true
			t.Tools = append(t.Tools, c.Tool)
		}
	}
	sort.Strings(t.Tools)
	if err := eval.SavePinned(a.EvalDir(), t); err != nil {
		return t, &UserError{err.Error()}
	}
	return t, nil
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// EvalGate is the regression gate (LEARNING.md "Evaluation"): after a learned skill changed, run
// the suite again and compare with the last run of the same model. When results fell, each learned
// skill that changed version since that run and took part in a task that went from pass to fail
// goes back to the version of that run. A fall in tasks no learned skill touched (plain questions,
// say) is the model's noise, not learning's, and rolls nothing back.
type GateResult struct {
	Outcome    *EvalOutcome
	RolledBack map[string]string // skill → version it went back to
	// Kept are learned skills that are new since the last run and took part in a task that fell:
	// there's no older version to go back to, so they're reported.
	Kept []string
}

func (a *App) EvalGate(ctx context.Context, o EvalOptions, rollback bool) (*GateResult, error) {
	out, err := a.Eval(ctx, o)
	if err != nil {
		return nil, err
	}
	g := &GateResult{Outcome: out}
	if len(out.Regressions) == 0 || out.Prev == nil || !rollback {
		return g, nil
	}
	var then map[string]string
	_ = json.Unmarshal([]byte(out.Prev.SkillState), &then)
	var now map[string]string
	_ = json.Unmarshal([]byte(out.Run.SkillState), &now)
	g.RolledBack = map[string]string{}
	for _, name := range suspects(out.PrevResults, out.Results) {
		v, ok := now[name]
		if !ok {
			continue // no longer a learned skill (archived or removed since)
		}
		old, had := then[name]
		switch {
		case had && old == v:
			continue
		case !had:
			// A skill added since the good run can't be rolled back to nothing; it stays and the
			// report says so. The curator or the user archives it.
			g.Kept = append(g.Kept, name)
			continue
		}
		got, err := a.RollbackSkill(ctx, name, old)
		if err != nil {
			return g, fmt.Errorf("rolling %s back to %s: %w", name, old, err)
		}
		g.RolledBack[name] = got
	}
	// Tuned built-ins (ADR-0014): a task naming the skill fell after its tuned version changed, so
	// the skill goes back to what it used in the good run.
	tasks, _ := a.EvalTasks(nil, nil, false)
	skillsOf := map[string][]string{}
	for _, t := range append(tasks, o.Tasks...) {
		skillsOf[t.ID] = t.Skills
	}
	for _, name := range tunedSuspects(out.PrevResults, out.Results, skillsOf) {
		cur, old := now[tunedKey+name], then[tunedKey+name]
		if cur == old {
			continue
		}
		to, _ := strconv.Atoi(old)
		if err := a.OptimiseRevert(ctx, name, to, "the weekly eval gate: a task that names it fell after it changed"); err != nil {
			return g, fmt.Errorf("putting %s back: %w", name, err)
		}
		if to > 0 {
			g.RolledBack[name] = "tuned " + old
		} else {
			g.RolledBack[name] = "built-in instructions"
		}
	}
	return g, nil
}

// tunedSuspects are the skills named by tasks that passed before and fail now.
func tunedSuspects(prev, cur []store.EvalResult, skillsOf map[string][]string) []string {
	before := map[string]bool{}
	for _, r := range prev {
		before[r.TaskID] = r.Passed
	}
	var out []string
	for _, r := range cur {
		if !before[r.TaskID] || r.Passed || r.Skipped {
			continue
		}
		for _, n := range skillsOf[r.TaskID] {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// suspects are the learned skills used, before or now, by tasks that passed before and fail now.
func suspects(prev, cur []store.EvalResult) []string {
	before := map[string]store.EvalResult{}
	for _, r := range prev {
		before[r.TaskID] = r
	}
	var out []string
	for _, r := range cur {
		b, ok := before[r.TaskID]
		if !ok || !b.Passed || r.Passed || r.Skipped {
			continue
		}
		for _, n := range append(slices.Clone(b.Learned), r.Learned...) {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// clipText shortens s to n bytes on one line.
func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// evalDocsIndex is a task's own documents: a temporary folder, added to a temporary index (keyword
// search only, so a score doesn't depend on which embedding model is installed).
type evalDocsIndex struct {
	*docs.Index
	cleanup func()
}

func evalDocs(ctx context.Context, files map[string]string) (*evalDocsIndex, error) {
	root, err := os.MkdirTemp("", "noskill-eval-docs-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*evalDocsIndex, error) { os.RemoveAll(root); return nil, err }
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	folder := filepath.Join(root, "docs")
	for name, body := range files {
		p := filepath.Join(folder, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			return fail(err)
		}
	}
	ix, err := docs.Open(ctx, filepath.Join(root, "docs.db"))
	if err != nil {
		return fail(err)
	}
	if err := ix.AddFolder(ctx, folder); err != nil {
		ix.Close()
		return fail(err)
	}
	if _, err := ix.IndexFolder(ctx, folder); err != nil {
		ix.Close()
		return fail(err)
	}
	return &evalDocsIndex{Index: ix, cleanup: func() { ix.Close(); os.RemoveAll(root) }}, nil
}
