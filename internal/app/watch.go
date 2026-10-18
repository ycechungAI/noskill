package app

import (
	"context"
	"fmt"
	"time"

	"noskill/internal/decide"
	"noskill/internal/scheduler"
	"noskill/internal/watch"
)

// The watcher's service (internal/tools/builtin/watch.go): watches live in state.db, and each one
// is a scheduler job, so `noskill schedule list` shows it and `jobs` can stop it.

const watcherSkill = "watcher"

func watchJobName(name string) string { return "watch: " + name }

func (s *runServices) WatchAdd(ctx context.Context, w watch.Watch) (watch.Watch, error) {
	sched, err := scheduler.ParseSchedule(w.Schedule)
	if err != nil {
		return w, err
	}
	w.Schedule = sched.String()
	w.CreatedAt = time.Now()
	if err := s.a.DB.AddWatch(ctx, w); err != nil {
		return w, err
	}
	sc := s.a.Cfg.Scheduler
	j, err := (&scheduler.Store{DB: s.a.DB}).Add(ctx, scheduler.Job{
		Name: watchJobName(w.Name), TargetKind: "skill", Target: watcherSkill,
		Args:     scheduler.Args{Prompt: fmt.Sprintf("Run the watch %q: call watch_check, keep what's relevant to the topic, and report what's new.", w.Name)},
		Schedule: w.Schedule, Missed: sc.MissedPolicy, Overlap: "skip", Timeout: sc.DefaultTimeout.Duration, Retries: sc.Retries,
		Notify: []string{"os"}, Enabled: true, NextRunAt: sched.Next(time.Now()), CreatedAt: time.Now(),
	})
	if err != nil {
		s.a.DB.RemoveWatch(ctx, w.Name) // no half-made watch without its job
		return w, err
	}
	w.JobID = j.ID
	return w, s.a.DB.SetWatchJob(ctx, w.Name, j.ID)
}

func (s *runServices) WatchGet(ctx context.Context, name string) (watch.Watch, error) {
	return s.a.DB.GetWatch(ctx, name)
}

func (s *runServices) WatchList(ctx context.Context) ([]watch.Watch, error) {
	return s.a.DB.ListWatches(ctx)
}

func (s *runServices) WatchRemove(ctx context.Context, name string) error {
	jobID, err := s.a.DB.RemoveWatch(ctx, name)
	if err != nil {
		return err
	}
	if jobID != "" {
		(&scheduler.Store{DB: s.a.DB}).Remove(ctx, jobID)
	}
	return nil
}

func (s *runServices) WatchSeen(ctx context.Context, name string) (map[string]bool, error) {
	return s.a.DB.SeenItems(ctx, name)
}

func (s *runServices) WatchRecord(ctx context.Context, name string, items []watch.Item, etag, lastModified string) error {
	return s.a.DB.RecordCheck(ctx, name, items, etag, lastModified, time.Now())
}

// WatchRelevant asks the decision model (or the chat model when there is none) whether each new
// item matters to the watch's topic. It returns nil when nothing can decide.
func (s *runServices) WatchRelevant(ctx context.Context, topic string, items []watch.Item) ([]float64, error) {
	models, _ := s.a.Models(ctx) // an unreachable backend just means no decider below
	d := s.a.decider(ctx, models)
	if d == nil {
		return nil, nil
	}
	q := map[string]decide.Question{"relevant": {Type: decide.Noul, Instructions: "Is this new item relevant to the user's topic?"}}
	out := make([]float64, len(items))
	for i, it := range items {
		ans, err := d.Decide(ctx, map[string]any{"topic": topic, "item": it.Title + "\n" + it.Summary}, q)
		if err != nil {
			return nil, err
		}
		out[i] = ans.Answers["relevant"].Noul
	}
	return out, nil
}
