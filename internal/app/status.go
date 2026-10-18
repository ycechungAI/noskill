package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/platform"
	"noskill/internal/scheduler"
	"noskill/internal/store"
)

// What the status page and the tray show (FR-I6, FR-I7). One snapshot, so a client makes one call.

// Status is the snapshot.
type Status struct {
	At         time.Time        `json:"at"`
	Primary    string           `json:"primary,omitempty"` // the loaded chat model
	Models     []ModelStatus    `json:"models"`
	Backends   []BackendStatus  `json:"backends"`
	Jobs       []JobStatus      `json:"jobs"`
	JobsPaused bool             `json:"jobs_paused"` // "pause all" is in effect
	Approvals  []approvals.Item `json:"approvals"`
	// Volumes are the attached drives, offered as folder suggestions for approval fields (FR-A6).
	Volumes  []platform.VolumeInfo `json:"volumes,omitempty"`
	Runs     []RunStatus           `json:"runs"`
	Skills   []SkillStatus         `json:"skills"`
	Learning []store.LearnItem     `json:"learning"`
	Eval     []store.EvalRun       `json:"eval"`
	Sandbox  string                `json:"sandbox"`
}

type ModelStatus struct {
	Name     string             `json:"name"`
	Backend  string             `json:"backend"`
	SizeGB   float64            `json:"size_gb"`
	Loaded   bool               `json:"loaded"`
	Caps     []string           `json:"caps"`
	Strategy string             `json:"strategy,omitempty"`
	TokPerS  float64            `json:"tok_s,omitempty"`
	LoadS    float64            `json:"load_s,omitempty"`
	MemGB    float64            `json:"mem_gb,omitempty"`
	Quality  map[string]float64 `json:"quality,omitempty"`
}

type BackendStatus struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type JobStatus struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Schedule string    `json:"schedule"`
	Enabled  bool      `json:"enabled"`
	Next     time.Time `json:"next,omitzero"`
	Last     string    `json:"last,omitempty"` // status of the latest run
	LastAt   time.Time `json:"last_at,omitzero"`
	Running  bool      `json:"running"`
}

type RunStatus struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Model   string    `json:"model"`
	Status  string    `json:"status"`
	Outcome string    `json:"outcome,omitempty"`
	Task    string    `json:"task_type,omitempty"`
	Steps   int       `json:"steps"`
	Tokens  int       `json:"tokens"`
	Ms      int64     `json:"ms"`
	At      time.Time `json:"at"`
}

