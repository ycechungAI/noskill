package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/learning"
	"noskill/internal/skills"
)

// The trial period for learned skills (LEARNING.md §5, FR-L4). A new skill is a candidate: the
// first time it matches a request, noskill asks before using it. After promote_after good runs in
// a row it becomes active; two skips or two failed runs archive it. Nothing is deleted.

const (
	trialApprovalKind = "learned_skill"
	trialAskTimeout   = 7 * 24 * time.Hour // a queued question waits this long for an answer
	trialMaxSkips     = 2
	trialMaxFailures  = 2
)

// trialState is what noskill remembers about a candidate between runs (app_state).
type trialState struct {
	Version  string `json:"version"`
	Approved bool   `json:"approved"`
	Asked    string `json:"asked,omitempty"`    // a queued approval nobody has answered yet
	Failures int    `json:"failures,omitempty"` // failures already acted on (each one asks again)
}

func trialKey(name string) string { return "learning.trial." + name }

func (a *App) trialState(ctx context.Context, sk *skills.Skill) trialState {
	var st trialState
	if v, err := a.DB.GetState(ctx, trialKey(sk.Name)); err == nil && v != "" {
		json.Unmarshal([]byte(v), &st)
	}
	if st.Version != sk.Meta.Version {
		st = trialState{Version: sk.Meta.Version}
	}
	return st
}

func (a *App) saveTrial(ctx context.Context, name string, st trialState) {
	b, _ := json.Marshal(st)
	a.DB.SetState(ctx, trialKey(name), string(b), time.Now())
}

