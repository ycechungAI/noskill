package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/eval"
	"noskill/internal/learning"
	"noskill/internal/skills"
	skillfiles "noskill/skills"
)

// Skill optimisation (FR-L9, ADR-0014): `noskill skills optimise <name>` rewrites a built-in or
// learned skill's instructions with the author model, scores each rewrite on the checked tasks that
// name the skill, and keeps one only if it is clearly better there and no worse on held-out tasks.
// A built-in's rewrite waits in <data>/tuned for `adopt`; a learned skill's goes on trial in .next.

// OptimiseOptions: which skill, how hard to look, on which model.
type OptimiseOptions struct {
	Skill    string
	Budget   int    // rewrites to try (default 4)
	Model    string // the model the tasks run on ("" = the smallest chat model, as `eval run`)
	MaxTasks int    // 0 = all; else at most this many training tasks, spread evenly, and a third as many held-out ones
	DryRun   bool   // search and report; the rewrite isn't saved as a skill version (the report file still is)
	Adopt    bool   // a built-in's accepted rewrite goes straight into use, not to wait for `optimise adopt`
	Force    bool   // run on battery too
	Progress func(string)
	tasks    func() (train, held []eval.Task) // tests: in place of OptimiseTasks
}

// OptimiseOutcome is what a search did and where its result went.
type OptimiseOutcome struct {
	Skill   string                   `json:"skill"`
	Source  string                   `json:"source"`
	Model   string                   `json:"model"`
	Author  string                   `json:"author_model"`
	Train   []string                 `json:"train"`
	Held    []string                 `json:"held"`
	Result  *learning.OptimiseResult `json:"result"`
	Saved   string                   `json:"saved,omitempty"`  // "tuned version 2 (candidate)", "1.3.0 on trial"
	Report  string                   `json:"report,omitempty"` // the file holding every rewrite and score, written for every search
	Problem string                   `json:"problem,omitempty"`
}

// optimisable returns the skill and checks it may be optimised.
func (a *App) optimisable(name string) (*skills.Skill, error) {
	sk, ok := a.Skills.Get(name)
	if !ok {
		return nil, &UserError{fmt.Sprintf("no skill named %q", name)}
	}
	switch sk.Source {
	case skills.SourceBuiltin, skills.SourceLearned:
		return sk, nil
	}
	return nil, &UserError{fmt.Sprintf("%s is a %s skill: only built-in and learned skills are optimised (yours are never rewritten)", name, sk.Source)}
}

// OptimiseTasks splits the checked tasks that name the skill into training and held-out ones
// (ADR-0014): the multi-step tasks and your pinned tasks are held out.
func (a *App) OptimiseTasks(name string) (train, held []eval.Task, err error) {
	all, err := a.EvalTasks(nil, nil, false)
	if err != nil {
		return nil, nil, err
	}
	work, err := eval.Workload()
	if err != nil {
		return nil, nil, err
	}
	for _, t := range work {
		t.Source = "workload"
		all = append(all, t)
	}
	for _, t := range all {
		// Only tasks that run this one skill: a candidate stands in for it alone.
		if !t.Agent() || len(t.Skills) != 1 || t.Skills[0] != name {
			continue
		}
		if t.Source == "pinned" || (t.Source != "workload" && t.Family != "") {
			held = append(held, t)
		} else {
			train = append(train, t)
		}
	}
	return train, held, nil
}

// writeOptimiseReport keeps every search's rewrites and scores in <data>/optimise, dry runs
// included, so a good result is never only in a process's memory. It returns the path, or "" if
// the file couldn't be written (the search itself is not failed for that).
func (a *App) writeOptimiseReport(out *OptimiseOutcome) string {
	dir := filepath.Join(a.Dirs.Data, "optimise")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%s.json", out.Skill, time.Now().Format("20060102-150405")))
	if err := writeJSON(p, out); err != nil {
		return ""
	}
	return p
}

// sampleTasks keeps n tasks spread evenly through the list (the same ones every time), so a quick
// run still covers the whole mix of tasks.
func sampleTasks(ts []eval.Task, n int) []eval.Task {
	if n >= len(ts) {
		return ts
	}
	out := make([]eval.Task, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ts[i*len(ts)/n])
	}
	return out
}

// embeddedSkillMD is the SKILL.md shipped in the binary.
func embeddedSkillMD(name string) ([]byte, error) {
	return fs.ReadFile(skillfiles.Builtin(), path.Join(name, "SKILL.md"))
}

