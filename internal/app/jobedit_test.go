package app

import (
	"testing"
	"time"

	"noskill/internal/scheduler"
)

func TestJobResultLevels(t *testing.T) {
	cases := []struct {
		run   scheduler.JobRun
		level string
		msg   string
	}{
		{scheduler.JobRun{Status: scheduler.StatusDone, Result: "3 new episodes"}, "ok", "3 new episodes"},
		{scheduler.JobRun{Status: scheduler.StatusDone}, "ok", "finished"},
		{scheduler.JobRun{Status: scheduler.StatusDone, Result: "Problems: - Transmission didn't answer."}, "warning", "Problems: - Transmission didn't answer."},
		{scheduler.JobRun{Status: scheduler.StatusDone, Result: "Please add the folder(s) you want to index with `noskill docs add <folder>`."}, "warning", "Please add the folder(s) you want to index with `noskill docs add <folder>`."},
		{scheduler.JobRun{Status: scheduler.StatusFailed, Error: "model unloaded"}, "error", "model unloaded"},
		{scheduler.JobRun{Status: scheduler.StatusTimeout, Error: "took longer than 30m"}, "error", "took longer than 30m"},
		{scheduler.JobRun{Status: scheduler.StatusUnmet}, "warning", "a condition didn't hold, so it didn't run"},
		{scheduler.JobRun{Status: scheduler.StatusOverlap}, "warning", "the previous run was still going, so it was skipped"},
		{scheduler.JobRun{Status: scheduler.StatusDone, Result: "To run the scheduled run, I need to list the contents of downloads."}, "warning", "To run the scheduled run, I need to list the contents of downloads."},
		{scheduler.JobRun{Status: scheduler.StatusRunning}, "running", "running now"},
	}
	for _, c := range cases {
		level, msg := jobLevel(c.run)
		if level != c.level || msg != c.msg {
			t.Errorf("%s: got %s %q, want %s %q", c.run.Status, level, msg, c.level, c.msg)
		}
	}
}

// A job read back into the form gives the same target and schedule it was made from.
func TestSpecOfRoundTrip(t *testing.T) {
	for _, j := range []scheduler.Job{
		{TargetKind: "skill", Target: "daily-brief", Schedule: "cron:0 7 * * 1-5", Enabled: true, Timeout: 30 * time.Minute, Notify: []string{"os"}},
		{TargetKind: "prompt", Target: "Summarise my notes folder please", Schedule: "every:6h", Args: scheduler.Args{Prompt: ""}, Enabled: false},
		{TargetKind: "brief", Target: "12", Schedule: "every:1d", Enabled: true, Preconditions: []string{"ac-power"}},
		{TargetKind: "remind", Target: "call Sam", Schedule: "every:1h", Enabled: true},
		{TargetKind: "backup", Schedule: "cron:0 3 * * *", Enabled: true},
		{TargetKind: "anime", Target: "download,file", Schedule: "cron:0 14 * * 6", Enabled: true},
	} {
		s := SpecOf(j)
		if got := jobTarget(j); s.Target != got {
			t.Errorf("%s: target %q, want %q", j.TargetKind, s.Target, got)
		}
		if j.Schedule[:5] == "cron:" && s.Cron != j.Schedule[5:] || j.Schedule[:6] == "every:" && s.Every != j.Schedule[6:] {
			t.Errorf("%s: schedule %+v from %s", j.TargetKind, s, j.Schedule)
		}
		if s.Paused != !j.Enabled {
			t.Errorf("%s: paused %v", j.TargetKind, s.Paused)
		}
	}
	if got := jobTarget(scheduler.Job{TargetKind: "backup"}); got != "backup" {
		t.Errorf("backup target %q", got)
	}
}
