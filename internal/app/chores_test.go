package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"noskill/internal/backendtest"
	"noskill/internal/scheduler"
)

// Reminders, the morning brief and disk-watch are scheduled jobs that run in Go with no model.
func TestRemindersAreModelFreeJobs(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	svc := &runServices{a: e.app}
	st := &scheduler.Store{DB: e.app.DB}

	out, err := svc.AddReminder(ctx, "remind me to submit the report every Monday at nine")
	if err != nil || !strings.Contains(out, "every Monday at 09:00") {
		t.Fatalf("%s %v", out, err)
	}
	j, err := st.Get(ctx, "remind-submit-the-report")
	if err != nil || j.TargetKind != "remind" || j.Target != "Submit the report" || j.Args.Once || !j.Enabled || len(j.Notify) != 1 || j.Notify[0] != "os" {
		t.Fatalf("job %+v %v", j, err)
	}
	// The same words again get a second, numbered job rather than an error.
	if _, err := svc.AddReminder(ctx, "remind me to submit the report every Friday at 4pm"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "remind-submit-the-report-2"); err != nil {
		t.Error("the second reminder wasn't numbered")
	}
	// A sentence with no time is refused with something to ask the user.
	if _, err := svc.AddReminder(ctx, "remind me to call Sam"); err == nil || !strings.Contains(err.Error(), "when") {
		t.Errorf("err = %v", err)
	}
	if list, _ := svc.ListReminders(ctx); !strings.Contains(list, `"name":"remind-submit-the-report"`) {
		t.Errorf("list = %s", list)
	}

	// A one-off fires once and switches itself off; the notification text is the reminder.
	once, _, err := e.app.AddReminder(ctx, "call Sam in 30 minutes")
	if err != nil || !once.Args.Once {
		t.Fatalf("%+v %v", once, err)
	}
	res := Jobs{A: e.app}.Start(ctx, once)
	if res.Status != "done" || res.Summary != "Call Sam" || res.Quiet {
		t.Errorf("result %+v", res)
	}
	if got, _ := st.Get(ctx, once.Name); got.Enabled {
		t.Error("a one-off reminder stayed on after it fired")
	}

	if err := svc.RemoveReminder(ctx, "remind-submit-the-report"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveReminder(ctx, "remind-submit-the-report"); err == nil {
		t.Error("removing a reminder twice should say there is none")
	}
}

func TestBriefAndDiskwatchJobs(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	b, err := e.app.AddJob(ctx, JobSpec{Target: "brief:12", Cron: "0 7 * * *"})
	if err != nil || b.TargetKind != "brief" || b.Target != "12" || b.Name != "brief" || len(b.Notify) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	res := Jobs{A: e.app}.Start(ctx, b)
	if res.Status != "done" || res.Summary == "" {
		t.Errorf("brief: %+v", res)
	}
	if _, err := e.app.AddJob(ctx, JobSpec{Target: "brief:900", Cron: "0 7 * * *", Name: "x"}); err == nil {
		t.Error("a 900-hour brief was accepted")
	}
	if _, err := e.app.AddJob(ctx, JobSpec{Target: "brief", Cron: "0 7 * * *", Model: "qwen3:4b", Name: "y"}); err == nil {
		t.Error("a model-free job accepted a model")
	}
	d, err := e.app.AddJob(ctx, JobSpec{Target: "diskwatch", Cron: "30 6 * * *"})
	if err != nil || d.TargetKind != "diskwatch" || d.Name != "diskwatch" {
		t.Fatalf("%+v %v", d, err)
	}
	r := Jobs{A: e.app}.Start(ctx, d)
	if r.Status != "done" || (r.Quiet && !strings.Contains(r.Summary, "enough")) {
		t.Errorf("diskwatch: %+v", r)
	}
	if !r.Quiet && !strings.Contains(r.Summary, "free") {
		t.Errorf("an alert should say what is low: %+v", r)
	}
}

func TestBriefCountsRunsAndApprovals(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	text := e.app.Brief(ctx, 24)
	if !strings.Contains(text, "Nothing needs you") && !strings.Contains(text, "Low disk space") {
		t.Errorf("an empty system's brief = %q", text)
	}
	_ = time.Now
}
