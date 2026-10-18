package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"noskill/internal/agent"
	"noskill/internal/backends"
	"noskill/internal/learning"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// Drafting and verification (LEARNING.md §3–4, FR-L2, FR-L3): a queued run becomes a draft in
// learned/.drafts, is checked and replayed, and is published as a candidate or set aside in
// learned/.rejected with the reason.

const (
	maxRepairs  = 2 // self-repair attempts after failed tests
	maxRewrites = 2 // description rewrites after a failed retrieval check
)

func (a *App) draftFromQueue(ctx context.Context, it store.LearnItem) (string, error) {
	if len(it.RunIDs) == 0 {
		return "", fmt.Errorf("%w: no run", errSkip)
	}
	run, err := a.DB.GetRun(ctx, it.RunIDs[0])
	if err != nil {
		return "", fmt.Errorf("%w: run %s is gone", errSkip, it.RunIDs[0])
	}
	tr, err := learning.ReadTrace(run.TracePath)
	if err != nil {
		return "", fmt.Errorf("%w: its trace can't be read (%v)", errSkip, err)
	}
	models, _ := a.Models(ctx)
	m, err := a.authorModel(models, tr.Model)
	if err != nil {
		return "", err
	}
	au := a.author(ctx, models, m)
	// A learned skill that does this task already (LEARNING.md §7): skip, unless it's plain steps
	// and pass d could make it one script call; then the new draft replaces it.
	fam := a.family(tr, traceTools(tr))
	if fam != nil && (len(fam.Tools) > 0 || au.Script == nil || a.upgrades(ctx, fam.Name) >= maxUpgrades) {
		return "", fmt.Errorf("%w: covered by %s", errSkip, fam.Name)
	}
	d, err := au.Draft(ctx, tr)
	if err != nil {
		return "", err
	}
	if fam != nil {
		if d.Tool == nil {
			return "", fmt.Errorf("%w: covered by %s, and no script tool came of it (%s)", errSkip, fam.Name, strings.Join(d.Notes, "; "))
		}
		a.DB.SetState(ctx, upgradeKey(fam.Name), fmt.Sprint(a.upgrades(ctx, fam.Name)+1), time.Now())
		if fam.Status() == "active" {
			if a.nextSkill(fam.Name) != nil {
				return "", fmt.Errorf("%w: covered by %s, which has a refinement on trial", errSkip, fam.Name)
			}
			d.Name = fam.Name // published as its refinement, on trial in its place
		}
	}
	lib := a.Learned()
	dir, err := lib.NewDraftDir(d.Name)
	if err != nil {
		return "", err
	}
	rep, err := a.verifyDraft(ctx, d, dir, tr, au, m)
	if err != nil {
		if ctx.Err() == nil {
			lib.Reject(dir, "verification stopped: "+err.Error())
		}
		return "", err
	}
	if !rep.ok() {
		to, _ := lib.Reject(dir, rep.text())
		return "rejected " + d.Name + " (" + rep.short() + "); kept in " + to, nil
	}
	if fam != nil && d.Tool == nil { // verification dropped the script tool: plain steps add nothing to fam
		lib.Reject(dir, "its script tool failed replay, and "+fam.Name+" already does the task as steps\n"+strings.Join(d.Notes, "\n"))
		return "", fmt.Errorf("%w: covered by %s, and its script tool failed replay (%s)", errSkip, fam.Name, strings.Join(d.Notes, "; "))
	}
	note := fmt.Sprintf("Learned from run %s (%s). %s", tr.RunID, it.Reason, rep.text())
	if fam != nil {
		note = fmt.Sprintf("Redrafted from run %s with script tool %s, so the task takes one call. %s", tr.RunID, d.Tool.Name, rep.text())
	}
	v, err := lib.Publish(dir, note)
	if err != nil {
		return "", err
	}
	if fam != nil && d.Name == fam.Name {
		a.recordVersion(ctx, d.Name, v, fam.Meta.Version, note, "learning")
		return fmt.Sprintf("refined %s %s with script tool %s (on trial in place of %s)", d.Name, v, d.Tool.Name, fam.Meta.Version), nil
	}
	if fam != nil {
		a.setLearnedStatus(ctx, fam.Name, "archived", fmt.Sprintf("Replaced by %s, which does the task in one call. `noskill skills enable %s` brings it back.", d.Name, fam.Name))
	}
	if err := a.loadSkills(ctx); err != nil {
		return "", err
	}
	a.recordVersion(ctx, d.Name, v, "", note, "learning")
	return fmt.Sprintf("learned %s %s (candidate; it asks before its first use)", d.Name, v), nil
}

