package app

import (
	"context"
	"slices"
	"strings"
	"time"

	"noskill/internal/scheduler"
)

// Editing a job (FR-J8): the job keeps its ID, history and enabled state. The new settings are checked
// with the same rules as a new job; anything the form doesn't carry (scopes, the model pin, the
// Once flag, retries and timeout when left out) keeps its old value.

// UpdateJob rewrites a job from a spec. A job that is running can't be edited until it finishes.
func (a *App) UpdateJob(ctx context.Context, id string, in JobSpec) (scheduler.Job, error) {
	st := &scheduler.Store{DB: a.DB}
	old, err := st.Get(ctx, id)
	if err != nil {
		return scheduler.Job{}, &UserError{"no such job " + id}
	}
	active, _ := st.ActiveRuns(ctx)
	if slices.ContainsFunc(active, func(r scheduler.JobRun) bool { return r.JobID == old.ID }) {
		return scheduler.Job{}, &UserError{old.Name + " is running now; edit it when the run finishes"}
	}
	if in.Model == "" {
		in.Model = old.Args.Model
	}
	in.Allow = old.Args.Allow
	in.Once = old.Args.Once
	if in.Missed == "" {
		in.Missed = old.Missed
	}
	if in.Overlap == "" {
		in.Overlap = old.Overlap
	}
	if in.Timeout == "" && old.Timeout > 0 {
		in.Timeout = scheduler.FormatDuration(old.Timeout)
	}
	if in.Retries == nil {
		r := old.Retries
		in.Retries = &r
	}
	if in.Name == "" {
		in.Name = old.Name
	}
	in.Paused = !old.Enabled
	j, err := a.jobFromSpec(ctx, in)
	if err != nil {
		return j, err
	}
	j.ID, j.CreatedAt, j.LastRunID, j.RetryAt, j.Attempt = old.ID, old.CreatedAt, old.LastRunID, old.RetryAt, old.Attempt
	if err := st.Update(ctx, j); err != nil {
		return scheduler.Job{}, &UserError{err.Error()}
	}
	return j, nil
}

// SpecOf turns a stored job back into the form it was made from, so the edit form starts filled in.
func SpecOf(j scheduler.Job) JobSpec {
	s := JobSpec{Target: jobTarget(j), Name: j.Name, Prompt: j.Args.Prompt, Model: j.Args.Model, When: j.Preconditions,
		Missed: j.Missed, Overlap: j.Overlap, Notify: j.Notify, Paused: !j.Enabled}
	if rest, ok := strings.CutPrefix(j.Schedule, "cron:"); ok {
		s.Cron = rest
	} else {
		s.Every = strings.TrimPrefix(j.Schedule, "every:")
	}
	if j.Timeout > 0 {
		s.Timeout = scheduler.FormatDuration(j.Timeout)
	}
	r := j.Retries
	s.Retries = &r
	return s
}

// jobTarget writes a job's target the way AddJob reads it: a skill name, a prompt sentence, or
// kind:rest for the model-free kinds (remind:, brief:, anime:).
func jobTarget(j scheduler.Job) string {
	switch {
	case j.TargetKind == "skill" || j.TargetKind == "prompt":
		return j.Target
	case j.Target == "":
		return j.TargetKind
	case j.TargetKind == "anime":
		return "anime:" + j.Target
	default:
		return j.TargetKind + ":" + j.Target
	}
}

// JobResult is one job run for the results box. Level is ok, warning, error or running: a warning is
// a run that didn't happen as planned, an error is one that failed.
type JobResult struct {
	ID      string    `json:"id"`
	Job     string    `json:"job"`
	Status  string    `json:"status"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// RecentJobResults lists the latest job runs across all jobs, newest first.
func (a *App) RecentJobResults(ctx context.Context, limit int) ([]JobResult, error) {
	runs, err := (&scheduler.Store{DB: a.DB}).Runs(ctx, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]JobResult, 0, len(runs))
	for _, r := range runs {
		level, msg := jobLevel(r)
		at := r.StartedAt
		if at.IsZero() {
			at = r.ScheduledFor
		}
		out = append(out, JobResult{ID: r.ID, Job: r.JobName, Status: r.Status, Level: level, Message: msg, At: at})
	}
	return out, nil
}

// needsAttention spots a run that reported success but says it had problems or needs the user to do
// something, as the model's own summary does. The run status can't tell these apart yet.
func needsAttention(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"problems", "can't reach", "didn't answer", "couldn't", "could not", "please add", "please specify", "i need to"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func jobLevel(r scheduler.JobRun) (level, msg string) {
	switch r.Status {
	case scheduler.StatusDone:
		msg = r.Result
		if msg == "" {
			msg = "finished"
		}
		if needsAttention(msg) {
			return "warning", clipText(msg, 300)
		}
		return "ok", clipText(msg, 300)
	case scheduler.StatusRunning, scheduler.StatusStopping:
		return "running", "running now"
	case scheduler.StatusWaiting:
		return "running", "waiting for your approval"
	case scheduler.StatusFailed, scheduler.StatusTimeout, scheduler.StatusInterrupted:
		msg = r.Error
		if msg == "" {
			msg = strings.ReplaceAll(r.Status, "_", " ")
		}
		return "error", clipText(msg, 300)
	}
	msg = r.Error
	if msg == "" {
		msg = map[string]string{
			scheduler.StatusUnmet:    "a condition didn't hold, so it didn't run",
			scheduler.StatusOverlap:  "the previous run was still going, so it was skipped",
			scheduler.StatusMissed:   "the machine was off or asleep at the time",
			scheduler.StatusDeclined: "the approval it waited on was declined or expired",
			scheduler.StatusStopped:  "stopped by you",
		}[r.Status]
	}
	if msg == "" {
		msg = r.Status
	}
	return "warning", clipText(msg, 300)
}
