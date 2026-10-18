package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"noskill/internal/agent"
	"noskill/internal/backends"
	"noskill/internal/memory"
	"noskill/internal/orchestrator"
	"noskill/internal/store"
)

// Outcome capture v1 (FR-L1, LEARNING.md §1): explicit ratings, hard checks and the "asked it
// again" signal. The decision-model and LLM judges arrive with the learning loop (Phase 2).

type reaskEntry struct {
	fp    string
	runID string
	at    time.Time
}

var reasks = struct {
	sync.Mutex
	last map[string]reaskEntry
}{last: map[string]reaskEntry{}}

const reaskWindow = 10 * time.Minute

// noteReask: the same request from the same client within ten minutes means the first answer
// probably didn't do the job. It marks the earlier run failed with low confidence unless the user
// already rated it.
func (a *App) noteReask(ctx context.Context, client, text, runID string, now time.Time) {
	fp := fingerprint(text)
	if len(strings.Fields(text)) < 3 { // "yes", "ok": not a repeated request
		return
	}
	reasks.Lock()
	prev, ok := reasks.last[client]
	reasks.last[client] = reaskEntry{fp: fp, runID: runID, at: now}
	reasks.Unlock()
	if ok && prev.fp == fp && now.Sub(prev.at) <= reaskWindow {
		if err := a.DB.SetOutcome(ctx, prev.runID, "fail", 0.6, "implicit"); err == nil {
			a.traceOutcome(ctx, prev.runID, "fail", 0.6, "implicit", "the same request was asked again")
		}
	}
}

// captureOutcome sets what can be known at the end of a run without asking anyone.
func (a *App) captureOutcome(ctx context.Context, row store.Run, res *agent.Result) {
	set := func(outcome string, conf float64, source, note string) {
		if err := a.DB.SetOutcome(ctx, row.ID, outcome, conf, source); err == nil {
			a.traceOutcome(ctx, row.ID, outcome, conf, source, note)
		}
	}
	switch row.Status {
	case "failed":
		set("fail", 1, "check", "the run ended with an error")
		return
	case "cancelled":
		set("fail", 0.7, "implicit", "the run was cancelled")
		return
	case "waiting":
		return
	}
	if res == nil {
		return
	}
	if res.Stop == agent.StopMaxSteps || res.Stop == agent.StopMaxTokens {
		set("fail", 0.8, "check", "the run hit its "+string(res.Stop)+" limit before finishing")
		return
	}
	// Post-condition check: everything the run claims to have done to files is really there.
	entries, err := a.DB.UndoEntries(ctx, row.ID)
	if err != nil || len(entries) == 0 {
		return
	}
	for _, e := range entries {
		if e.To == "" {
			continue
		}
		if _, err := os.Lstat(e.To); err != nil {
			set("fail", 0.9, "check", fmt.Sprintf("%s %s: the result is missing", e.Op, e.To))
			return
		}
	}
	set("success", 0.8, "check", fmt.Sprintf("%d file change(s), all present", len(entries)))
}

// traceOutcome follows every recorded outcome: it notes it in the trace and updates the trials of
// learned skills the run used.
func (a *App) traceOutcome(ctx context.Context, runID, outcome string, conf float64, source, note string) {
	defer a.trialOutcome(ctx, runID)
	defer a.banditOutcome(ctx, runID)
	run, err := a.DB.GetRun(ctx, runID)
	if err != nil || run.TracePath == "" {
		return
	}
	agent.AppendEvent(run.TracePath, agent.Event{Time: time.Now(), Kind: agent.EventOutcome,
		Tool: source, Content: outcome, Note: fmt.Sprintf("confidence %.2f: %s", conf, note)}, a.secretValuesAll())
}

func (a *App) secretValuesAll() []string { return a.secretValues(a.Skills.All()) }

// Rate records the user's own verdict on a run: good, partial or bad. "last" rates the most
// recent top-level run. An explicit rating always wins over a guess.
func (a *App) Rate(ctx context.Context, runID, rating, note string) (store.Run, error) {
	outcome := map[string]string{"good": "success", "partial": "partial", "bad": "fail"}[rating]
	if outcome == "" {
		return store.Run{}, fmt.Errorf("rating must be good, partial or bad")
	}
	var run store.Run
	var err error
	if runID == "" || runID == "last" {
		run, err = a.DB.LatestRun(ctx, "")
	} else {
		run, err = a.DB.GetRun(ctx, runID)
	}
	if err != nil {
		return run, fmt.Errorf("no such run: %w", err)
	}
	if run.NoLearn {
		return run, errors.New("that run used --no-learn, so no signals are recorded for it")
	}
	if err := a.DB.SetOutcome(ctx, run.ID, outcome, 1, "explicit"); err != nil {
		return run, err
	}
	a.traceOutcome(ctx, run.ID, outcome, 1, "explicit", note)
	run, err = a.DB.GetRun(ctx, run.ID)
	if err == nil && outcome == "success" && a.Cfg.Learning.Enabled {
		a.MineRun(ctx, run) // the miner may have looked at this run before the rating
	}
	return run, err
}

// learnFacts stores durable facts from the user's latest message (FR-L6). Only the user's own
// words are read, never tool output or fetched pages.
func (a *App) learnFacts(ctx context.Context, msgs []backends.Message) {
	text, _ := lastUser(msgs)
	for _, c := range memory.Extract(text) {
		a.Memory.Add(ctx, c.Key, c.Value, c.Source, c.Confidence)
	}
}

// banditOutcome counts a rated run in the routing bandit (FR-O4): the model that drove it gets the
// run's reward for its task type and difficulty. A later rating replaces the earlier one.
func (a *App) banditOutcome(ctx context.Context, runID string) {
	run, err := a.DB.GetRun(ctx, runID)
	if err != nil || run.NoLearn || run.Pinned || run.TaskType == "" || run.Outcome == "" {
		return
	}
	reward, ok := orchestrator.Reward(run.Outcome, run.OutcomeConf, float64(run.Ms)/1000, a.Cfg.Orchestrator.Preset)
	if !ok {
		return
	}
	if err := a.DB.SetReward(ctx, run, reward, time.Now()); err != nil {
		slog.Warn("bandit update failed", "run", runID, "err", err)
	}
}