// trialSkills are the enabled learned skills still on trial.
func (a *App) trialSkills(ctx context.Context) ([]*skills.Skill, error) {
	enabled, err := a.DB.EnabledSkills(ctx)
	if err != nil {
		return nil, err
	}
	var out []*skills.Skill
	for _, s := range a.Skills.All() {
		if enabled[s.Name] && onTrial(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func onTrial(s *skills.Skill) bool {
	return s.Source == skills.SourceLearned && s.Status() == "candidate"
}

// gateTrials drops the candidates among the matches that may not be used for this request yet.
func (a *App) gateTrials(ctx context.Context, req RunRequest, text string, matches []skills.Match) []skills.Match {
	var out []skills.Match
	for _, m := range matches {
		if !onTrial(m.Skill) || a.useCandidate(ctx, req, text, m.Skill, nil) {
			out = append(out, m)
		}
	}
	return out
}

// useCandidate says whether a candidate may be used for this request. In ask mode (and shadow,
// which behaves like ask for now) the first match asks: at the terminal right away, otherwise as a
// queued approval while the request runs the normal way.
// growth, when set, is what a refinement needs beyond the current version: it is asked about the
// same way, and two skips set the refinement aside instead of archiving the skill.
func (a *App) useCandidate(ctx context.Context, req RunRequest, text string, sk *skills.Skill, growth []string) bool {
	if a.Cfg.Learning.PromoteMode == "auto" {
		return true
	}
	st := a.trialState(ctx, sk)
	if st.Approved {
		return true
	}
	if st.Asked != "" {
		it, err := a.Approvals.Get(ctx, st.Asked)
		switch {
		case err == nil && it.Status == approvals.Pending:
			return false // still waiting for an answer
		case err == nil && it.Status == approvals.Approved:
			st.Approved, st.Asked = true, ""
			a.saveTrial(ctx, sk.Name, st)
			return true
		case err == nil && it.Status == approvals.Skipped:
			st.Asked = ""
			a.saveTrial(ctx, sk.Name, st)
			if a.skipCandidate(ctx, sk, growth != nil) {
				return false
			}
		default: // expired or gone: ask again
			st.Asked = ""
		}
	}
	summary := fmt.Sprintf("noskill learned a way to do this: %s. Use it?", sk.Name)
	if growth != nil {
		summary = fmt.Sprintf("noskill improved %s; the new version also needs %s. Use it?", sk.Name, strings.Join(growth, ", "))
	}
	it, err := a.Approvals.Create(ctx, approvals.New{
		Source: "learning", Kind: trialApprovalKind, Timeout: trialAskTimeout, Summary: summary,
		Details: map[string]any{"skill": sk.Name, "version": sk.Meta.Version, "description": sk.Description,
			"request": clipRunes(text, 200), "needs": growth},
	})
	if err != nil {
		slog.Warn("couldn't ask about a learned skill", "skill", sk.Name, "err", err)
		return false
	}
	if req.Prompt == nil || req.Headless || req.ApprovalMode != "" {
		st.Asked = it.ID
		a.saveTrial(ctx, sk.Name, st)
		return false
	}
	ok, err := req.Prompt(ctx, it)
	if err != nil {
		return false // left pending; the next match looks at it again
	}
	if _, err := a.Approvals.Resolve(ctx, it.ID, approvals.Answer{Approve: ok, Via: "cli"}); err != nil && !errors.Is(err, approvals.ErrResolved) {
		return false
	}
	if !ok {
		a.saveTrial(ctx, sk.Name, st)
		a.skipCandidate(ctx, sk, growth != nil)
		return false
	}
	st.Approved = true
	a.saveTrial(ctx, sk.Name, st)
	return true
}

// skipCandidate counts a "no" and archives the candidate after the second (or, for a refinement,
// sets it aside); it reports whether it did.
func (a *App) skipCandidate(ctx context.Context, sk *skills.Skill, refinement bool) bool {
	n, err := a.DB.AddSkip(ctx, sk.Name, sk.Meta.Version)
	if err != nil || n < trialMaxSkips {
		return false
	}
	if refinement {
		lib := a.Learned()
		if _, err := lib.Reject(lib.NextDir(sk.Name), fmt.Sprintf("Version %s was skipped %d times; the current version stays.", sk.Meta.Version, n)); err != nil {
			slog.Warn("couldn't set aside a refinement", "skill", sk.Name, "err", err)
		}
		return true
	}
	a.setLearnedStatus(ctx, sk.Name, "archived", fmt.Sprintf("Archived: skipped %d times during its trial.", n))
	return true
}

// trialOutcome runs after any outcome is recorded: it refreshes skill_stats for the learned skills
// the run used, promotes or archives candidates, adopts or rejects refinements on trial, and queues
// a refinement for an active skill that keeps failing.
func (a *App) trialOutcome(ctx context.Context, runID string) {
	if !a.Cfg.Learning.Enabled {
		return
	}
	used, err := a.DB.RunSkills(ctx, runID)
	if err != nil {
		return
	}
	for name, version := range used {
		sk, ok := a.Skills.Get(name)
		if !ok || sk.Source != skills.SourceLearned {
			continue
		}
		st, err := a.DB.RefreshSkillStats(ctx, name, version, learning.MinConfidence)
		if err != nil {
			continue
		}
		if sk.Status() == "active" {
			if nxt := a.nextSkill(name); nxt != nil {
				if nxt.Meta.Version == version {
					a.nextOutcome(ctx, name, version, runID, st)
				}
			} else if sk.Meta.Version == version && needsRefine(st.Recent) {
				a.queueRefine(ctx, name, st.Recent)
			}
			continue
		}
		if !onTrial(sk) || sk.Meta.Version != version {
			continue
		}
		after := max(a.Cfg.Learning.PromoteAfter, 1)
		switch {
		case st.Failures >= trialMaxFailures || (st.Failures > 0 && a.Cfg.Learning.PromoteMode == "auto"):
			a.setLearnedStatus(ctx, name, "archived", fmt.Sprintf("Archived: %d failed run(s) during its trial (last: run %s).", st.Failures, runID))
		case st.Streak >= after:
			a.setLearnedStatus(ctx, name, "active", fmt.Sprintf("Promoted to active after %d good runs.", st.Streak))
		case st.Failures > 0:
			// One failure: ask again before the next use rather than archive on one bad run.
			ts := a.trialState(ctx, sk)
			if ts.Failures < st.Failures {
				ts.Failures, ts.Approved = st.Failures, false
				a.saveTrial(ctx, name, ts)
			}
		}
	}
}

func (a *App) setLearnedStatus(ctx context.Context, name, status, note string) {
	if err := a.Learned().SetStatus(name, status, note); err != nil {
		slog.Warn("couldn't change a learned skill's status", "skill", name, "status", status, "err", err)
		return
	}
	slog.Info("learned skill "+status, "skill", name, "note", note)
	if err := a.loadSkills(ctx); err != nil {
		slog.Warn("reloading skills", "err", err)
	}
}
