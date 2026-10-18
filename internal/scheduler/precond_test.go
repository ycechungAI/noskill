package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeCheck struct {
	mu  sync.Mutex
	off map[string]bool // precondition → doesn't hold
}

func (f *fakeCheck) Check(_ context.Context, cond string, _ time.Time) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off[cond] {
		return false, "not now"
	}
	return true, ""
}

func (f *fakeCheck) set(cond string, off bool) { f.mu.Lock(); f.off[cond] = off; f.mu.Unlock() }

func TestParsePrecondition(t *testing.T) {
	for _, ok := range []string{"volume:RED", "ac-power", "unmetered", "window:22:00-06:00", "window:09:30-17:00"} {
		if got, err := ParsePrecondition(ok); err != nil || got != ok {
			t.Errorf("%s: %q %v", ok, got, err)
		}
	}
	for _, bad := range []string{"volume:", "volume:RED/x", "ac-power:yes", "battery", "window:9-17", "window:10:00-10:00", "window:25:00-01:00"} {
		if _, err := ParsePrecondition(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestInWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 15, h, m, 0, 0, time.Local) }
	cases := []struct {
		w    string
		t    time.Time
		want bool
	}{
		{"22:00-06:00", at(23, 0), true}, {"22:00-06:00", at(3, 0), true}, {"22:00-06:00", at(6, 0), false},
		{"22:00-06:00", at(12, 0), false}, {"09:00-17:00", at(9, 0), true}, {"09:00-17:00", at(17, 0), false},
	}
	for _, c := range cases {
		if InWindow(c.w, c.t) != c.want {
			t.Errorf("%s at %s: want %v", c.w, c.t.Format("15:04"), c.want)
		}
	}
}

func TestPreconditionWaitsThenRuns(t *testing.T) {
	g := newRig(t)
	chk := &fakeCheck{off: map[string]bool{"volume:RED": true}}
	g.s.Check, g.s.Grace = chk, 2*time.Minute
	g.add(t, "file", "every:6h", func(j *Job) { j.Preconditions = []string{"volume:RED", "ac-power"}; j.Missed = "skip" })
	j, _ := g.st.Get(context.Background(), "file")
	if !eq(j.Preconditions, []string{"volume:RED", "ac-power"}) {
		t.Fatalf("stored %v", j.Preconditions)
	}
	g.clk.Add(6 * time.Hour)
	g.step()
	g.clk.Add(time.Hour) // still waiting, well past the grace period: not "missed"
	g.step()
	if len(g.exec.starts) != 0 || len(g.statuses(t, "file")) != 0 {
		t.Fatalf("ran or recorded while the drive was away: %v %v", g.exec.starts, g.statuses(t, "file"))
	}
	chk.set("volume:RED", false) // the drive is plugged in
	g.step()
	if len(g.exec.starts) != 1 || !eq(g.statuses(t, "file"), []string{"done"}) {
		t.Fatalf("starts %v runs %v", g.exec.starts, g.statuses(t, "file"))
	}
}

func TestPreconditionSkipsAtNextOccurrence(t *testing.T) {
	g := newRig(t)
	chk := &fakeCheck{off: map[string]bool{"ac-power": true}}
	g.s.Check = chk
	g.add(t, "big", "every:1h", func(j *Job) { j.Preconditions = []string{"ac-power"} })
	g.clk.Add(time.Hour)
	g.step()
	g.clk.Add(time.Hour) // the next occurrence comes while still on battery
	g.step()
	if len(g.exec.starts) != 0 || !eq(g.statuses(t, "big"), []string{StatusUnmet}) {
		t.Fatalf("starts %v runs %v", g.exec.starts, g.statuses(t, "big"))
	}
	runs, _ := g.st.Runs(context.Background(), "", 0)
	if len(runs) == 0 || runs[0].Error != "ac-power: not now" {
		t.Errorf("why: %+v", runs)
	}
	chk.set("ac-power", false)
	g.step() // the new occurrence (now) runs
	if len(g.exec.starts) != 1 {
		t.Fatalf("starts %v", g.exec.starts)
	}
}
