package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"noskill/internal/store"
)

func mustCron(t *testing.T, e string) *Cron {
	t.Helper()
	c, err := ParseCron(e)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCronNext(t *testing.T) {
	loc := time.UTC
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, loc) }
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"0 3 * * *", at(2026, 10, 15, 2, 59), at(2026, 10, 15, 3, 0)},
		{"0 3 * * *", at(2026, 10, 15, 3, 0), at(2026, 10, 16, 3, 0)}, // strictly after
		{"*/15 * * * *", at(2026, 10, 15, 3, 7), at(2026, 10, 15, 3, 15)},
		{"30 9 * * mon-fri", at(2026, 10, 16, 10, 0), at(2026, 10, 19, 9, 30)}, // Fri → Mon
		{"0 0 29 feb *", at(2026, 3, 1, 0, 0), at(2028, 2, 29, 0, 0)},
		{"@daily", at(2026, 10, 15, 12, 0), at(2026, 10, 16, 0, 0)},
		{"0 12 1 * 0", at(2026, 10, 2, 0, 0), at(2026, 10, 4, 12, 0)}, // day-of-month OR weekday (Sun 4th)
		{"0 0 * * 7", at(2026, 10, 15, 0, 0), at(2026, 10, 18, 0, 0)}, // 7 = Sunday
		{"5/20 * * * *", at(2026, 10, 15, 3, 6), at(2026, 10, 15, 3, 25)},
	}
	for _, c := range cases {
		if got := mustCron(t, c.expr).Next(c.from); !got.Equal(c.want) {
			t.Errorf("%s from %s = %s, want %s", c.expr, c.from, got, c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	if !mustCron(t, "0 0 31 2 *").Next(at(2026, 1, 1, 0, 0)).IsZero() {
		t.Error("31 feb never happens")
	}
}

func TestCronDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	// 2026-11-01 01:00–02:00 happens twice in New York; a 01:30 job runs once.
	c := mustCron(t, "30 1 * * *")
	first := c.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, ny))
	second := c.Next(first)
	if first.Hour() != 1 || second.Day() != 2 {
		t.Errorf("first %s, second %s", first, second)
	}
	// 2026-03-08 02:30 doesn't exist: skipped to the next day.
	if got := mustCron(t, "30 2 * * *").Next(time.Date(2026, 3, 8, 0, 0, 0, 0, ny)); got.Day() != 9 && !(got.Day() == 8 && got.Hour() == 3) {
		t.Errorf("spring forward: %s", got)
	}
}

func TestEvery(t *testing.T) {
	for in, want := range map[string]string{"30m": "every:30m", "6h": "every:6h", "1d": "every:1d", "90m": "every:1h30m", "1d12h": "every:1d12h"} {
		e, err := ParseEvery(in)
		if err != nil || e.String() != want {
			t.Errorf("%s → %v %v, want %s", in, e, err, want)
		}
		if back, err := ParseSchedule(e.String()); err != nil || back.String() != want {
			t.Errorf("round trip %s: %v %v", want, back, err)
		}
	}
	if _, err := ParseEvery("10s"); err == nil {
		t.Error("intervals under a minute are refused")
	}
}

// ---- scheduler ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fakeExec struct {
	mu       sync.Mutex
	starts   []string
	resumes  []string
	results  []Result // consumed in order; then done
	approval map[string]string
	block    chan struct{} // if set, Start waits for it or ctx
}

func (f *fakeExec) Start(ctx context.Context, j Job) Result {
	f.mu.Lock()
	f.starts = append(f.starts, j.Name)
	var r Result
	if len(f.results) > 0 {
		r, f.results = f.results[0], f.results[1:]
	} else {
		r = Result{Status: "done", RunID: "run-" + j.Name, Summary: "ok"}
	}
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return Result{Status: "cancelled", Err: ctx.Err()}
		}
	}
	return r
}

func (f *fakeExec) Resume(ctx context.Context, j Job, jr JobRun) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, jr.ApprovalID)
	return Result{Status: "done", RunID: "resumed", Summary: "continued"}
}

func (f *fakeExec) ApprovalStatus(ctx context.Context, id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approval[id]
}

type rig struct {
	s     *Scheduler
	st    *Store
	clk   *clock
	exec  *fakeExec
	notes []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g := &rig{st: &Store{DB: db}, clk: &clock{t: time.Date(2026, 10, 15, 2, 0, 0, 0, time.Local)}, exec: &fakeExec{approval: map[string]string{}}}
	var mu sync.Mutex
	g.s = &Scheduler{Store: g.st, Exec: g.exec, Now: g.clk.Now, Holder: "test", Backoff: []time.Duration{time.Minute},
		Notify: func(_ context.Context, j Job, jr JobRun, kind string) {
			mu.Lock()
			g.notes = append(g.notes, j.Name+":"+kind)
			mu.Unlock()
		}}
	return g
}

