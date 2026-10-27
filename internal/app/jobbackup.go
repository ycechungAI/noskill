package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/scheduler"
)

// Job backups (FR-J6): the job list as a JSON file, so a new computer or a new account can get its
// jobs back. A backup holds what each job is, not its state: no ids, next runs or history.

// JobBackupFormat is the version written in every backup.
const JobBackupFormat = 1

// JobBackup is one backup file.
type JobBackup struct {
	Format    int           `json:"noskill_jobs_backup"`
	CreatedAt time.Time     `json:"created_at"`
	Jobs      []BackedUpJob `json:"jobs"`
}

// BackedUpJob is a job as a backup keeps it.
type BackedUpJob struct {
	Name          string         `json:"name"`
	TargetKind    string         `json:"target_kind"`
	Target        string         `json:"target"`
	Args          scheduler.Args `json:"args"`
	Schedule      string         `json:"schedule"`
	Preconditions []string       `json:"preconditions,omitempty"`
	Missed        string         `json:"missed_policy"`
	Overlap       string         `json:"overlap_policy"`
	Timeout       string         `json:"timeout"`
	Retries       int            `json:"retries"`
	Notify        []string       `json:"notify,omitempty"`
	Enabled       bool           `json:"enabled"`
}

// JobBackupInfo describes a backup file in the backups folder.
type JobBackupInfo struct {
	Name      string    `json:"name"` // the file name, which RestoreJobBackup takes
	Path      string    `json:"path"`
	Jobs      int       `json:"jobs"`
	CreatedAt time.Time `json:"created_at"`
	Unchanged bool      `json:"unchanged,omitempty"` // BackupJobs wrote nothing: the newest backup already has these jobs
}

// RestoreReport says what a restore did with each job in the backup.
type RestoreReport struct {
	Added   []string         `json:"added"`
	Skipped []SkippedRestore `json:"skipped"`
}

type SkippedRestore struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

// JobBackupsDir is where backups go: <data>/backups.
func (a *App) JobBackupsDir() string { return filepath.Join(a.Dirs.Data, "backups") }

// jobKinds are the target kinds a job can have (scheduler.Job.TargetKind).
var jobKinds = []string{"skill", "prompt", "anime", "remind", "brief", "diskwatch", "backup"}

func (a *App) currentBackup(ctx context.Context) (JobBackup, error) {
	js, err := (&scheduler.Store{DB: a.DB}).List(ctx)
	if err != nil {
		return JobBackup{}, err
	}
	b := JobBackup{Format: JobBackupFormat, CreatedAt: time.Now(), Jobs: []BackedUpJob{}}
	for _, j := range js {
		b.Jobs = append(b.Jobs, BackedUpJob{Name: j.Name, TargetKind: j.TargetKind, Target: j.Target, Args: j.Args, Schedule: j.Schedule,
			Preconditions: j.Preconditions, Missed: j.Missed, Overlap: j.Overlap, Timeout: scheduler.FormatDuration(j.Timeout),
			Retries: j.Retries, Notify: j.Notify, Enabled: j.Enabled})
	}
	return b, nil
}

// BackupJobs writes the job list to a new file in the backups folder, or to path when it is set.
// In the backups folder nothing is written when the newest backup already holds the same jobs, so a
// nightly backup job doesn't pile up copies.
func (a *App) BackupJobs(ctx context.Context, path string) (JobBackupInfo, error) {
	b, err := a.currentBackup(ctx)
	if err != nil {
		return JobBackupInfo{}, err
	}
	if path == "" {
		if list, _ := a.ListJobBackups(); len(list) > 0 {
			if last, err := readJobBackup(list[0].Path); err == nil && sameJobs(last.Jobs, b.Jobs) {
				list[0].Unchanged = true
				return list[0], nil
			}
		}
		if err := os.MkdirAll(a.JobBackupsDir(), 0o700); err != nil {
			return JobBackupInfo{}, err
		}
		path = filepath.Join(a.JobBackupsDir(), "jobs-"+b.CreatedAt.Format("20060102-150405")+".json")
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return JobBackupInfo{}, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return JobBackupInfo{}, err
	}
	return JobBackupInfo{Name: filepath.Base(path), Path: path, Jobs: len(b.Jobs), CreatedAt: b.CreatedAt}, nil
}

func sameJobs(x, y []BackedUpJob) bool {
	a, _ := json.Marshal(x)
	b, _ := json.Marshal(y)
	return bytes.Equal(a, b)
}

