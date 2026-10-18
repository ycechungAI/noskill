package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noskill/internal/learning"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// Refinement (LEARNING.md §6, FR-L4): an active learned skill that keeps failing gets revised
// steps. The new version waits in <name>/.next and is used in place of the current one until
// promote_after good runs adopt it or two failures reject it. Only learned skills are refined:
// built-in and user skills are never rewritten.

const (
	refineWindow   = 10                 // rolling success rate over this many rated runs …
	refineMinRated = 5                  // … once there are at least this many
	refineBelow    = 0.7                // … below this rate
	refineDedupe   = 3 * 24 * time.Hour // one refine per skill in this time
	maxFailCases   = 2                  // failed runs turned into test cases per refinement
)

// needsRefine applies the rule: 2 failures in a row, or under 70% success over the last 10 rated
// runs.
func needsRefine(recent []store.RatedRun) bool {
	if len(recent) >= 2 && recent[0].Outcome == "fail" && recent[1].Outcome == "fail" {
		return true
	}
	if len(recent) < refineMinRated {
		return false
	}
	ok := 0
	for _, r := range recent[:min(len(recent), refineWindow)] {
		if r.Outcome == "success" {
			ok++
		}
	}
	return float64(ok)/float64(min(len(recent), refineWindow)) < refineBelow
}

func (a *App) queueRefine(ctx context.Context, name string, recent []store.RatedRun) {
	var runs []string
	for _, r := range recent {
		if r.Outcome == "fail" && len(runs) < maxFailCases {
			runs = append(runs, r.RunID)
		}
	}
	if len(runs) == 0 {
		return
	}
	a.DB.Enqueue(ctx, store.LearnItem{Kind: "refine", Skill: name, RunIDs: runs, Reason: "failures"}, time.Now(), refineDedupe)
}

// nextSkill loads the refinement waiting in <name>/.next, or nil.
func (a *App) nextSkill(name string) *skills.Skill {
	dir := a.Learned().NextDir(name)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return nil
	}
	sk, _ := skills.Load(os.DirFS(dir), name, dir, skills.SourceLearned)
	return sk
}

// withRefinements puts a learned skill's refinement on trial in place of the current version. It
// asks first only when the refinement needs access the current version didn't have.
func (a *App) withRefinements(ctx context.Context, req RunRequest, text string, matches []skills.Match) []skills.Match {
	for i, m := range matches {
		if m.Skill.Source != skills.SourceLearned || m.Skill.Status() != "active" {
			continue
		}
		nxt := a.nextSkill(m.Skill.Name)
		if nxt == nil {
			continue
		}
		growth := learning.AccessGrowth(m.Skill, nxt)
		if len(growth) == 0 || a.useCandidate(ctx, req, text, nxt, growth) {
			matches[i].Skill = nxt
		}
	}
	return matches
}

// nextOutcome adopts or rejects a refinement on trial after one of its runs is rated.
func (a *App) nextOutcome(ctx context.Context, name, version, runID string, st store.SkillStats) {
	lib := a.Learned()
	switch {
	case st.Failures >= trialMaxFailures || (st.Failures > 0 && a.Cfg.Learning.PromoteMode == "auto"):
		if _, err := lib.Reject(lib.NextDir(name), fmt.Sprintf("Version %s failed %d run(s) on trial (last: run %s); the current version stays.", version, st.Failures, runID)); err != nil {
			slog.Warn("couldn't set aside a failed refinement", "skill", name, "err", err)
		}
	case st.Streak >= max(a.Cfg.Learning.PromoteAfter, 1):
		cur, _ := a.Skills.Get(name)
		v, err := lib.AdoptNext(name)
		if err != nil {
			slog.Warn("couldn't adopt a refinement", "skill", name, "err", err)
			return
		}
		parent := ""
		if cur != nil {
			parent = cur.Meta.Version
		}
		a.recordVersion(ctx, name, v, parent, fmt.Sprintf("Adopted after %d good runs on trial.", st.Streak), "learning")
		if err := a.loadSkills(ctx); err != nil {
			slog.Warn("reloading skills", "err", err)
		}
	}
}

