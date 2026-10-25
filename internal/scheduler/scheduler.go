// Package scheduler runs jobs on a schedule inside the daemon (FR-J1–J4): cron or interval
// schedules, a missed-run policy for when the machine was off or asleep, an overlap policy, a
// timeout, retries with backoff, notifications, and runs that wait for approvals without
// blocking other jobs. Jobs are rows in state.db; the CLI edits them and the daemon picks the
// changes up on its next tick, so no IPC is needed.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"noskill/internal/ids"
)

// Result is what a job's run came to.
type Result struct {
	RunID      string
	Status     string // done | failed | waiting | cancelled
	ApprovalID string // when waiting
	Summary    string // the start of the answer
	Err        error
	// Quiet: nothing to report, so a finished run sends no notification (disk-watch on a good night).
	Quiet bool
}

// Executor runs jobs: the engine in the daemon, a fake in tests.
type Executor interface {
	Start(ctx context.Context, j Job) Result
	// Resume continues a run parked on an approval that is now approved.
	Resume(ctx context.Context, j Job, jr JobRun) Result
	// ApprovalStatus is pending, approved, skipped or expired ("" if the item is gone).
	ApprovalStatus(ctx context.Context, id string) string
}

// Event kinds passed to Notify.
const (
	NotifyDone     = "done"
	NotifyFailed   = "failed" // final: no retries left
	NotifyDeclined = "declined"
)

type Scheduler struct {
	Store *Store
	Exec  Executor
	// Notify reports a finished run on the job's channels. Waiting runs aren't reported here: the
	// approval itself sends the notification (FR-A6).
	Notify        func(ctx context.Context, j Job, jr JobRun, kind string)
	MaxConcurrent int           // [scheduler] max_concurrent_jobs; runs waiting on approvals don't count
	Tick          time.Duration // default 5 s: how fast CLI changes and `jobs stop` take effect
	Grace         time.Duration // later than this past its time = missed (default 2 minutes)
	Backoff       []time.Duration
	Now           func() time.Time
	Log           func(format string, args ...any)
	Holder        string  // lease holder id; default host+pid
	Check         Checker // preconditions (FR-J5); nil = this machine (PlatformChecker)

	mu       sync.Mutex
	active   map[string]context.CancelFunc // job run id → cancel
	stopping map[string]bool
	wg       sync.WaitGroup
	base     context.Context
	// waiting holds the occurrence each job is waiting on for its preconditions, so the wait isn't
	// mistaken for a missed run once it passes the grace period.
	waiting      map[string]time.Time
	defaultCheck *PlatformChecker
}

// DefaultBackoff is the wait before each retry of a failed run.
var DefaultBackoff = []time.Duration{time.Minute, 5 * time.Minute, 25 * time.Minute}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

func (s *Scheduler) init(ctx context.Context) {
	if s.active == nil {
		s.active, s.stopping = map[string]context.CancelFunc{}, map[string]bool{}
	}
	if s.Tick <= 0 {
		s.Tick = 5 * time.Second
	}
	if s.Grace <= 0 {
		s.Grace = 2 * time.Minute
	}
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 1
	}
	if s.Backoff == nil {
		s.Backoff = DefaultBackoff
	}
	if s.Holder == "" {
		h, _ := os.Hostname()
		s.Holder = fmt.Sprintf("%s/%d/%s", h, os.Getpid(), ids.New())
	}
	if s.base == nil {
		s.base = ctx
	}
}

// Run ticks until ctx ends, then cancels running jobs (they're marked interrupted and retried
// when the daemon comes back) and gives up the lease.
func (s *Scheduler) Run(ctx context.Context) error {
	s.init(ctx)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		s.Step(ctx)
		select {
		case <-ctx.Done():
			s.Wait()
			s.Store.release(context.WithoutCancel(ctx), s.Holder)
			return nil
		case <-t.C:
		}
	}
}

// Wait blocks until every run this scheduler started has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Step is one pass: take the lease, apply stop requests, resume answered approvals, start due
// jobs. Exported for tests, which drive time by hand.
func (s *Scheduler) Step(ctx context.Context) {
	s.init(ctx)
	now := s.now()
	held, fresh, err := s.Store.lease(ctx, s.Holder, now, max(3*s.Tick, 30*time.Second))
	if err != nil {
		s.logf("scheduler: lease: %v", err)
		return
	}
	if !held {
		return // another noskill process schedules
	}
	if fresh {
		s.recover(ctx, now)
	}
	s.applyStops(ctx, now)
	s.resumeAnswered(ctx, now)
	s.startDue(ctx, now)
}

// recover cleans up after a process that died mid-run (a reboot, a crash): its runs are marked
// interrupted and retried now, without using up a retry.
func (s *Scheduler) recover(ctx context.Context, now time.Time) {
	list, err := s.Store.interruptOrphans(ctx, now, func(id string) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.active[id]
		return ok
	})
	if err != nil {
		s.logf("scheduler: recover: %v", err)
	}
	for _, jr := range list {
		s.logf("scheduler: %s was interrupted; running it again", jr.JobName)
		if j, err := s.Store.Get(ctx, jr.JobID); err == nil {
			s.Store.scheduleRetry(ctx, j.ID, now, jr.Attempt)
		}
	}
}

