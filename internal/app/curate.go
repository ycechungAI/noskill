package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"noskill/internal/decide"
	"noskill/internal/learning"
	"noskill/internal/notify"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// The curator (LEARNING.md §7, FR-L5): weekly and idle-only, it retires learned skills nobody
// uses, folds near-duplicates together and reports the week. Nothing is deleted: retired and merged
// skills are archived, and `noskill skills enable` brings one back.

const (
	curateKey     = "learning.curated_at"
	curateEvery   = 7 * 24 * time.Hour
	staleAfter    = 30 * 24 * time.Hour
	archiveAfter  = 90 * 24 * time.Hour
	duplicateMin  = 0.85 // decide("duplicate") needed to merge
	maxMergesWeek = 3
)

func (a *App) curateDue(ctx context.Context) bool {
	v, _ := a.DB.GetState(ctx, curateKey)
	t, err := time.Parse(time.RFC3339, v)
	return err != nil || time.Since(t) >= curateEvery
}

// Curate retires unused learned skills, queues likely duplicates for merging and sends the weekly
// report. It returns the report.
func (a *App) Curate(ctx context.Context) (string, error) {
	now := time.Now()
	lib := a.Learned()
	var live []*skills.Skill
	retired := 0
	for _, s := range a.Skills.All() {
		st := s.Status()
		if s.Source != skills.SourceLearned || (st != "active" && st != "candidate" && st != "stale") {
			continue
		}
		last, err := a.DB.SkillLastUse(ctx, s.Name)
		if err != nil {
			return "", err
		}
		idle := now.Sub(last)
		switch {
		case !last.IsZero() && idle >= archiveAfter:
			a.setLearnedStatus(ctx, s.Name, "archived", fmt.Sprintf("Archived: unused for %d days.", int(idle.Hours()/24)))
			retired++
		case !last.IsZero() && idle >= staleAfter && st != "stale":
			a.setLearnedStatus(ctx, s.Name, "stale", fmt.Sprintf("Stale: unused for %d days; hidden from selection.", int(idle.Hours()/24)))
			retired++
		case st != "stale":
			live = append(live, s)
		}
	}
	// Pairs that share most tools, closest requests first: the cap goes to the likeliest
	// duplicates rather than to whichever pair comes first.
	usage, _ := a.DB.SkillUsage(ctx)
	type pair struct {
		x, y *skills.Skill
		text float64
	}
	var pairs []pair
	for i := range live {
		for j := i + 1; j < len(live); j++ {
			x, y := live[i], live[j]
			dx, dy := lib.SkillDir(x.Name), lib.SkillDir(y.Name)
			if learning.Overlap(learning.ToolsOf(dx), learning.ToolsOf(dy)) < learning.FamilyTools {
				continue
			}
			pairs = append(pairs, pair{x, y, learning.TextOverlap(learning.SourceRequest(dx), learning.SourceRequest(dy))})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].text > pairs[j].text })
	queued := 0
	for _, p := range pairs {
		if queued >= maxMergesWeek {
			break
		}
		x, y := p.x, p.y
		// Keep one with a script tool (one call does the task), then the one with more
		// successes, then an active one over a candidate.
		sx, sy := len(x.Tools) > 0, len(y.Tools) > 0
		ux, uy := usage[x.Name].Success, usage[y.Name].Success
		if (sy && !sx) || (sx == sy && (uy > ux || (uy == ux && y.Status() == "active" && x.Status() != "active"))) {
			x, y = y, x
		}
		if ok, _ := a.DB.Enqueue(ctx, store.LearnItem{Kind: "merge", Skill: x.Name, Pattern: y.Name, Reason: "duplicate"}, now, learnDedupe); ok {
			queued++
		}
	}
	week := now.Add(-curateEvery)
	learned, _ := a.DB.LearningDone(ctx, "draft", "learned ", week)
	refined, _ := a.DB.LearningDone(ctx, "refine", "refined ", week)
	merged, _ := a.DB.LearningDone(ctx, "merge", "merged ", week)
	report := fmt.Sprintf("This week: %d skill(s) learned, %d refined, %d merged, %d retired.", learned, refined, merged, retired)
	if learned+refined+merged+retired > 0 {
		a.Hub.Send(ctx, notify.Message{ID: "learning-" + now.Format("2006-01-02"), Title: "noskill learning", Body: report})
	}
	return report, a.DB.SetState(ctx, curateKey, now.UTC().Format(time.RFC3339), now)
}