// authorModel is [learning] author_model, or with "auto" the model that ran the source request
// (it got the task done), else the loaded chat model, else the largest one.
func (a *App) authorModel(models []Model, traceModel string) (Model, error) {
	want := a.Cfg.Learning.AuthorModel
	var best *Model
	for i := range models {
		m := &models[i]
		if !m.chat() {
			continue
		}
		if want != "" && want != "auto" {
			if m.Name == want {
				return *m, nil
			}
			continue
		}
		if m.Name == traceModel {
			return *m, nil
		}
		if best == nil || (m.Loaded && !best.Loaded) || (m.Loaded == best.Loaded && m.ParamsB > best.ParamsB) {
			best = m
		}
	}
	if best == nil {
		if want != "" && want != "auto" {
			return Model{}, fmt.Errorf("learning.author_model %s isn't installed", want)
		}
		return Model{}, fmt.Errorf("no chat model is installed")
	}
	return *best, nil
}

func (a *App) author(ctx context.Context, models []Model, m Model) *learning.Author {
	home, _ := os.UserHomeDir()
	return &learning.Author{
		LLM:     a.authorLLM(m),
		Decide:  a.decider(ctx, models),
		Home:    home,
		Script:  a.scriptRun(),
		Confirm: a.confirmsTool,
		Taken: func(n string) bool {
			_, ok := a.Skills.Get(n)
			return ok || a.Learned().Exists(n)
		},
		SkillOf: func(tool string) string {
			for _, s := range a.Skills.All() {
				for _, t := range s.Tools {
					if t.Name == tool {
						return s.Name
					}
				}
			}
			return ""
		},
	}
}

// authorLLM is one constrained call on the author model. Thinking stays off: the passes are small
// and the schema does the structuring. A nil schema asks for plain text, returned as is.
func (a *App) authorLLM(m Model) learning.LLM {
	return func(ctx context.Context, prompt string, schema json.RawMessage, maxTokens int) (string, error) {
		no := false
		temp := 0.2
		if m.Has(backends.CapThinkAlways) {
			maxTokens *= 3 // its reasoning counts against num_predict
		}
		req := backends.ChatRequest{Model: m.Name, Messages: []backends.Message{{Role: backends.RoleUser, Content: prompt}},
			Think: &no, Options: backends.Options{NumCtx: a.numCtx(m), NumPredict: maxTokens, Temperature: &temp}}
		if m.Has(backends.CapSchema) && schema != nil {
			req.Format = schema
		}
		s, err := m.NB.B.Chat(ctx, req)
		if err != nil {
			return "", err
		}
		resp, err := backends.Collect(s, nil)
		if err != nil {
			return "", err
		}
		out := strings.TrimSpace(resp.Content)
		if schema == nil {
			return out, nil // a plain-text pass (pass d's code)
		}
		// Without schema support, take the outermost JSON object from the reply.
		if i, j := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && j > i {
			out = out[i : j+1]
		}
		if !json.Valid([]byte(out)) {
			return "", fmt.Errorf("%s didn't reply with JSON", m.Name)
		}
		return out, nil
	}
}

// ---- verification ----

