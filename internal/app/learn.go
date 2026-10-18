package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"noskill/internal/ids"
	"noskill/internal/learning"
	"noskill/internal/platform"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// The learning worker (LEARNING.md "Resources", FR-L2): it mines finished runs into the learning
// queue and works through the queue, but only when nobody has used noskill for idle_minutes, the
// machine is on AC power and no job is running. An interactive request cancels what it's doing;
// the item goes back to the queue.

const (
	mineCursorKey = "learning.mined_until" // ended_at (ms) of the last run the miner looked at
	mineSettle    = 2 * time.Minute        // let the decision judge (and a quick /rate) land first
	learnStale    = time.Hour              // a running item older than this was abandoned
	learnDedupe   = 30 * 24 * time.Hour
)

// LearnLoop runs the worker until ctx ends; `serve` (and so the daemon) starts it.
func (a *App) LearnLoop(ctx context.Context, logf func(string, ...any)) {
	if !a.Cfg.Learning.Enabled {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if why := a.LearningBlocked(ctx); why != "" {
			continue
		}
		did, err := a.LearnOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil && logf != nil:
			logf("learning: %v", err)
		case did != "" && logf != nil:
			logf("learning: %s", did)
		}
	}
}

// LearningBlocked says why learning can't run now, or "" when it can.
func (a *App) LearningBlocked(ctx context.Context) string {
	if !a.Cfg.Learning.Enabled {
		return "learning is off ([learning] enabled = false)"
	}
	if a.interactive.Load() > 0 {
		return "a request is waiting"
	}
	last, running, err := a.DB.LastActivity(ctx)
	if err != nil {
		return err.Error()
	}
	if running {
		return "a request is running"
	}
	if idle := time.Duration(a.Cfg.Learning.IdleMinutes) * time.Minute; time.Since(last) < idle {
		return fmt.Sprintf("noskill was used %s ago (waits for %s of quiet)", time.Since(last).Round(time.Second), idle)
	}
	if job, _ := a.DB.JobRunning(ctx); job {
		return "a job is running"
	}
	if a.Cfg.Learning.RequireAC {
		if on, _ := a.onAC(); !on {
			return "on battery"
		}
	}
	return ""
}

func (a *App) onAC() (bool, bool) {
	if a.powerOverride != nil {
		return a.powerOverride()
	}
	return platform.OnACPower()
}

// LearnOnce mines new runs, then works on one queued item within tonight's caps. It returns what it
// did ("" when there was nothing to do).
func (a *App) LearnOnce(ctx context.Context) (string, error) {
	a.applyOutcomeChecks(ctx)
	if _, err := a.MineRuns(ctx); err != nil {
		return "", err
	}
	if n, err := a.llmJudge(ctx); err != nil {
		return "", err
	} else if n > 0 {
		return fmt.Sprintf("LLM judge: judged %d run(s)", n), nil
	}
	if a.evalDue(ctx) {
		report, err := a.EvalWeekly(ctx)
		if err != nil {
			return "", err
		}
		if report != "" {
			return report, nil
		}
	}
	if a.curateDue(ctx) {
		report, err := a.Curate(ctx)
		if err != nil {
			return "", err
		}
		return "curator: " + report, nil
	}
	day := time.Now().Add(-24 * time.Hour)
	var kinds []string
	if n, _ := a.DB.LearningStarted(ctx, "draft", day); n < a.Cfg.Learning.MaxDraftsPerNight {
		kinds = append(kinds, "draft")
	}
	if n, _ := a.DB.LearningStarted(ctx, "refine", day); n < a.Cfg.Learning.MaxRefinesPerNight {
		kinds = append(kinds, "refine")
	}
	kinds = append(kinds, "merge") // the curator queues few; they're cheap
	it, ok, err := a.DB.ClaimLearning(ctx, kinds, time.Now(), learnStale)
	if err != nil || !ok {
		return "", err
	}
	return a.processLearning(ctx, it, true)
}

