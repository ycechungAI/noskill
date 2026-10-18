package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"noskill/internal/notify"
	"noskill/internal/scheduler"
)

// The weekly evaluation (LEARNING.md "Evaluation", [learning] eval_schedule): the learning worker
// runs the whole suite on the model you use most, compares it with the last run, and when a task
// type fell while learned skills changed, puts those skills back one version and says so.

const (
	evalAtKey    = "learning.eval_at"    // when the last weekly run started
	evalTriedKey = "learning.eval_tried" // when one was last attempted (a pre-empted run isn't retried at once)
	evalRetry    = 6 * time.Hour
)

func (a *App) evalDue(ctx context.Context) bool {
	sched, err := scheduler.ParseSchedule("cron:" + a.Cfg.Learning.EvalSchedule)
	if err != nil {
		return false
	}
	now := time.Now()
	stamp := func(key string) time.Time {
		v, _ := a.DB.GetState(ctx, key)
		t, _ := time.Parse(time.RFC3339, v)
		return t
	}
	last := stamp(evalAtKey)
	if last.IsZero() {
		a.DB.SetState(ctx, evalAtKey, now.Format(time.RFC3339), now) // the first run is the next scheduled time
		return false
	}
	if tried := stamp(evalTriedKey); !tried.IsZero() && now.Sub(tried) < evalRetry {
		return false
	}
	return !sched.Next(last).After(now)
}

// EvalWeekly runs the suite as background work. It returns what it did for the worker's log.
func (a *App) EvalWeekly(ctx context.Context) (string, error) {
	now := time.Now()
	a.DB.SetState(ctx, evalTriedKey, now.Format(time.RFC3339), now)
	model, _ := a.DB.TopDriverModel(ctx, now.AddDate(0, 0, -30))
	if _, err := a.FindModel(ctx, model); model == "" || err != nil {
		var cerr error
		if model, cerr = a.CheapestChat(ctx); cerr != nil {
			return "", cerr
		}
	}
	tasks, err := a.EvalTasks(nil, nil, false)
	if err != nil {
		return "", err
	}

	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	release, lctx, err := a.acquire(lctx, 0, true)
	if err != nil {
		return "", err
	}
	defer release()
	a.mu.Lock()
	a.learnCancel = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.learnCancel = nil
		a.mu.Unlock()
	}()

	g, err := a.EvalGate(lctx, EvalOptions{Model: model, Tasks: tasks, Suite: "all", Note: "weekly"}, true)
	if err != nil {
		if lctx.Err() != nil && ctx.Err() == nil {
			return "", nil // pre-empted by a request; tried again after evalRetry
		}
		return "", err
	}
	a.DB.SetState(ctx, evalAtKey, now.Format(time.RFC3339), time.Now())
	out := g.Outcome
	msg := fmt.Sprintf("eval on %s: %d/%d passed", out.Run.Model, out.Run.Passed, out.Run.Tasks)
	if len(out.Regressions) == 0 {
		return msg, nil
	}
	var parts []string
	for _, r := range out.Regressions {
		parts = append(parts, r.Type)
	}
	body := fmt.Sprintf("%s. Fell: %s.", msg, strings.Join(parts, ", "))
	if len(g.RolledBack) > 0 {
		var rb []string
		for n, v := range g.RolledBack {
			rb = append(rb, n+" → "+v)
		}
		sort.Strings(rb)
		body += " Skills put back: " + strings.Join(rb, ", ") + "."
	}
	if len(g.Kept) > 0 {
		body += " New learned skills in tasks that fell (no older version; `noskill skills disable` if they're the cause): " + strings.Join(g.Kept, ", ") + "."
	}
	if len(g.RolledBack) == 0 && len(g.Kept) == 0 {
		body += " No learned skill took part in those tasks, so nothing was put back."
	}
	body += " `noskill eval report` has the details."
	a.Hub.Send(ctx, notify.Message{ID: "eval-" + out.Run.ID, Title: "noskill: evaluation regressed", Body: body})
	return body, nil
}