// ListJobBackups lists the backups folder, newest first. Files that aren't job backups are left out.
func (a *App) ListJobBackups() ([]JobBackupInfo, error) {
	ents, err := os.ReadDir(a.JobBackupsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []JobBackupInfo
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(a.JobBackupsDir(), e.Name())
		b, err := readJobBackup(p)
		if err != nil {
			continue
		}
		out = append(out, JobBackupInfo{Name: e.Name(), Path: p, Jobs: len(b.Jobs), CreatedAt: b.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func readJobBackup(path string) (JobBackup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return JobBackup{}, err
	}
	return ParseJobBackup(data)
}

// ParseJobBackup reads a backup file's contents. Every problem is a *UserError.
func ParseJobBackup(data []byte) (JobBackup, error) {
	var b JobBackup
	if err := json.Unmarshal(data, &b); err != nil {
		return b, &UserError{"that isn't a noskill jobs backup (it isn't JSON)"}
	}
	if b.Format == 0 {
		return b, &UserError{"that isn't a noskill jobs backup (no noskill_jobs_backup field)"}
	}
	if b.Format > JobBackupFormat {
		return b, &UserError{fmt.Sprintf("this backup is format %d, newer than this noskill reads (%d): update noskill first", b.Format, JobBackupFormat)}
	}
	return b, nil
}

// RestoreJobBackup restores a backup from the backups folder by its file name.
func (a *App) RestoreJobBackup(ctx context.Context, name string) (RestoreReport, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return RestoreReport{}, &UserError{fmt.Sprintf("%q isn't a backup in %s", name, a.JobBackupsDir())}
	}
	data, err := os.ReadFile(filepath.Join(a.JobBackupsDir(), name))
	if os.IsNotExist(err) {
		return RestoreReport{}, &UserError{fmt.Sprintf("no backup named %s in %s", name, a.JobBackupsDir())}
	}
	if err != nil {
		return RestoreReport{}, err
	}
	b, err := ParseJobBackup(data)
	if err != nil {
		return RestoreReport{}, err
	}
	return a.RestoreJobs(ctx, b)
}

// RestoreJobs adds the backup's jobs. A job whose name is already taken is skipped, never replaced,
// and so is one this noskill can't run (an unknown kind, a skill that isn't installed, a schedule it
// can't read); the report says why. Restored jobs start at their next occurrence from now.
func (a *App) RestoreJobs(ctx context.Context, b JobBackup) (RestoreReport, error) {
	st := &scheduler.Store{DB: a.DB}
	have, err := st.List(ctx)
	if err != nil {
		return RestoreReport{}, err
	}
	taken := map[string]bool{}
	for _, j := range have {
		taken[j.Name] = true
	}
	rep := RestoreReport{Added: []string{}, Skipped: []SkippedRestore{}}
	for _, bj := range b.Jobs {
		j, why := a.restorable(bj)
		if why == "" && taken[j.Name] {
			why = "a job with this name already exists"
		}
		if why == "" {
			if _, err := st.Add(ctx, j); err != nil {
				why = err.Error()
			}
		}
		if why != "" {
			rep.Skipped = append(rep.Skipped, SkippedRestore{Name: bj.Name, Why: why})
			continue
		}
		taken[j.Name] = true
		rep.Added = append(rep.Added, j.Name)
	}
	return rep, nil
}

// restorable turns a backed-up job into a new job, or says why it can't be restored here.
func (a *App) restorable(bj BackedUpJob) (scheduler.Job, string) {
	j := scheduler.Job{Name: strings.TrimSpace(bj.Name), TargetKind: bj.TargetKind, Target: bj.Target, Args: bj.Args,
		Schedule: bj.Schedule, Preconditions: bj.Preconditions, Missed: bj.Missed, Overlap: bj.Overlap, Retries: bj.Retries,
		Notify: bj.Notify, Enabled: bj.Enabled, CreatedAt: time.Now()}
	if j.Name == "" {
		return j, "it has no name"
	}
	if !slices.Contains(jobKinds, j.TargetKind) {
		return j, fmt.Sprintf("this noskill doesn't know the job kind %q", j.TargetKind)
	}
	if j.TargetKind == "skill" {
		s, ok := a.Skills.Get(j.Target)
		if !ok {
			return j, fmt.Sprintf("skill %s isn't installed", j.Target)
		}
		if s.NeedsApproval() {
			return j, fmt.Sprintf("skill %s comes from a project folder; `noskill skills enable %s` first", s.Name, s.Name)
		}
	}
	sched, err := scheduler.ParseSchedule(j.Schedule)
	if err != nil {
		return j, err.Error()
	}
	for _, p := range j.Preconditions {
		if _, err := scheduler.ParsePrecondition(p); err != nil {
			return j, err.Error()
		}
	}
	if j.Missed != "skip" && j.Missed != "once" {
		j.Missed = a.Cfg.Scheduler.MissedPolicy
	}
	if j.Overlap != "skip" && j.Overlap != "queue" {
		j.Overlap = "skip"
	}
	j.Timeout = a.Cfg.Scheduler.DefaultTimeout.Duration
	if d, err := scheduler.ParseEvery(bj.Timeout); err == nil {
		j.Timeout = d.D
	}
	if j.NextRunAt = sched.Next(time.Now()); j.NextRunAt.IsZero() {
		return j, "its schedule never comes round"
	}
	return j, ""
}

// backupJob runs a `backup` job: a backup of the job list into the backups folder.
func (a *App) backupJob(ctx context.Context) scheduler.Result {
	info, err := a.BackupJobs(ctx, "")
	if err != nil {
		return scheduler.Result{Status: "failed", Err: err}
	}
	if info.Unchanged {
		return scheduler.Result{Status: "done", Quiet: true, Summary: fmt.Sprintf("jobs unchanged since %s", info.Name)}
	}
	return scheduler.Result{Status: "done", Summary: fmt.Sprintf("backed up %d jobs to %s", info.Jobs, info.Path)}
}