// processLearning runs one claimed item. Background work can be pre-empted by an interactive
// request: the item then goes back to the queue.
func (a *App) processLearning(ctx context.Context, it store.LearnItem, background bool) (string, error) {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release, lctx, err := a.acquire(lctx, 0, background)
	if err != nil {
		a.DB.FinishLearning(context.WithoutCancel(ctx), it.ID, "pending", "", "", time.Now())
		return "", err
	}
	defer release()
	if background {
		a.mu.Lock()
		a.learnCancel = cancel
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			a.learnCancel = nil
			a.mu.Unlock()
		}()
	}
	var result string
	switch it.Kind {
	case "draft":
		result, err = a.draftFromQueue(lctx, it)
	case "refine":
		result, err = a.refineFromQueue(lctx, it)
	case "merge":
		result, err = a.mergeFromQueue(lctx, it)
	default:
		err = fmt.Errorf("%s items arrive with the curator", it.Kind)
	}
	now := time.Now()
	wctx := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		a.DB.FinishLearning(wctx, it.ID, "done", result, "", now)
		return fmt.Sprintf("%s %s: %s", it.Kind, it.ID, result), nil
	case lctx.Err() != nil && ctx.Err() == nil, errors.Is(err, context.Canceled):
		a.DB.FinishLearning(wctx, it.ID, "pending", "", "pre-empted by a request", now)
		return "", nil
	case errors.Is(err, errSkip):
		a.DB.FinishLearning(wctx, it.ID, "skipped", result, err.Error(), now)
		return fmt.Sprintf("%s %s skipped: %v", it.Kind, it.ID, err), nil
	default:
		a.DB.FinishLearning(wctx, it.ID, "failed", result, err.Error(), now)
		return "", fmt.Errorf("%s %s: %w", it.Kind, it.ID, err)
	}
}

// errSkip marks an item that turned out not worth doing (not a failure).
var errSkip = errors.New("skipped")

// preemptLearning cancels background learning work so an interactive request gets the model.
func (a *App) preemptLearning() {
	a.mu.Lock()
	c := a.learnCancel
	a.mu.Unlock()
	if c != nil {
		c()
	}
}