// skillMD is the SKILL.md a skill is loaded from now (a tuned version, if one is in use).
func (a *App) skillMD(sk *skills.Skill) ([]byte, error) {
	return fs.ReadFile(sk.FS, "SKILL.md")
}

// OptimiseSkill runs the search and stores an accepted rewrite.
func (a *App) OptimiseSkill(ctx context.Context, o OptimiseOptions) (*OptimiseOutcome, error) {
	sk, err := a.optimisable(o.Skill)
	if err != nil {
		return nil, err
	}
	if o.Adopt && (o.DryRun || sk.Source != skills.SourceBuiltin) {
		return nil, &UserError{"--adopt is for a built-in skill's real run: a dry run saves nothing, and a learned skill's rewrite goes on trial first"}
	}
	if o.Adopt && o.MaxTasks > 0 {
		return nil, &UserError{"--adopt with --tasks: a partial task list is too noisy to put a rewrite into use (an 8-task run accepted a rewrite that the full 28 tasks then rejected). Drop --tasks to adopt, or leave out --adopt to save it as a candidate"}
	}
	if sk.Source == skills.SourceLearned && a.Learned().HasNext(sk.Name) {
		return nil, &UserError{fmt.Sprintf("a new version of %s is already on trial; wait for it to be adopted or rejected", sk.Name)}
	}
	if a.Cfg.Learning.RequireAC && !o.Force {
		if on, known := a.onAC(); known && !on {
			return nil, &UserError{"on battery: optimising runs dozens of tasks. Plug in, or pass --force"}
		}
	}
	say := func(f string, args ...any) {
		if o.Progress != nil {
			o.Progress(fmt.Sprintf(f, args...))
		}
	}
	var train, held []eval.Task
	if o.tasks != nil {
		train, held = o.tasks()
	} else if train, held, err = a.OptimiseTasks(sk.Name); err != nil {
		return nil, err
	}
	if o.MaxTasks > 0 {
		if o.MaxTasks < learning.MinTrainTasks {
			return nil, &UserError{fmt.Sprintf("--tasks needs at least %d: fewer can't tell two rewrites apart", learning.MinTrainTasks)}
		}
		train, held = sampleTasks(train, o.MaxTasks), sampleTasks(held, max(2, o.MaxTasks/3))
	}
	m, err := a.FindModel(ctx, o.Model)
	if err != nil {
		return nil, err
	}
	models, _ := a.Models(ctx)
	am, err := a.authorModel(models, m.Name)
	if err != nil {
		return nil, err
	}
	out := &OptimiseOutcome{Skill: sk.Name, Source: string(sk.Source), Model: m.Name, Author: am.Name}
	for _, t := range train {
		out.Train = append(out.Train, t.ID)
	}
	for _, t := range held {
		out.Held = append(out.Held, t.ID)
	}

	raw, err := a.skillMD(sk)
	if err != nil {
		return nil, err
	}
	front, body, err := learning.SplitSkillMD(raw)
	if err != nil {
		return nil, err
	}
	byID := map[string]eval.Task{}
	for _, t := range append(slices.Clone(train), held...) {
		byID[t.ID] = t
	}
	// A candidate is the skill with its SKILL.md swapped; its other files are the skill's own.
	load := func(b string) (*skills.Skill, error) {
		md := learning.JoinSkillMD(front, b)
		c, issues := skills.Load(learning.WithSkillMD(sk.FS, md), sk.Name, sk.Dir, sk.Source)
		for _, is := range issues {
			if is.Level == "error" {
				return nil, errors.New(is.Msg)
			}
		}
		if c == nil {
			return nil, errors.New("it doesn't load")
		}
		if sk.Source == skills.SourceLearned {
			if ch := learning.FileChecks(c, issues, map[string][]byte{"SKILL.md": md}, a.secretValuesKnown()); !ch.OK() {
				return nil, errors.New(strings.Join(append(ch.Problems, ch.Escalations...), "; "))
			}
		}
		return c, nil
	}
	runs := 0
	opt := &learning.Optimiser{
		Skill: sk.Name, Base: body, Budget: o.Budget, Train: out.Train, Held: out.Held,
		LLM:    a.authorLLM(am),
		Values: taskValues(train),
		Check:  func(b string) error { _, err := load(b); return err },
		Eval: func(ctx context.Context, b string, ids []string) ([]learning.TaskResult, error) {
			c, err := load(b)
			if err != nil {
				return nil, err
			}
			var rs []learning.TaskResult
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				t := byID[id]
				began := time.Now()
				r, calls := a.evalTaskWith(ctx, m.Name, t, false, c)
				runs++
				mark := "✗"
				if r.Passed {
					mark = "✓"
				}
				say("    %s %s (%d steps, %s)", mark, id, r.Steps, time.Since(began).Round(time.Second))
				rs = append(rs, learning.TaskResult{ID: id, Passed: r.Passed && !r.Skipped, Steps: r.Steps, Why: r.Why,
					Prompt: t.Prompt, Want: describeTask(t), Calls: calls})
			}
			return rs, nil
		},
		Progress: say,
	}
	if sk.Source == skills.SourceLearned {
		opt.MaxLines = learning.MaxSkillLines - 1 - strings.Count(front, "\n")
	}
	res, err := opt.Run(ctx)
	out.Result = res
	if err != nil {
		if errors.Is(err, learning.ErrTooFewTasks) {
			return nil, &UserError{err.Error()}
		}
		return out, err
	}
	say("%d task runs", runs)
	out.Report = a.writeOptimiseReport(out)
	f, ok := res.Finalist()
	if !ok || o.DryRun {
		return out, nil
	}
	base, best := res.Candidates[0].Score, f.Score
	note := fmt.Sprintf("Optimised on %s by %s: %s", m.Name, am.Name, res.Why)
	switch sk.Source {
	case skills.SourceBuiltin:
		emb, err := embeddedSkillMD(sk.Name)
		if err != nil {
			return out, err
		}
		n, err := a.Tuned().Save(sk.Name, learning.JoinSkillMD(front, f.Body), learning.TunedVersion{
			BaseHash: learning.HashMD(emb), Note: note, Train: best, Held: res.BestHeld, Base: base, BaseHeld: res.BaseHeld})
		if err != nil {
			return out, err
		}
		out.Saved = fmt.Sprintf("tuned version %d of %s is waiting: `noskill skills optimise adopt %s` uses it", n, sk.Name, sk.Name)
		if o.Adopt {
			if _, err := a.OptimiseAdopt(ctx, sk.Name); err != nil {
				return out, err
			}
			out.Saved = fmt.Sprintf("tuned version %d of %s is now in use; `noskill skills optimise revert %s` goes back", n, sk.Name, sk.Name)
		}
	case skills.SourceLearned:
		saved, problem, err := a.saveLearnedRewrite(ctx, sk, front, f.Body, am, note)
		if err != nil {
			return out, err
		}
		out.Saved, out.Problem = saved, problem
	}
	return out, nil
}