func (s *Scheduler) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

func (s *Scheduler) applyStops(ctx context.Context, now time.Time) {
	list, err := s.Store.runsWithStatus(ctx, StatusStopping)
	if err != nil {
		return
	}
	for _, jr := range list {
		s.mu.Lock()
		cancel, ok := s.active[jr.ID]
		if ok {
			s.stopping[jr.ID] = true
		}
		s.mu.Unlock()
		if ok {
			cancel()
			continue
		}
		// Not running here (its process is gone): just record it.
		jr.Status, jr.EndedAt, jr.Error = StatusStopped, now, "stopped by you"
		s.Store.updateRun(ctx, jr)
	}
}

// resumeAnswered continues runs whose approval was answered: approved ones pick up where they
// stopped, skipped or expired ones end as declined.
func (s *Scheduler) resumeAnswered(ctx context.Context, now time.Time) {
	list, err := s.Store.runsWithStatus(ctx, StatusWaiting)
	if err != nil {
		return
	}
	for _, jr := range list {
		switch s.Exec.ApprovalStatus(ctx, jr.ApprovalID) {
		case "pending":
			continue
		case "approved":
			if s.running() >= s.MaxConcurrent {
				continue // next tick
			}
			j, err := s.Store.Get(ctx, jr.JobID)
			if err != nil || !s.Store.casStatus(ctx, jr.ID, StatusWaiting, StatusRunning) {
				continue
			}
			jr.Status = StatusRunning
			s.logf("scheduler: %s approved; continuing", j.Name)
			s.launch(j, jr, func(rctx context.Context) Result { return s.Exec.Resume(rctx, j, jr) })
		default: // skipped, expired, or gone
			if !s.Store.casStatus(ctx, jr.ID, StatusWaiting, StatusDeclined) {
				continue
			}
			jr.Status, jr.EndedAt = StatusDeclined, now
			jr.Error = "the approval it waited for was skipped or expired"
			s.Store.updateRun(ctx, jr)
			if j, err := s.Store.Get(ctx, jr.JobID); err == nil {
				s.notify(ctx, j, jr, NotifyDeclined)
			}
		}
	}
}

func (s *Scheduler) startDue(ctx context.Context, now time.Time) {
	due, err := s.Store.Due(ctx, now)
	if err != nil {
		s.logf("scheduler: %v", err)
		return
	}
	for _, j := range due {
		sched, err := ParseSchedule(j.Schedule)
		if err != nil {
			s.logf("scheduler: job %s: %v", j.Name, err)
			continue
		}
		regular := !j.NextRunAt.IsZero() && !j.NextRunAt.After(now)
		occurrence := j.RetryAt
		if regular {
			occurrence = j.NextRunAt
		}
		next := j.NextRunAt
		if regular {
			next = nextAfter(sched, j.NextRunAt, now)
		}
		busy, err := s.Store.hasActive(ctx, j.ID)
		if err != nil {
			continue
		}
		if busy {
			if j.Overlap == "queue" {
				continue // stays due until the previous run ends
			}
			if ok, _ := s.Store.claim(ctx, j, next, !regular); ok {
				s.record(ctx, j, occurrence, now, StatusOverlap, "the previous run was still going")
			}
			continue
		}
		if regular && now.Sub(j.NextRunAt) > s.Grace && j.Missed == "skip" && !s.waitedFor(j) {
			if ok, _ := s.Store.claim(ctx, j, next, false); ok {
				s.record(ctx, j, occurrence, now, StatusMissed, "noskill wasn't running at "+j.NextRunAt.Format("Jan 2 15:04"))
			}
			continue
		}
		if why := s.unmet(ctx, j, now); why != "" {
			// FR-J5: wait until the precondition holds or the next occurrence comes, then skip.
			if until := sched.Next(j.NextRunAt); regular && (until.IsZero() || !now.Before(until)) {
				// The occurrence that has just come gets its own wait (several passed ones collapse).
				if !until.IsZero() {
					next = latest(sched, until, now)
				}
				if ok, _ := s.Store.claim(ctx, j, next, false); ok {
					s.record(ctx, j, occurrence, now, StatusUnmet, why)
				}
				s.setWaiting(j.ID, time.Time{})
			} else if s.setWaiting(j.ID, occurrence) {
				s.logf("scheduler: %s waits: %s", j.Name, why)
			}
			continue
		}
		if s.running() >= s.MaxConcurrent {
			return // the rest wait for the next tick
		}
		ok, err := s.Store.claim(ctx, j, next, !regular)
		if err != nil || !ok {
			continue
		}
		attempt := 0
		if !regular {
			attempt = j.Attempt
		}
		jr := JobRun{ID: ids.New(), JobID: j.ID, JobName: j.Name, ScheduledFor: occurrence, StartedAt: now, Status: StatusRunning, Attempt: attempt}
		if err := s.Store.insertRun(ctx, jr); err != nil {
			s.logf("scheduler: %v", err)
			continue
		}
		s.setWaiting(j.ID, time.Time{})
		s.logf("scheduler: starting %s", j.Name)
		s.launch(j, jr, func(rctx context.Context) Result { return s.Exec.Start(rctx, j) })
	}
}