// refineFromQueue revises a learned skill's steps from its failed runs and verifies the result:
// the old tests plus one case per failed run.
func (a *App) refineFromQueue(ctx context.Context, it store.LearnItem) (string, error) {
	sk, ok := a.Skills.Get(it.Skill)
	lib := a.Learned()
	if !ok || sk.Source != skills.SourceLearned || !lib.Exists(it.Skill) {
		return "", fmt.Errorf("%w: %s isn't a learned skill", errSkip, it.Skill)
	}
	if a.nextSkill(it.Skill) != nil {
		return "", fmt.Errorf("%w: a refinement of %s is already on trial", errSkip, it.Skill)
	}
	var traces []*learning.Trace
	var fails []learning.Failure
	for _, id := range it.RunIDs {
		run, err := a.DB.GetRun(ctx, id)
		if err != nil || run.TracePath == "" {
			continue
		}
		if tr, err := learning.ReadTrace(run.TracePath); err == nil && tr.Request != "" {
			traces = append(traces, tr)
			fails = append(fails, learning.FailureOf(tr))
		}
	}
	if len(traces) == 0 {
		return "", fmt.Errorf("%w: none of the failed runs' traces can be read", errSkip)
	}
	models, _ := a.Models(ctx)
	m, err := a.authorModel(models, traces[0].Model)
	if err != nil {
		return "", err
	}
	au := a.author(ctx, models, m)
	dir, err := lib.NewRefineDir(it.Skill)
	if err != nil {
		return "", err
	}
	rep, err := a.verifyRefinement(ctx, sk, dir, traces, fails, au, m)
	if err != nil {
		if ctx.Err() == nil {
			lib.Reject(dir, "refinement stopped: "+err.Error())
		}
		return "", err
	}
	if !rep.ok() {
		to, _ := lib.Reject(dir, rep.text())
		return fmt.Sprintf("couldn't refine %s (%s); kept in %s", it.Skill, rep.short(), to), nil
	}
	var ids []string
	for _, tr := range traces {
		ids = append(ids, tr.RunID)
	}
	note := fmt.Sprintf("Refined after failed run(s) %s. %s", strings.Join(ids, ", "), rep.text())
	v, err := lib.Publish(dir, note)
	if err != nil {
		return "", err
	}
	a.recordVersion(ctx, it.Skill, v, sk.Meta.Version, note, "learning")
	return fmt.Sprintf("refined %s: %s is on trial (the current %s stays until it proves itself)", it.Skill, v, sk.Meta.Version), nil
}

// verifyRefinement writes revised steps into dir and runs the gate, revising again with the
// failed tests up to maxRepairs times.
func (a *App) verifyRefinement(ctx context.Context, cur *skills.Skill, dir string, traces []*learning.Trace, fails []learning.Failure,
	au *learning.Author, m Model) (*verifyReport, error) {
	read := func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(n))) }
	cases, err := read("tests/cases.yaml")
	if err != nil {
		return nil, fmt.Errorf("%s has no tests to keep passing: %w", cur.Name, err)
	}
	for _, tr := range traces {
		var fxPath string
		var fx []byte
		if cases, fxPath, fx, err = learning.AddFailureCase(cases, tr); err != nil {
			return nil, err
		}
		if fxPath != "" {
			p := filepath.Join(dir, "tests", filepath.FromSlash(fxPath))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, fx, 0o600); err != nil {
				return nil, err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tests", "cases.yaml"), cases, 0o600); err != nil {
		return nil, err
	}
	md, err := read("SKILL.md")
	if err != nil {
		return nil, err
	}
	steps := learning.StepsOf(string(md))
	rep := &verifyReport{}
	for {
		revised, err := au.Revise(ctx, steps, fails, rep.results)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			rep.check.Problems = append(rep.check.Problems, "the author model gave no usable steps ("+err.Error()+")")
			return rep, nil
		}
		steps = revised
		out, err := learning.WithSteps(string(md), steps)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(out), 0o600); err != nil {
			return nil, err
		}
		sk, issues := skills.Load(os.DirFS(dir), cur.Name, dir, skills.SourceLearned)
		files := map[string][]byte{"SKILL.md": []byte(out), "tests/cases.yaml": cases}
		rep.check = learning.FileChecks(sk, issues, files, a.secretValuesKnown())
		if sk != nil {
			rep.check.Escalations = learning.AccessGrowth(cur, sk)
		}
		if !rep.check.OK() {
			return rep, nil
		}
		rep.results, err = a.replayCases(ctx, sk, dir, m, au.Home)
		if err != nil {
			return nil, err
		}
		rep.passedTests = learning.Passed(rep.results)
		if rep.passedTests || rep.repairs >= maxRepairs {
			return rep, nil
		}
		rep.repairs++
	}
}