// mergeFromQueue checks that two learned skills really do the same task and, if the kept one
// passes the other's tests too, puts the merged version on trial and archives the other.
func (a *App) mergeFromQueue(ctx context.Context, it store.LearnItem) (string, error) {
	keep, ok1 := a.Skills.Get(it.Skill)
	dup, ok2 := a.Skills.Get(it.Pattern)
	lib := a.Learned()
	switch {
	case !ok1 || !ok2 || keep.Source != skills.SourceLearned || dup.Source != skills.SourceLearned:
		return "", fmt.Errorf("%w: %s and %s aren't both learned skills", errSkip, it.Skill, it.Pattern)
	case keep.Status() == "archived" || dup.Status() == "archived":
		return "", fmt.Errorf("%w: one of them is archived", errSkip)
	case a.nextSkill(keep.Name) != nil:
		return "", fmt.Errorf("%w: %s has a refinement on trial", errSkip, keep.Name)
	}
	models, _ := a.Models(ctx)
	m, err := a.authorModel(models, "")
	if err != nil {
		return "", err
	}
	describe := func(s *skills.Skill) string {
		return s.Name + ": " + s.Description + "\nSteps:\n" + strings.Join(learning.StepsOf(s.Body), "\n")
	}
	// Same tools and the same request words: one family, no need to ask. Otherwise the decider
	// judges whether they're paraphrases of one task.
	dk, dd := lib.SkillDir(keep.Name), lib.SkillDir(dup.Name)
	if !learning.SameFamily(learning.ToolsOf(dk), learning.ToolsOf(dd), learning.SourceRequest(dk), learning.SourceRequest(dd)) {
		ans, err := a.decider(ctx, models).Decide(ctx, map[string]any{"skill_a": describe(keep), "skill_b": describe(dup)},
			map[string]decide.Question{"duplicate": {Type: decide.Noul,
				Instructions: "Do skill_a and skill_b do the same task, so that one could handle the other's requests?"}})
		if err != nil {
			return "", err
		}
		if p := ans.Answers["duplicate"].Noul; p < duplicateMin {
			return fmt.Sprintf("kept %s and %s apart (duplicate %.2f, needs %.2f)", keep.Name, dup.Name, p, duplicateMin), nil
		}
	}
	dir, err := lib.NewRefineDir(keep.Name)
	if err != nil {
		return "", err
	}
	if err := learning.MergeInto(dir, lib.SkillDir(dup.Name), dup); err != nil {
		lib.Reject(dir, "merge failed: "+err.Error())
		return "", err
	}
	rep := &verifyReport{}
	sk, issues := skills.Load(os.DirFS(dir), keep.Name, dir, skills.SourceLearned)
	md, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	rep.check = learning.FileChecks(sk, issues, map[string][]byte{"SKILL.md": md}, a.secretValuesKnown())
	if sk != nil {
		rep.check.Escalations = learning.AccessGrowth(keep, sk)
	}
	if rep.check.OK() {
		home, _ := os.UserHomeDir()
		if rep.results, err = a.replayCases(ctx, sk, dir, m, home); err != nil {
			if ctx.Err() == nil {
				lib.Reject(dir, "merge stopped: "+err.Error())
			}
			return "", err
		}
		rep.passedTests = learning.Passed(rep.results)
	}
	if !rep.ok() {
		to, _ := lib.Reject(dir, rep.text())
		return fmt.Sprintf("kept %s and %s apart (%s); the attempt is in %s", keep.Name, dup.Name, rep.short(), to), nil
	}
	note := fmt.Sprintf("Merged with %s, which does the same task. %s", dup.Name, rep.text())
	v, err := lib.Publish(dir, note)
	if err != nil {
		return "", err
	}
	a.recordVersion(ctx, keep.Name, v, keep.Meta.Version, note, "learning")
	a.setLearnedStatus(ctx, dup.Name, "archived", fmt.Sprintf("Merged into %s (%s is on trial). `noskill skills enable %s` brings it back.", keep.Name, v, dup.Name))
	return fmt.Sprintf("merged %s into %s: %s is on trial", dup.Name, keep.Name, v), nil
}

// RestoreLearned makes a stale or archived learned skill active again (`noskill skills enable`).
func (a *App) RestoreLearned(ctx context.Context, name string) error {
	s, ok := a.Skills.Get(name)
	if !ok || s.Source != skills.SourceLearned {
		return fmt.Errorf("no learned skill named %q", name)
	}
	if err := a.Learned().SetStatus(name, "active", "Restored by you."); err != nil {
		return err
	}
	return a.loadSkills(ctx)
}