// saveLearnedRewrite puts a learned skill's rewrite on trial in .next, after its required tests
// pass (the same gate as a refinement, FR-L4).
func (a *App) saveLearnedRewrite(ctx context.Context, sk *skills.Skill, front, body string, am Model, note string) (string, string, error) {
	lib := a.Learned()
	dir, err := lib.NewRefineDir(sk.Name)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), learning.JoinSkillMD(front, body), 0o600); err != nil {
		return "", "", err
	}
	c, _ := skills.Load(os.DirFS(dir), sk.Name, dir, skills.SourceLearned)
	if c == nil {
		to, _ := lib.Reject(dir, "the rewrite doesn't load")
		return "", "the rewrite doesn't load; kept in " + to, nil
	}
	home, _ := os.UserHomeDir()
	results, err := a.replayCases(ctx, c, dir, am, home)
	if err != nil {
		return "", "", err
	}
	if !learning.Passed(results) {
		to, _ := lib.Reject(dir, "better on the checked tasks but failed its own tests")
		return "", "the rewrite failed the skill's own tests; kept in " + to, nil
	}
	v, err := lib.Publish(dir, note)
	if err != nil {
		return "", "", err
	}
	a.recordVersion(ctx, sk.Name, v, sk.Meta.Version, note, "optimise")
	return fmt.Sprintf("%s %s is on trial (the current %s stays until it proves itself)", sk.Name, v, sk.Meta.Version), "", nil
}

// taskValues are the words from the training tasks a rewrite may not pick up: file and folder
// names and the words the checks look for.
func taskValues(ts []eval.Task) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if len(v) >= 3 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, t := range ts {
		for f := range t.Files {
			add(f)
			for _, part := range strings.Split(f, "/") {
				add(part)
			}
		}
		for _, p := range append(slices.Clone(t.End.Exists), t.End.Absent...) {
			for _, part := range strings.Split(p, "/") {
				add(part)
			}
		}
		for _, w := range t.Check.Contains {
			add(w)
		}
	}
	sort.Strings(out)
	return out
}

