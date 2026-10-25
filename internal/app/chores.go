package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"noskill/internal/anime"
	"noskill/internal/approvals"
	"noskill/internal/chores"
	"noskill/internal/platform"
	"noskill/internal/scheduler"
)

// The housekeeping skills' jobs and services (skills/daily-brief, skills/disk-watch; the logic is in
// internal/chores). The jobs run in Go with no model.

// choreJob runs a remind, brief or diskwatch job.
func (a *App) choreJob(ctx context.Context, j scheduler.Job) scheduler.Result {
	switch j.TargetKind {
	case "remind":
		if j.Args.Once {
			// A one-off switches itself off first, so a crash can't make it fire again next year.
			(&scheduler.Store{DB: a.DB}).SetEnabled(ctx, j.ID, false, time.Time{})
		}
		return scheduler.Result{Status: "done", Summary: j.Target}
	case "brief":
		hours := 24
		fmt.Sscanf(j.Target, "%d", &hours)
		return scheduler.Result{Status: "done", Summary: a.Brief(ctx, hours)}
	case "diskwatch":
		reports := a.DiskReports()
		if alert := chores.Alert(reports); alert != "" {
			return scheduler.Result{Status: "done", Summary: alert}
		}
		return scheduler.Result{Status: "done", Summary: "Every drive has enough free space.", Quiet: true}
	}
	return scheduler.Result{Status: "failed", Err: fmt.Errorf("unknown job kind %q", j.TargetKind)}
}

// DiskReports checks every volume against disk-watch's limits ([skills.disk-watch]: min_free_gb,
// min_free_percent, volumes).
func (a *App) DiskReports() []chores.VolumeReport {
	vs, _ := platform.ListVolumes()
	o := chores.DiskOptions{}
	if v, ok := a.userSetting("disk-watch", "min_free_gb"); ok {
		fmt.Sscanf(v, "%g", &o.MinFreeGB)
	}
	if v, ok := a.userSetting("disk-watch", "min_free_percent"); ok {
		fmt.Sscanf(v, "%g", &o.MinFreePct)
	}
	if v, ok := a.userSetting("disk-watch", "volumes"); ok {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				o.Only = append(o.Only, s)
			}
		}
	}
	return chores.CheckVolumes(vs, o)
}

// Brief is the digest of the last `hours` hours: jobs, filed episodes, approvals, disk, what is next.
func (a *App) Brief(ctx context.Context, hours int) string {
	if hours < 1 {
		hours = 24
	}
	now := time.Now()
	f := chores.BriefFacts{Since: now.Add(-time.Duration(hours) * time.Hour)}
	st := &scheduler.Store{DB: a.DB}
	if runs, err := st.Runs(ctx, "", 300); err == nil {
		failed := map[string]bool{}
		for _, r := range runs {
			if r.ScheduledFor.Before(f.Since) {
				break
			}
			switch r.Status {
			case scheduler.StatusDone:
				f.JobsRan++
			case scheduler.StatusFailed, scheduler.StatusTimeout:
				f.JobsRan++
				if !failed[r.JobName] {
					failed[r.JobName] = true
					f.JobsFailed = append(f.JobsFailed, r.JobName)
				}
			}
		}
	}
	if titles, err := (&anime.State{DB: a.DB}).Titles(ctx); err == nil {
		for _, t := range titles {
			var keys []string
			for k, e := range t.Filed {
				if e.At.After(f.Since) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				f.Filed = append(f.Filed, t.Title+" "+k)
			}
		}
	}
	if items, err := a.Approvals.List(ctx, false, 200); err == nil {
		for _, it := range items {
			if it.Status == approvals.Pending {
				f.ApprovalsWaiting++
			}
		}
	}
	f.LowVolumes = chores.Alert(a.DiskReports())
	if jobs, err := st.List(ctx); err == nil {
		sort.Slice(jobs, func(i, k int) bool { return jobs[i].NextRunAt.Before(jobs[k].NextRunAt) })
		for _, j := range jobs {
			if j.Enabled && !j.NextRunAt.IsZero() && len(f.Next) < 3 && j.TargetKind != "brief" {
				f.Next = append(f.Next, j.Name+" "+briefWhen(j.NextRunAt, now))
			}
		}
	}
	return f.Text(now)
}

func briefWhen(t, now time.Time) string {
	t = t.Local()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "today " + t.Format("15:04")
	case t.Before(now.AddDate(0, 0, 6)):
		return t.Format("Mon 15:04")
	}
	return t.Format("2 Jan 15:04")
}

// AddReminder reads a sentence and schedules it as a model-free job (FR-J1).
func (a *App) AddReminder(ctx context.Context, text string) (scheduler.Job, chores.Reminder, error) {
	r, err := chores.ParseReminder(text, time.Now())
	if err != nil {
		return scheduler.Job{}, r, &UserError{err.Error()}
	}
	spec := JobSpec{Target: "remind:" + r.Message, Cron: r.Cron, Every: r.Every, Once: r.Once, Missed: "once"}
	st := &scheduler.Store{DB: a.DB}
	base := JobName(scheduler.Job{TargetKind: "remind", Target: r.Message})
	spec.Name = base
	for i := 2; ; i++ {
		if _, err := st.Get(ctx, spec.Name); err != nil {
			break
		}
		spec.Name = fmt.Sprintf("%s-%d", base, i)
	}
	j, err := a.AddJob(ctx, spec)
	return j, r, err
}

// Reminders lists the reminder jobs.
func (a *App) Reminders(ctx context.Context) ([]scheduler.Job, error) {
	jobs, err := (&scheduler.Store{DB: a.DB}).List(ctx)
	var out []scheduler.Job
	for _, j := range jobs {
		if j.TargetKind == "remind" {
			out = append(out, j)
		}
	}
	return out, err
}

// The daily-brief tools' service (internal/tools/builtin/chores.go).

func (s *runServices) Brief(ctx context.Context, hours int) (string, error) {
	return s.a.Brief(ctx, hours), nil
}

func (s *runServices) AddReminder(ctx context.Context, text string) (string, error) {
	j, r, err := s.a.AddReminder(ctx, text)
	if err != nil {
		return "", err // the message says what to ask the user
	}
	b, err := json.Marshal(map[string]any{"name": j.Name, "reminder": r.Message, "when": r.When, "next": j.NextRunAt})
	return string(b), err
}

func (s *runServices) ListReminders(ctx context.Context) (string, error) {
	jobs, err := s.a.Reminders(ctx)
	if err != nil {
		return "", err
	}
	type row struct {
		Name     string    `json:"name"`
		Text     string    `json:"text"`
		Schedule string    `json:"schedule"`
		Next     time.Time `json:"next,omitzero"`
		Once     bool      `json:"once,omitempty"`
		Active   bool      `json:"active"`
	}
	out := []row{}
	for _, j := range jobs {
		out = append(out, row{j.Name, j.Target, j.Schedule, j.NextRunAt, j.Args.Once, j.Enabled})
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func (s *runServices) RemoveReminder(ctx context.Context, name string) error {
	return s.a.RemoveReminder(ctx, name)
}

// RemoveReminder deletes a reminder job by name.
func (a *App) RemoveReminder(ctx context.Context, name string) error {
	st := &scheduler.Store{DB: a.DB}
	j, err := st.Get(ctx, name)
	if err != nil || j.TargetKind != "remind" {
		return &UserError{fmt.Sprintf("no reminder named %q (reminder_list shows them)", name)}
	}
	return st.Remove(ctx, j.ID)
}