type verifyReport struct {
	check       learning.Check
	results     []learning.CaseResult
	retrieval   string // "" = passed
	repairs     int
	rewrites    int
	notes       []string
	passedTests bool
}

func (r *verifyReport) ok() bool {
	return r.check.OK() && r.passedTests && r.retrieval == ""
}

func (r *verifyReport) short() string {
	switch {
	case len(r.check.Escalations) > 0:
		return "wants more access than the run used"
	case len(r.check.Problems) > 0:
		return r.check.Problems[0]
	case !r.passedTests:
		return "its tests failed"
	case r.retrieval != "":
		return r.retrieval
	}
	return "passed"
}

func (r *verifyReport) text() string {
	var b strings.Builder
	if len(r.check.Escalations) > 0 {
		b.WriteString("Needs your approval for: " + strings.Join(r.check.Escalations, ", ") + ".\n")
	}
	for _, p := range r.check.Problems {
		b.WriteString("Problem: " + p + ".\n")
	}
	pass := 0
	for _, c := range r.results {
		if c.Passed {
			pass++
		}
	}
	if len(r.results) > 0 {
		fmt.Fprintf(&b, "Tests: %d of %d passed", pass, len(r.results))
		if r.repairs > 0 {
			fmt.Fprintf(&b, " after %d repair(s)", r.repairs)
		}
		b.WriteString(".\n")
		for _, c := range r.results {
			if !c.Passed {
				fmt.Fprintf(&b, "- %s: %s\n", c.Case, c.Why)
			}
		}
	}
	if r.retrieval != "" {
		b.WriteString("Retrieval: " + r.retrieval + ".\n")
	}
	for _, n := range r.notes {
		b.WriteString("Note: " + n + ".\n")
	}
	return strings.TrimSpace(b.String())
}

// verifyDraft writes the draft to dir and runs the gate on it, repairing the steps or the
// description when the model got them wrong.
func (a *App) verifyDraft(ctx context.Context, d *learning.Draft, dir string, tr *learning.Trace, au *learning.Author, m Model) (*verifyReport, error) {
	rep := &verifyReport{notes: d.Notes}
	for {
		sk, files, issues, err := writeDraft(d, dir)
		if err != nil {
			return nil, err
		}
		rep.check = learning.StaticCheck(sk, issues, files, tr, au.Home, a.secretValuesKnown())
		if !rep.check.OK() {
			return rep, nil // escalations need a human; the rest are bugs in the draft, not the model's steps
		}
		rep.results, err = a.replayCases(ctx, sk, dir, m, au.Home)
		if err != nil {
			return nil, err
		}
		rep.passedTests = learning.Passed(rep.results)
		if !rep.passedTests {
			if rep.repairs >= maxRepairs {
				return rep, nil
			}
			rep.repairs++
			if err := au.Repair(ctx, d, tr, rep.results); err != nil {
				return nil, err
			}
			continue
		}
		rep.retrieval, err = a.retrievalCheck(ctx, sk, tr)
		if err != nil {
			return nil, err
		}
		if rep.retrieval != "" && rep.rewrites < maxRewrites {
			rep.rewrites++
			if err := au.Rewrite(ctx, d, tr, rep.retrieval); err != nil {
				return nil, err
			}
			continue
		}
		return rep, nil
	}
}

// writeDraft writes the draft's files into dir and loads it.
func writeDraft(d *learning.Draft, dir string) (*skills.Skill, map[string][]byte, []skills.Issue, error) {
	files, err := d.Files()
	if err != nil {
		return nil, nil, nil, err
	}
	for name, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, nil, nil, err
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return nil, nil, nil, err
		}
	}
	sk, issues := skills.Load(os.DirFS(dir), filepath.Base(dir), dir, skills.SourceLearned)
	return sk, files, issues, nil
}