// MineRuns looks at runs that finished since the last look and queues the ones worth learning
// from (LEARNING.md §2). It returns how many it queued.
func (a *App) MineRuns(ctx context.Context) (int, error) {
	cur, _ := a.DB.GetState(ctx, mineCursorKey)
	ms, _ := strconv.ParseInt(cur, 10, 64)
	after := time.UnixMilli(ms)
	if ms == 0 {
		after = time.Now().Add(-learning.RepeatWindow)
	}
	settled := time.Now().Add(-mineSettle)
	runs, err := a.DB.RunsToMine(ctx, after, 2, 200)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, r := range runs {
		if r.EndedAt.After(settled) {
			break
		}
		if ok, err := a.MineRun(ctx, r); err != nil {
			return queued, err
		} else if ok {
			queued++
		}
		if err := a.DB.SetState(ctx, mineCursorKey, strconv.FormatInt(r.EndedAt.UnixMilli(), 10), time.Now()); err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// MineRun queues one run when the mining rules say it's worth a skill. `noskill rate` calls it
// right away, since an explicit rating may come after the miner looked.
func (a *App) MineRun(ctx context.Context, r store.Run) (bool, error) {
	if r.TracePath == "" || r.ParentID != "" {
		return false, nil
	}
	repeats := 0
	if r.ToolPattern != "" {
		repeats, _ = a.DB.PatternCount(ctx, r.ToolPattern, time.Now().Add(-learning.RepeatWindow))
	}
	rules := learning.MineRules{MinToolCalls: a.Cfg.Learning.MinToolCalls, Novelty: a.Cfg.Learning.NoveltyThreshold}
	if learning.Worth(r, 0, repeats, rules) == "" && learning.Worth(r, 1, repeats, rules) == "" {
		return false, nil // not even with no skill covering it: skip the coverage check
	}
	tr, err := learning.ReadTrace(r.TracePath)
	if err != nil {
		return false, nil // trace purged or unreadable: nothing to learn from
	}
	cov, _, err := a.coverage(ctx, tr.Request)
	if err != nil {
		return false, err
	}
	reason := learning.Worth(r, cov, repeats, rules)
	if reason == "" {
		return false, nil
	}
	return a.DB.Enqueue(ctx, store.LearnItem{ID: ids.New(), Kind: "draft", RunIDs: []string{r.ID}, Reason: reason, Pattern: r.ToolPattern},
		time.Now(), learnDedupe)
}

// coverage is how well existing skills (learned candidates included) already match a request:
// the best selector score, embedding when available.
func (a *App) coverage(ctx context.Context, request string) (float64, string, error) {
	var cands []*skills.Skill
	for _, s := range a.Skills.All() {
		if st := s.Status(); st == "active" || st == "candidate" {
			cands = append(cands, s)
		}
	}
	sel := &skills.Selector{Cache: a.DB, MinScore: 1e-9, MinKeyword: 1e-9, MinLearned: 1e-9}
	if models, err := a.Models(ctx); err == nil {
		if em := a.embedModel(models); em != nil {
			sel.Embedder, sel.EmbedModel = em.NB.B, em.Name
		}
	}
	ms, err := sel.Select(ctx, cands, request, 1)
	if err != nil || len(ms) == 0 {
		return 0, "", err
	}
	return ms[0].Score, ms[0].Skill.Name, nil
}

// Learn is `noskill learn <run>` and `/learn`: queue the run for drafting whatever the mining
// rules say, as long as it wasn't marked no-learn and didn't fail.
func (a *App) Learn(ctx context.Context, runID string) (store.LearnItem, error) {
	r, err := a.DB.GetRun(ctx, runID)
	if err != nil {
		return store.LearnItem{}, &UserError{fmt.Sprintf("no run %q (see `noskill traces list`)", runID)}
	}
	switch {
	case r.NoLearn || r.TracePath == "":
		return store.LearnItem{}, &UserError{"that run was marked --no-learn, so it has no trace to learn from"}
	case r.Outcome == "fail":
		return store.LearnItem{}, &UserError{"that run was rated as failed; there's nothing to repeat"}
	case r.Status != "done":
		return store.LearnItem{}, &UserError{fmt.Sprintf("that run is %s, not done", r.Status)}
	case r.ToolCalls == 0:
		return store.LearnItem{}, &UserError{"that run used no tools, so there's no procedure to learn (memory keeps facts: `noskill memory add`)"}
	}
	it := store.LearnItem{ID: ids.New(), Kind: "draft", RunIDs: []string{r.ID}, Reason: "asked", Pattern: r.ToolPattern}
	added, err := a.DB.Enqueue(ctx, it, time.Now(), 0)
	if err != nil {
		return it, err
	}
	if !added { // already queued or running
		items, _ := a.DB.ListLearning(ctx, 200)
		for _, x := range items {
			if x.Kind == "draft" && len(x.RunIDs) > 0 && x.RunIDs[0] == r.ID && (x.Status == "pending" || x.Status == "running") {
				return x, nil
			}
		}
	}
	return it, nil
}

// LearnNow works on one item in the foreground (`noskill learn` without --later): the user asked,
// so the idle rules don't apply, but the generation slot is still shared.
func (a *App) LearnNow(ctx context.Context, id string) (store.LearnItem, error) {
	it, err := a.DB.GetLearning(ctx, id)
	if err != nil {
		return it, err
	}
	if it.Status != "pending" {
		return it, &UserError{fmt.Sprintf("item %s is %s", id, it.Status)}
	}
	if ok, err := a.DB.ClaimLearningID(ctx, id, time.Now()); err != nil || !ok {
		if err == nil {
			err = &UserError{fmt.Sprintf("item %s was just taken by the background worker", id)}
		}
		return it, err
	}
	_, perr := a.processLearning(ctx, it, false)
	it, err = a.DB.GetLearning(context.WithoutCancel(ctx), id)
	if err == nil {
		err = perr
	}
	return it, err
}
