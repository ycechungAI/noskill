package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"noskill/internal/agent"
	"noskill/internal/backends"
	"noskill/internal/notify"
	"noskill/internal/scheduler"
)

// Jobs is the scheduler's view of the engine (FR-J1–J4): it runs a job as a headless request
// (Source "job"), so a `confirm` tool parks the run on an approval instead of asking anyone
// (SECURITY.md: scheduled jobs never run confirm actions unattended).
type Jobs struct{ A *App }

var _ scheduler.Executor = Jobs{}

// JobPrompt is what a skill job asks when it has no --prompt of its own.
func JobPrompt(j scheduler.Job, description string) string {
	if j.TargetKind == "prompt" {
		return j.Target
	}
	if j.Args.Prompt != "" {
		return j.Args.Prompt
	}
	return fmt.Sprintf("This is the scheduled run of the %s skill. Do its task now: %s", j.Target, description)
}

func (x Jobs) request(j scheduler.Job) (RunRequest, error) {
	req := RunRequest{Source: "job", Headless: true, Allow: j.Args.Allow, Model: j.Args.Model}
	desc := ""
	if j.TargetKind == "skill" {
		s, ok := x.A.Skills.Get(j.Target)
		if !ok {
			return req, fmt.Errorf("skill %s isn't installed any more", j.Target)
		}
		desc = s.Description
		req.Skills = []string{j.Target}
	}
	req.Messages = []backends.Message{{Role: backends.RoleUser, Content: JobPrompt(j, desc)}}
	return req, nil
}

func (x Jobs) Start(ctx context.Context, j scheduler.Job) scheduler.Result {
	switch j.TargetKind {
	case "anime":
		return x.animeRun(ctx, j)
	case "remind", "brief", "diskwatch":
		return x.A.choreJob(ctx, j)
	}
	req, err := x.request(j)
	if err != nil {
		return scheduler.Result{Status: "failed", Err: err}
	}
	res, err := x.A.Run(ctx, req)
	return jobResult(res, err)
}

// animeRun runs the tracker's stages itself, with no model: the tracker is plain code, and a small
// model driving it only added minutes and wrong turns (a run took 13 minutes where the stages
// take seconds). The job's target is the stage list, "all" or e.g. "download,file".
func (x Jobs) animeRun(ctx context.Context, j scheduler.Job) scheduler.Result {
	t, err := x.A.AnimeTracker(ctx, false)
	if err != nil {
		return jobResult(nil, err)
	}
	var out []string
	for _, st := range strings.Split(j.Target, ",") {
		rep, err := t.Run(ctx, st)
		if err != nil {
			return jobResult(nil, err)
		}
		if s := strings.TrimSpace(rep.String()); s != "" {
			out = append(out, s)
		}
	}
	return scheduler.Result{Status: "done", Summary: summarize(strings.Join(out, " "), 280)}
}

func (x Jobs) Resume(ctx context.Context, j scheduler.Job, jr scheduler.JobRun) scheduler.Result {
	res, err := x.A.Continue(ctx, jr.ApprovalID, RunRequest{Headless: true})
	if errors.Is(err, ErrAlreadyResumed) {
		return scheduler.Result{Status: "done", Summary: "continued from another window"}
	}
	return jobResult(res, err)
}

func (x Jobs) ApprovalStatus(ctx context.Context, id string) string {
	it, err := x.A.Approvals.Get(ctx, id)
	if err != nil {
		return ""
	}
	return string(it.Status)
}

func jobResult(res *RunResult, err error) scheduler.Result {
	out := scheduler.Result{Err: err}
	if res != nil {
		out.RunID = res.RunID
		out.Summary = summarize(res.Answer, 280)
	}
	switch {
	case res != nil && res.Stop == agent.StopWaiting:
		out.Status, out.ApprovalID, out.Err = "waiting", res.WaitingFor, nil
	case err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		out.Status = "cancelled"
	case err != nil:
		out.Status = "failed"
	default:
		out.Status = "done"
	}
	return out
}

func summarize(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// NotifyJob sends a job's result to the channels the job names (FR-J4).
func (a *App) NotifyJob(ctx context.Context, j scheduler.Job, jr scheduler.JobRun, kind string) {
	m := notify.Message{ID: j.ID}
	switch kind {
	case scheduler.NotifyDone:
		m.Title, m.Body = "noskill: "+j.Name+" finished", jr.Result
		switch j.TargetKind {
		case "remind":
			m.Title = "Reminder"
		case "brief":
			m.Title = "noskill: your morning brief"
		case "diskwatch":
			m.Title, m.Urgent = "noskill: low disk space", true
		}
		if m.Body == "" {
			m.Body = "Done."
		}
	case scheduler.NotifyFailed:
		m.Title, m.Body, m.Urgent = "noskill: "+j.Name+" failed", jr.Error, true
	case scheduler.NotifyDeclined:
		m.Title, m.Body = "noskill: "+j.Name+" stopped", "The approval it was waiting for was skipped or expired."
	default:
		return
	}
	if jr.RunID != "" {
		m.Body += "\nnoskill traces show " + jr.RunID
	}
	for _, err := range a.Hub.SendTo(ctx, j.Notify, m) {
		fmt.Fprintln(a.Opts.Stderr, "noskill: notify:", err)
	}
}