// replayCases runs each test case through the normal run path on model m, with recorded results
// in place of the real tools.
func (a *App) replayCases(ctx context.Context, sk *skills.Skill, dir string, m Model, home string) ([]learning.CaseResult, error) {
	cases, fx, err := learning.ReadCases(func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(n))) })
	if err != nil {
		return nil, err
	}
	var out []learning.CaseResult
	for _, c := range cases {
		rp := &learning.Replay{Fixtures: fx[c.Fixtures]}
		res, err := a.Run(ctx, RunRequest{
			Messages: []backends.Message{{Role: backends.RoleUser, Content: c.Input}},
			Source:   "learning", NoLearn: true, Headless: true, ApprovalMode: "deny", Model: m.Name,
			MaxSteps: 12, trial: sk,
			tools: func(inner agent.Tools) agent.Tools { rp.Inner = inner; return rp },
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			out = append(out, learning.CaseResult{Case: c.Name, Required: c.Required, Why: "the run failed: " + err.Error()})
			continue
		}
		out = append(out, learning.Judge(c, rp.Calls, res.Stop == agent.StopAnswered && strings.TrimSpace(res.Answer) != "", home))
	}
	return out, nil
}

// retrievalCheck: the source request must find the skill among the top 3, and unrelated past
// requests must not find it at all (LEARNING.md §3). It returns what went wrong, or "".
func (a *App) retrievalCheck(ctx context.Context, sk *skills.Skill, tr *learning.Trace) (string, error) {
	usable, err := a.UsableSkills(ctx)
	if err != nil {
		return "", err
	}
	cands := append(slices.Clone(usable), sk)
	sel := &skills.Selector{Cache: a.DB}
	if models, err := a.Models(ctx); err == nil {
		if em := a.embedModel(models); em != nil {
			sel.Embedder, sel.EmbedModel = em.NB.B, em.Name
		}
	}
	found := func(q string) (bool, error) {
		ms, err := sel.Select(ctx, cands, q, 3)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(ms, func(m skills.Match) bool { return m.Skill == sk }), nil
	}
	if ok, err := found(tr.Request); err != nil || !ok {
		return "the request it came from doesn't find it", err
	}
	for _, q := range a.unrelatedRequests(ctx, tr, 5) {
		if ok, err := found(q); err != nil || ok {
			return fmt.Sprintf("an unrelated request finds it too (%q)", clipRunes(q, 80)), err
		}
	}
	return "", nil
}

// unrelatedRequests are recent requests whose runs used other tools: at most half of the tools
// either run used are shared. Comparing the exact pattern called the same chore done in another
// order (make_dir>list_dir>move_many vs list_dir>make_dir>move_many) unrelated, and rejected
// good skills for it (block 2c).
func (a *App) unrelatedRequests(ctx context.Context, tr *learning.Trace, n int) []string {
	runs, err := a.DB.ListRuns(ctx, 60)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range runs {
		if len(out) == n {
			break
		}
		if r.ID == tr.RunID || r.TracePath == "" || r.ToolCalls == 0 || similarTools(r.ToolPattern, tr.Pattern()) {
			continue
		}
		if t, err := learning.ReadTrace(r.TracePath); err == nil && t.Request != "" && !slices.Contains(out, t.Request) {
			out = append(out, t.Request)
		}
	}
	return out
}

// secretValuesKnown are the values of every stored secret, for the secret scan.
func (a *App) secretValuesKnown() []string {
	names, _ := a.Secrets.List()
	var out []string
	for _, n := range names {
		if v, ok := a.Secrets.Get(n); ok {
			out = append(out, v)
		}
	}
	return out
}

// similarTools: the distinct tools of two patterns ("a>b>c") overlap by more than half (Jaccard).
func similarTools(p, q string) bool {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, t := range strings.Split(s, ">") {
			if t != "" {
				m[t] = true
			}
		}
		return m
	}
	a, b := set(p), set(q)
	both := 0
	for t := range a {
		if b[t] {
			both++
		}
	}
	union := len(a) + len(b) - both
	return union > 0 && both*2 > union
}