// describeTask says what a task's check wants, for the reflection prompt.
func describeTask(t eval.Task) string {
	var parts []string
	if len(t.Tools) > 0 {
		parts = append(parts, "call "+strings.Join(t.Tools, " or "))
	}
	if len(t.End.Exists) > 0 {
		parts = append(parts, "leave "+strings.Join(t.End.Exists, ", "))
	}
	if len(t.End.Absent) > 0 {
		parts = append(parts, "leave no "+strings.Join(t.End.Absent, ", "))
	}
	for f, s := range t.End.FileHas {
		parts = append(parts, fmt.Sprintf("%s containing %q", f, s))
	}
	if len(t.Check.Contains) > 0 {
		parts = append(parts, "answer with "+strings.Join(t.Check.Contains, ", "))
	}
	if t.Check.Number != nil {
		parts = append(parts, fmt.Sprintf("answer the number %g", *t.Check.Number))
	}
	return strings.Join(parts, "; ")
}

// OptimiseAdopt starts using a built-in skill's tuned version.
func (a *App) OptimiseAdopt(ctx context.Context, name string) (int, error) {
	if _, err := a.builtinToTune(name); err != nil {
		return 0, err
	}
	tu := a.Tuned()
	info, ok, err := tu.Info(name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, &UserError{fmt.Sprintf("%s has no tuned version; make one with `noskill skills optimise %s`", name, name)}
	}
	emb, err := embeddedSkillMD(name)
	if err != nil {
		return 0, err
	}
	if tu.Stale(name, emb) {
		return 0, &UserError{fmt.Sprintf("the tuned version of %s was made from an older built-in; optimise it again", name)}
	}
	if err := tu.SetStatus(name, learning.TunedActive, "adopted"); err != nil {
		return 0, err
	}
	return info.Current, a.loadSkills(ctx)
}

// OptimiseRevert goes back to a built-in skill's embedded instructions (version 0) or to an
// earlier tuned version.
func (a *App) OptimiseRevert(ctx context.Context, name string, version int, why string) error {
	if _, err := a.builtinToTune(name); err != nil {
		return err
	}
	tu := a.Tuned()
	if _, ok, err := tu.Info(name); err != nil || !ok {
		if err == nil {
			err = &UserError{fmt.Sprintf("%s uses its built-in instructions already", name)}
		}
		return err
	}
	var err error
	if version > 0 {
		err = tu.UseVersion(name, version, learning.TunedActive, why)
	} else {
		err = tu.SetStatus(name, learning.TunedReverted, why)
	}
	if err != nil {
		return err
	}
	return a.loadSkills(ctx)
}

func (a *App) builtinToTune(name string) (*skills.Skill, error) {
	sk, ok := a.Skills.Get(name)
	if !ok || sk.Source != skills.SourceBuiltin {
		return nil, &UserError{fmt.Sprintf("%s isn't a built-in skill; learned skills' rewrites are adopted on trial", name)}
	}
	return sk, nil
}

// TunedStatus describes a built-in skill's tuned versions and the diff of the current one
// against the embedded instructions.
type TunedStatus struct {
	Skill   string             `json:"skill"`
	Info    learning.TunedInfo `json:"info"`
	InUse   int                `json:"in_use"` // 0 = the embedded instructions
	Stale   bool               `json:"stale"`
	Diff    string             `json:"diff,omitempty"`
	Changes string             `json:"changelog,omitempty"`
}

func (a *App) OptimiseStatus(name string) (*TunedStatus, error) {
	if _, err := a.builtinToTune(name); err != nil {
		return nil, err
	}
	tu := a.Tuned()
	info, ok, err := tu.Info(name)
	if err != nil {
		return nil, err
	}
	st := &TunedStatus{Skill: name, Info: info, InUse: a.TunedVersion(name)}
	if !ok {
		return st, nil
	}
	emb, err := embeddedSkillMD(name)
	if err != nil {
		return nil, err
	}
	st.Stale = tu.Stale(name, emb)
	cur, err := tu.VersionMD(name, info.Current)
	if err != nil {
		return nil, err
	}
	st.Diff = learning.DiffText(string(emb), string(cur), "built-in", fmt.Sprintf("tuned %d", info.Current))
	if b, err := os.ReadFile(filepath.Join(tu.SkillDir(name), "CHANGELOG.md")); err == nil {
		st.Changes = string(b)
	}
	return st, nil
}