// nextAfter is the first occurrence after now, counted from the scheduled time: several missed
// occurrences collapse into the one run that happens now.
func nextAfter(sched Schedule, from, now time.Time) time.Time {
	t := from
	for i := 0; i < 100000; i++ {
		t = sched.Next(t)
		if t.IsZero() || t.After(now) {
			return t
		}
	}
	return sched.Next(now)
}

// setWaiting notes (or with a zero time clears) the occurrence job id waits on. It reports whether
// that is new, so the wait is logged once.
func (s *Scheduler) setWaiting(id string, occurrence time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if occurrence.IsZero() {
		delete(s.waiting, id)
		return false
	}
	if s.waiting == nil {
		s.waiting = map[string]time.Time{}
	}
	if s.waiting[id].Equal(occurrence) {
		return false
	}
	s.waiting[id] = occurrence
	return true
}

func (s *Scheduler) waitedFor(j Job) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiting[j.ID].Equal(j.NextRunAt)
}

// latest is the last occurrence from t onward that isn't after now (t itself if the next is later).
func latest(sched Schedule, t, now time.Time) time.Time {
	for i := 0; i < 100000; i++ {
		n := sched.Next(t)
		if n.IsZero() || n.After(now) {
			return t
		}
		t = n
	}
	return t
}

func (s *Scheduler) record(ctx context.Context, j Job, occurrence, now time.Time, status, why string) {
	s.logf("scheduler: %s %s: %s", j.Name, status, why)
	s.Store.insertRun(ctx, JobRun{ID: ids.New(), JobID: j.ID, ScheduledFor: occurrence, StartedAt: now, EndedAt: now, Status: status, Error: why})
}

func (s *Scheduler) launch(j Job, jr JobRun, run func(context.Context) Result) {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	rctx, cancel := context.WithTimeout(s.base, timeout)
	s.mu.Lock()
	s.active[jr.ID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		res := run(rctx)
		s.finish(j, jr, res, rctx)
	}()
}

func (s *Scheduler) finish(j Job, jr JobRun, res Result, rctx context.Context) {
	ctx := context.WithoutCancel(s.base)
	s.mu.Lock()
	stopped := s.stopping[jr.ID]
	delete(s.active, jr.ID)
	delete(s.stopping, jr.ID)
	s.mu.Unlock()

	now := s.now()
	jr.RunID, jr.ApprovalID, jr.Result, jr.EndedAt = res.RunID, res.ApprovalID, res.Summary, now
	if res.Err != nil {
		jr.Error = res.Err.Error()
	}
	switch {
	case stopped:
		jr.Status, jr.Error = StatusStopped, "stopped by you"
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		jr.Status, jr.Error = StatusTimeout, fmt.Sprintf("took longer than %s", FormatDuration(j.Timeout))
	case s.base.Err() != nil:
		jr.Status = StatusInterrupted
		if jr.Error == "" {
			jr.Error = "noskill stopped while this ran"
		}
	case res.Status == "waiting":
		jr.Status, jr.EndedAt = StatusWaiting, time.Time{}
	case res.Status == "done":
		jr.Status = StatusDone
	default:
		jr.Status = StatusFailed
	}
	if err := s.Store.updateRun(ctx, jr); err != nil {
		s.logf("scheduler: %v", err)
	}
	s.Store.setLastRun(ctx, j.ID, res.RunID)
	s.logf("scheduler: %s %s", j.Name, jr.Status)

	switch jr.Status {
	case StatusDone:
		if !res.Quiet {
			s.notify(ctx, j, jr, NotifyDone)
		}
	case StatusInterrupted:
		// Retried when the daemon starts again (recover), without using up a retry.
		s.Store.scheduleRetry(ctx, j.ID, now, jr.Attempt)
	case StatusFailed, StatusTimeout:
		if jr.Attempt < j.Retries {
			wait := s.Backoff[min(jr.Attempt, len(s.Backoff)-1)]
			s.Store.scheduleRetry(ctx, j.ID, now.Add(wait), jr.Attempt+1)
			s.logf("scheduler: %s will retry in %s (%d of %d)", j.Name, FormatDuration(wait), jr.Attempt+1, j.Retries)
			return
		}
		s.notify(ctx, j, jr, NotifyFailed)
	}
}

func (s *Scheduler) notify(ctx context.Context, j Job, jr JobRun, kind string) {
	if s.Notify != nil && len(j.Notify) > 0 {
		s.Notify(ctx, j, jr, kind)
	}
}
