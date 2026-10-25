package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"noskill/internal/anime"
	"noskill/internal/scheduler"
)

// JobSpec is a new scheduled job as `noskill schedule add` and the status page's form describe it
// (FR-J1). Cron and Every are alternatives; empty fields take the [scheduler] defaults.
type JobSpec struct {
	Target  string   `json:"target"` // a skill name, or a prompt written as a sentence
	Cron    string   `json:"cron,omitempty"`
	Every   string   `json:"every,omitempty"`
	Name    string   `json:"name,omitempty"`
	Prompt  string   `json:"prompt,omitempty"` // the task for a skill job
	Model   string   `json:"model,omitempty"`
	Allow   []string `json:"allow,omitempty"`
	When    []string `json:"when,omitempty"` // preconditions (FR-J5)
	Missed  string   `json:"missed,omitempty"`
	Overlap string   `json:"overlap,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	Retries *int     `json:"retries,omitempty"` // nil = the [scheduler] default
	Notify  []string `json:"notify,omitempty"`
	Paused  bool     `json:"paused,omitempty"`
}

// AddJob checks a spec and stores the job. Every problem with the input is a *UserError.
func (a *App) AddJob(ctx context.Context, in JobSpec) (scheduler.Job, error) {
	bad := func(f string, args ...any) (scheduler.Job, error) {
		return scheduler.Job{}, &UserError{fmt.Sprintf(f, args...)}
	}
	var pre []string
	for _, w := range in.When {
		p, err := scheduler.ParsePrecondition(w)
		if err != nil {
			return bad("%v", err)
		}
		if !slices.Contains(pre, p) {
			pre = append(pre, p)
		}
	}
	var sched scheduler.Schedule
	var err error
	switch {
	case in.Cron != "" && in.Every != "":
		return bad("use cron or every, not both")
	case in.Cron != "":
		if sched, err = scheduler.ParseCron(in.Cron); err != nil {
			return bad("%v", err)
		}
	case in.Every != "":
		if sched, err = scheduler.ParseEvery(in.Every); err != nil {
			return bad("%v", err)
		}
	default:
		return bad(`say when: a cron like "0 3 * * *" or an interval like 6h`)
	}
	sc := a.Cfg.Scheduler
	j := scheduler.Job{Schedule: sched.String(), Missed: sc.MissedPolicy, Overlap: "skip", Timeout: sc.DefaultTimeout.Duration,
		Retries: sc.Retries, Enabled: !in.Paused, CreatedAt: time.Now(), Args: scheduler.Args{Prompt: in.Prompt, Allow: in.Allow, Model: in.Model},
		Preconditions: pre}
	if in.Missed != "" {
		j.Missed = in.Missed
	}
	if j.Missed != "skip" && j.Missed != "once" {
		return bad("missed must be once or skip")
	}
	if in.Overlap != "" {
		j.Overlap = in.Overlap
	}
	if j.Overlap != "skip" && j.Overlap != "queue" {
		return bad("overlap must be skip or queue")
	}
	if in.Timeout != "" {
		d, err := scheduler.ParseEvery(in.Timeout)
		if err != nil {
			return bad("timeout: want a duration of a minute or more, like 30m")
		}
		j.Timeout = d.D
	}
	if in.Retries != nil {
		j.Retries = *in.Retries
	}
	if j.Retries > 10 {
		return bad("retries: 10 at most")
	}
	known := map[string]bool{"os": true, "ntfy": true, "telegram": true, "discord": true, "email": true}
	for _, n := range in.Notify {
		if n = strings.TrimSpace(n); n == "" || n == "none" {
			continue
		}
		if !known[n] {
			return bad("notify: %q isn't a channel (os, ntfy, telegram, discord, email, none)", n)
		}
		j.Notify = append(j.Notify, n)
	}

	target := strings.TrimSpace(in.Target)
	if target == "" {
		return bad("say what to run: a skill name, or a prompt written as a sentence")
	}
	if rest, ok := strings.CutPrefix(target, "anime:"); ok {
		stages, err := animeStages(rest)
		if err != nil {
			return bad("%v", err)
		}
		j.TargetKind, j.Target = "anime", strings.Join(stages, ",")
		if in.Prompt != "" || in.Model != "" {
			return bad("an anime job runs the tracker directly, with no model, so it takes no task or model")
		}
	} else if s, ok := a.Skills.Get(target); ok {
		j.TargetKind, j.Target = "skill", s.Name
		if s.NeedsApproval() {
			return bad("skill %s comes from a project folder; review and `noskill skills enable %s` first", s.Name, s.Name)
		}
	} else if len(strings.Fields(target)) == 1 && !strings.ContainsAny(target, "?.!") {
		return bad("no skill named %q (see `noskill skills list`); to schedule a prompt, write it as a sentence", target)
	} else {
		j.TargetKind, j.Target = "prompt", target
		if in.Prompt != "" {
			return bad("the task field is for skill jobs; this job is already a prompt")
		}
	}
	if in.Model != "" {
		if _, err := a.FindModel(ctx, in.Model); err != nil {
			return scheduler.Job{}, err
		}
		if slices.Contains(a.Cfg.Orchestrator.Exclude, in.Model) {
			return bad("%s is excluded in the config (orchestrator.exclude)", in.Model)
		}
	}
	j.Name = strings.TrimSpace(in.Name)
	if j.Name == "" {
		j.Name = JobName(j)
	}
	j.NextRunAt = sched.Next(time.Now())
	if j.NextRunAt.IsZero() {
		return bad("that schedule never comes round")
	}
	return (&scheduler.Store{DB: a.DB}).Add(ctx, j)
}

// JobName makes a short name from the target: the skill name, or the prompt's first words.
func JobName(j scheduler.Job) string {
	switch j.TargetKind {
	case "skill":
		return j.Target
	case "anime":
		return "anime-" + strings.ReplaceAll(j.Target, ",", "-")
	}
	var words []string
	for _, w := range strings.Fields(strings.ToLower(j.Target)) {
		w = strings.Trim(w, `.,!?:;"'()`)
		if w != "" {
			words = append(words, w)
		}
		if len(words) == 4 {
			break
		}
	}
	n := strings.Join(words, "-")
	if n == "" {
		n = "job"
	}
	return n
}

// animeStages checks a stage list for an anime job: "all", or stages from the tracker's own list.
func animeStages(s string) ([]string, error) {
	var out []string
	for _, st := range strings.Split(s, ",") {
		if st = strings.TrimSpace(st); st != "" {
			out = append(out, st)
		}
	}
	if len(out) == 0 {
		out = []string{"all"}
	}
	for _, st := range out {
		if st != "all" && !slices.Contains(anime.Stages, st) {
			return nil, fmt.Errorf("anime job: %q isn't a stage (all, %s)", st, strings.Join(anime.Stages, ", "))
		}
	}
	return out, nil
}