type SkillStatus struct {
	Name    string `json:"name"`
	Origin  string `json:"origin"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Uses    int    `json:"uses"`
	Rated   int    `json:"rated"`
	Success int    `json:"success"`
	Tools   int    `json:"tools"`
	// Review: a learned candidate waiting for its first use, or a refinement on trial.
	Review string `json:"review,omitempty"`
}

// Status gathers the snapshot. Parts that fail to load are left empty rather than failing the page.
func (a *App) Status(ctx context.Context) Status {
	st := Status{At: time.Now(), Models: []ModelStatus{}, Backends: []BackendStatus{}, Jobs: []JobStatus{},
		Approvals: []approvals.Item{}, Runs: []RunStatus{}, Skills: []SkillStatus{}, Learning: []store.LearnItem{}, Eval: []store.EvalRun{}}
	ms, errs := a.Models(ctx)
	for _, m := range ms {
		x := ModelStatus{Name: m.Name, Backend: m.NB.ID, SizeGB: float64(m.SizeBytes) / 1e9, Loaded: m.Loaded && m.chat(), Strategy: string(m.Strategy)}
		for _, c := range m.Capabilities {
			x.Caps = append(x.Caps, string(c))
		}
		if m.Loaded && m.chat() {
			st.Primary = m.Name
		}
		if perf, ok, _ := a.DB.GetPerf(ctx, m.ID); ok {
			x.TokPerS, x.LoadS, x.MemGB = perf.TokPerSec, perf.LoadMsCold/1000, float64(perf.MemBytes)/1e9
		}
		if sc, _, err := a.DB.Scores(ctx, m.ID); err == nil && len(sc) > 0 {
			x.Quality = sc
		}
		st.Models = append(st.Models, x)
	}
	for _, nb := range a.Backends {
		b := BackendStatus{ID: nb.ID, Kind: nb.Kind, OK: true}
		for _, err := range errs {
			if strings.HasPrefix(err.Error(), nb.ID+": ") {
				b.OK, b.Error = false, strings.TrimPrefix(err.Error(), nb.ID+": ")
			}
		}
		st.Backends = append(st.Backends, b)
	}

	js := &scheduler.Store{DB: a.DB}
	if jobs, err := js.List(ctx); err == nil {
		active, _ := js.ActiveRuns(ctx)
		for _, j := range jobs {
			x := JobStatus{ID: j.ID, Name: j.Name, Schedule: j.Schedule, Enabled: j.Enabled, Next: j.NextRunAt}
			if j.Enabled && x.Next.IsZero() {
				x.Next = j.RetryAt
			}
			if runs, err := js.Runs(ctx, j.ID, 1); err == nil && len(runs) > 0 {
				x.Last, x.LastAt = runs[0].Status, runs[0].EndedAt
			}
			x.Running = slices.ContainsFunc(active, func(r scheduler.JobRun) bool { return r.JobID == j.ID })
			st.Jobs = append(st.Jobs, x)
		}
	}
	if v, _ := a.DB.GetState(ctx, pausedJobsKey); v != "" && v != "[]" {
		st.JobsPaused = true
	}

	if items, err := a.Approvals.List(ctx, false, 0); err == nil {
		for _, it := range items {
			it.ResumeToken = ""
			st.Approvals = append(st.Approvals, it)
		}
	}
	if len(st.Approvals) > 0 {
		st.Volumes, _ = platform.ListVolumes()
	}
	if runs, err := a.DB.ListRuns(ctx, 25); err == nil {
		for _, r := range runs {
			st.Runs = append(st.Runs, RunStatus{ID: r.ID, Source: r.Source, Model: r.DriverModel, Status: r.Status, Outcome: r.Outcome,
				Task: r.TaskType, Steps: r.Steps, Tokens: r.Tokens, Ms: r.Ms, At: r.StartedAt})
		}
	}
	usage, _ := a.DB.SkillUsage(ctx)
	for _, s := range a.Skills.All() {
		u := usage[s.Name]
		x := SkillStatus{Name: s.Name, Origin: s.Origin(), Status: s.Status(), Version: s.Meta.Version, Uses: u.Uses, Rated: u.Rated, Success: u.Success, Tools: len(s.Tools)}
		switch {
		case s.Origin() == "learned" && s.Status() == "candidate":
			x.Review = "waiting for its first use (you'll be asked)"
		case s.Origin() == "learned":
			if a.Learned().HasNext(s.Name) {
				x.Review = "a refinement is on trial"
			}
		}
		st.Skills = append(st.Skills, x)
	}
	if q, err := a.DB.ListLearning(ctx, 15); err == nil {
		st.Learning = q
	}
	if ev, err := a.DB.EvalRuns(ctx, "", 8); err == nil {
		st.Eval = ev
	}
	st.Sandbox = platform.NewSandbox().Strength().Mechanism
	return st
}

// ---- job control for the tray and the status page ----

const pausedJobsKey = "jobs.paused_all"

// JobAction pauses, resumes or starts a job now (the same as `noskill schedule`).
func (a *App) JobAction(ctx context.Context, job, action string) error {
	s := &scheduler.Store{DB: a.DB}
	j, err := s.Get(ctx, job)
	if err != nil {
		return &UserError{"no such job: " + job}
	}
	switch action {
	case "pause":
		return s.SetEnabled(ctx, j.ID, false, time.Time{})
	case "resume":
		sched, err := scheduler.ParseSchedule(j.Schedule)
		if err != nil {
			return err
		}
		return s.SetEnabled(ctx, j.ID, true, sched.Next(time.Now()))
	case "run":
		if !j.Enabled {
			return &UserError{j.Name + " is paused; resume it first"}
		}
		return s.RunNow(ctx, j.ID, time.Now())
	}
	return &UserError{fmt.Sprintf("unknown job action %q (pause, resume, run)", action)}
}

// PauseAllJobs pauses every enabled job and remembers which, so ResumeAllJobs doesn't wake jobs
// the user had paused themselves.
func (a *App) PauseAllJobs(ctx context.Context) (int, error) {
	s := &scheduler.Store{DB: a.DB}
	jobs, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, j := range jobs {
		if j.Enabled {
			if err := s.SetEnabled(ctx, j.ID, false, time.Time{}); err != nil {
				return len(ids), err
			}
			ids = append(ids, j.ID)
		}
	}
	b, _ := json.Marshal(ids)
	return len(ids), a.DB.SetState(ctx, pausedJobsKey, string(b), time.Now())
}

// ResumeAllJobs undoes PauseAllJobs.
func (a *App) ResumeAllJobs(ctx context.Context) (int, error) {
	var ids []string
	if v, _ := a.DB.GetState(ctx, pausedJobsKey); v != "" {
		_ = json.Unmarshal([]byte(v), &ids)
	}
	n := 0
	for _, id := range ids {
		if err := a.JobAction(ctx, id, "resume"); err == nil {
			n++
		}
	}
	return n, a.DB.SetState(ctx, pausedJobsKey, "", time.Now())
}