func (g *rig) add(t *testing.T, name, sched string, mod func(*Job)) Job {
	t.Helper()
	sc, err := ParseSchedule(sched)
	if err != nil {
		t.Fatal(err)
	}
	j := Job{Name: name, TargetKind: "prompt", Target: "hi", Schedule: sc.String(), Missed: "once", Overlap: "skip",
		Timeout: time.Hour, Retries: 1, Notify: []string{"os"}, Enabled: true, CreatedAt: g.clk.Now(), NextRunAt: sc.Next(g.clk.Now())}
	if mod != nil {
		mod(&j)
	}
	j, err = g.st.Add(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (g *rig) step() {
	g.s.Step(context.Background())
	g.s.Wait()
}

func (g *rig) statuses(t *testing.T, job string) []string {
	t.Helper()
	j, err := g.st.Get(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := g.st.Runs(context.Background(), j.ID, 0)
	var out []string
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i].Status)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunsWhenDueAndNotifies(t *testing.T) {
	g := newRig(t)
	g.add(t, "nightly", "cron:0 3 * * *", nil)
	g.step()
	if len(g.exec.starts) != 0 {
		t.Fatal("ran before its time")
	}
	g.clk.Add(time.Hour)
	g.step()
	g.step() // not twice for the same occurrence
	if len(g.exec.starts) != 1 || !eq(g.statuses(t, "nightly"), []string{"done"}) || !eq(g.notes, []string{"nightly:done"}) {
		t.Fatalf("starts %v, runs %v, notes %v", g.exec.starts, g.statuses(t, "nightly"), g.notes)
	}
	j, _ := g.st.Get(context.Background(), "nightly")
	if j.NextRunAt.Day() != 16 || j.NextRunAt.Hour() != 3 || j.LastRunID != "run-nightly" {
		t.Errorf("next %s last %s", j.NextRunAt, j.LastRunID)
	}
}

func TestMissedPolicy(t *testing.T) {
	g := newRig(t)
	g.add(t, "once", "every:1h", nil)
	g.add(t, "skip", "every:1h", func(j *Job) { j.Missed = "skip" })
	g.clk.Add(10 * time.Hour) // the machine slept through 10 occurrences
	g.step()
	if got := g.statuses(t, "once"); !eq(got, []string{"done"}) {
		t.Errorf("once: %v (several missed runs collapse into one)", got)
	}
	if got := g.statuses(t, "skip"); !eq(got, []string{"missed"}) {
		t.Errorf("skip: %v", got)
	}
	for _, n := range []string{"once", "skip"} {
		j, _ := g.st.Get(context.Background(), n)
		if !j.NextRunAt.After(g.clk.Now()) || j.NextRunAt.Sub(g.clk.Now()) > time.Hour {
			t.Errorf("%s next %s, now %s", n, j.NextRunAt, g.clk.Now())
		}
	}
}

func TestRetryWithBackoffThenFail(t *testing.T) {
	g := newRig(t)
	g.exec.results = []Result{{Status: "failed", Err: errors.New("backend down")}, {Status: "failed", Err: errors.New("still down")}}
	g.add(t, "flaky", "every:1d", nil) // retries: 1
	g.clk.Add(24 * time.Hour)
	g.step()
	g.step() // retry not due yet
	if len(g.exec.starts) != 1 || len(g.notes) != 0 {
		t.Fatalf("starts %v notes %v", g.exec.starts, g.notes)
	}
	g.clk.Add(time.Minute)
	g.step()
	if got := g.statuses(t, "flaky"); !eq(got, []string{"failed", "failed"}) || !eq(g.notes, []string{"flaky:failed"}) {
		t.Fatalf("runs %v notes %v", got, g.notes)
	}
	g.clk.Add(time.Minute)
	g.step()
	if len(g.exec.starts) != 2 {
		t.Errorf("no more retries after the last: %v", g.exec.starts)
	}
}

func TestApprovalParksAndResumes(t *testing.T) {
	g := newRig(t)
	g.exec.results = []Result{{Status: "waiting", ApprovalID: "A1", RunID: "r1"}}
	g.add(t, "mover", "every:1h", nil)
	g.add(t, "other", "every:1h", nil)
	g.clk.Add(time.Hour)
	g.exec.approval["A1"] = "pending"
	g.step()
	g.step()
	if got := g.statuses(t, "mover"); !eq(got, []string{"waiting"}) {
		t.Fatalf("mover %v", got)
	}
	if got := g.statuses(t, "other"); !eq(got, []string{"done"}) {
		t.Fatalf("a waiting job must not block others: %v", got)
	}
	if len(g.notes) != 1 { // only other:done; the approval notifies for mover
		t.Errorf("notes %v", g.notes)
	}
	g.exec.approval["A1"] = "approved"
	g.step()
	if got := g.statuses(t, "mover"); !eq(got, []string{"done"}) || !eq(g.exec.resumes, []string{"A1"}) {
		t.Fatalf("after approve: %v resumes %v", got, g.exec.resumes)
	}
	// A skipped approval ends the run as declined.
	g.exec.results = []Result{{Status: "waiting", ApprovalID: "A2"}}
	g.clk.Add(time.Hour)
	g.exec.approval["A2"] = "skipped"
	g.step()
	g.step()
	if got := g.statuses(t, "mover"); got[len(got)-1] != "declined" {
		t.Errorf("after skip: %v", got)
	}
}

func TestOverlapStopAndTimeout(t *testing.T) {
	g := newRig(t)
	g.exec.block = make(chan struct{})
	g.add(t, "slow", "every:1h", func(j *Job) { j.Retries = 0 })
	g.clk.Add(time.Hour)
	g.s.Step(context.Background()) // starts, blocks
	g.clk.Add(time.Hour)
	g.s.Step(context.Background()) // next occurrence while still running
	if n, err := g.st.RequestStop(context.Background(), "slow", g.clk.Now()); err != nil || n != 1 {
		t.Fatalf("stop: %d %v", n, err)
	}
	g.s.Step(context.Background())
	g.s.Wait()
	if got := g.statuses(t, "slow"); !eq(got, []string{"stopped", "overlap"}) {
		t.Fatalf("runs %v", got)
	}

	g.add(t, "hang", "every:1h", func(j *Job) { j.Timeout = time.Second; j.Retries = 0 }) // timeouts are whole seconds
	g.clk.Add(time.Hour)
	g.step()
	if got := g.statuses(t, "hang"); !eq(got, []string{"timeout"}) || g.notes[len(g.notes)-1] != "hang:failed" {
		t.Errorf("runs %v notes %v", got, g.notes)
	}
}

func TestRebootRecoversInterruptedRun(t *testing.T) {
	g := newRig(t)
	j := g.add(t, "job", "every:1h", nil)
	// A previous daemon died mid-run and its lease ran out.
	before := g.clk.Now().Add(-time.Minute)
	g.st.insertRun(context.Background(), JobRun{ID: "old", JobID: j.ID, ScheduledFor: before, StartedAt: before, Status: StatusRunning})
	g.st.DB.ExecContext(context.Background(), `INSERT INTO leases VALUES ('scheduler', 'dead', 0)`)
	g.step()
	got := g.statuses(t, "job")
	if !eq(got, []string{"interrupted", "done"}) {
		t.Fatalf("runs %v", got)
	}
}

func TestOnlyOneSchedulerHoldsTheLease(t *testing.T) {
	g := newRig(t)
	j := g.add(t, "job", "every:1h", nil)
	other := &Scheduler{Store: g.st, Exec: g.exec, Now: g.clk.Now, Holder: "other"}
	g.step() // takes the lease
	g.st.RunNow(context.Background(), j.ID, g.clk.Now())
	other.Step(context.Background())
	other.Wait()
	if len(g.exec.starts) != 0 {
		t.Fatal("a second process must not run jobs while the lease is held")
	}
	g.step()
	if len(g.exec.starts) != 1 {
		t.Fatal("holder runs it")
	}
}

func TestPauseResumeAndRunNow(t *testing.T) {
	g := newRig(t)
	j := g.add(t, "job", "cron:0 3 * * *", nil)
	ctx := context.Background()
	g.st.SetEnabled(ctx, j.ID, false, time.Time{})
	g.clk.Add(48 * time.Hour)
	g.step()
	if len(g.exec.starts) != 0 {
		t.Fatal("paused job ran")
	}
	sc, _ := ParseSchedule(j.Schedule)
	g.st.SetEnabled(ctx, j.ID, true, sc.Next(g.clk.Now()))
	g.step()
	if len(g.exec.starts) != 0 {
		t.Fatal("resuming must not replay what was missed while paused")
	}
	g.st.RunNow(ctx, j.ID, g.clk.Now())
	g.step()
	if len(g.exec.starts) != 1 {
		t.Fatal("run now")
	}
	after, _ := g.st.Get(ctx, "job")
	if !after.NextRunAt.Equal(sc.Next(g.clk.Now())) {
		t.Errorf("run now must keep the schedule: next %s", after.NextRunAt)
	}
}
