package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"noskill/internal/ids"
	"noskill/internal/store"
)

// Job is one scheduled skill or prompt (FR-J1). It lives in state.db, so it survives restarts and
// reboots; the daemon is the only process that runs it (ARCHITECTURE.md "Scheduler").
type Job struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TargetKind string `json:"target_kind"` // skill | prompt | anime (the tracker, no model; Target is its stages)
	Target     string `json:"target"`
	Args       Args   `json:"args"`
	Schedule   string `json:"schedule"` // cron:<expr> | every:<duration>
	// Preconditions must all hold for a run to start (FR-J5): volume:<label>, ac-power, unmetered,
	// window:HH:MM-HH:MM (precond.go).
	Preconditions []string      `json:"preconditions,omitempty"`
	Missed        string        `json:"missed_policy"`  // skip | once
	Overlap       string        `json:"overlap_policy"` // skip | queue
	Timeout       time.Duration `json:"timeout"`
	Retries       int           `json:"retries"`
	Notify        []string      `json:"notify"` // channel names (os, ntfy, telegram, discord, email); empty = none
	Enabled       bool          `json:"enabled"`
	NextRunAt     time.Time     `json:"next_run_at"`
	RetryAt       time.Time     `json:"retry_at,omitzero"`
	Attempt       int           `json:"attempt,omitempty"`
	LastRunID     string        `json:"last_run_id,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
}

// Args are the job's run settings.
type Args struct {
	Prompt string   `json:"prompt,omitempty"` // the task for a skill job ("" = the skill's own default)
	Allow  []string `json:"allow,omitempty"`  // extra scopes for the job's skills (like `run --allow`)
	Model  string   `json:"model,omitempty"`  // pin a model; "" = route
}

// JobRun is one occurrence of a job.
type JobRun struct {
	ID           string    `json:"id"`
	JobID        string    `json:"job_id"`
	JobName      string    `json:"job,omitempty"`
	RunID        string    `json:"run_id,omitempty"`
	ScheduledFor time.Time `json:"scheduled_for"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	EndedAt      time.Time `json:"ended_at,omitzero"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	Result       string    `json:"result,omitempty"`
	ApprovalID   string    `json:"approval_id,omitempty"`
	Attempt      int       `json:"attempt,omitempty"`
}

// Job run statuses. running, waiting and stopping are active; the rest are final.
const (
	StatusRunning     = "running"
	StatusWaiting     = "waiting" // parked on an approval
	StatusStopping    = "stopping"
	StatusDone        = "done"
	StatusFailed      = "failed"
	StatusTimeout     = "timeout"
	StatusStopped     = "stopped"
	StatusDeclined    = "declined"    // the approval it waited on was skipped or expired
	StatusMissed      = "missed"      // the machine was off or asleep and missed_policy is skip
	StatusOverlap     = "overlap"     // the previous run was still going and overlap_policy is skip
	StatusInterrupted = "interrupted" // the daemon stopped mid-run
	StatusUnmet       = "unmet"       // a precondition didn't hold before the next occurrence (FR-J5)
)

func Active(status string) bool {
	return status == StatusRunning || status == StatusWaiting || status == StatusStopping
}

// Store reads and writes the jobs tables.
type Store struct{ DB *store.DB }

var ErrNoJob = errors.New("no such job")

const jobCols = `id, name, target_kind, target, args, schedule, missed_policy, overlap_policy, timeout_s, retries, notify, enabled, next_run_at, retry_at, attempt, last_run_id, created_at, preconditions`

func scanJob(r interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var args, notify, pre string
	var timeout int64
	var enabled int
	var next, retry sql.NullInt64
	var last sql.NullString
	var created int64
	if err := r.Scan(&j.ID, &j.Name, &j.TargetKind, &j.Target, &args, &j.Schedule, &j.Missed, &j.Overlap, &timeout, &j.Retries,
		&notify, &enabled, &next, &retry, &j.Attempt, &last, &created, &pre); err != nil {
		return Job{}, err
	}
	json.Unmarshal([]byte(args), &j.Args)
	json.Unmarshal([]byte(notify), &j.Notify)
	json.Unmarshal([]byte(pre), &j.Preconditions)
	j.Timeout, j.Enabled, j.LastRunID = time.Duration(timeout)*time.Second, enabled == 1, last.String
	if next.Valid {
		j.NextRunAt = time.UnixMilli(next.Int64)
	}
	if retry.Valid {
		j.RetryAt = time.UnixMilli(retry.Int64)
	}
	j.CreatedAt = time.UnixMilli(created)
	return j, nil
}

func ms(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

// Add stores a new job. NextRunAt must be set.
func (s *Store) Add(ctx context.Context, j Job) (Job, error) {
	if j.ID == "" {
		j.ID = ids.New()
	}
	args, _ := json.Marshal(j.Args)
	notify, _ := json.Marshal(j.Notify)
	if j.Notify == nil {
		notify = []byte("[]")
	}
	pre, _ := json.Marshal(j.Preconditions)
	if j.Preconditions == nil {
		pre = []byte("[]")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO jobs (`+jobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, NULL, ?, ?)`,
		j.ID, j.Name, j.TargetKind, j.Target, string(args), j.Schedule, j.Missed, j.Overlap, int64(j.Timeout/time.Second), j.Retries,
		string(notify), boolInt(j.Enabled), ms(j.NextRunAt), j.CreatedAt.UnixMilli(), string(pre))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return j, fmt.Errorf("a job named %q already exists (`noskill schedule rm %s` first, or pick another --name)", j.Name, j.Name)
	}
	return j, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get finds a job by name or id.
func (s *Store) Get(ctx context.Context, nameOrID string) (Job, error) {
	j, err := scanJob(s.DB.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE name = ? OR id = ?`, nameOrID, nameOrID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("%w %q (see `noskill schedule list`)", ErrNoJob, nameOrID)
	}
	return j, err
}

func (s *Store) List(ctx context.Context) ([]Job, error) {
	return s.jobs(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY name`)
}

// Due returns enabled jobs whose next occurrence or retry has come.
func (s *Store) Due(ctx context.Context, now time.Time) ([]Job, error) {
	n := now.UnixMilli()
	return s.jobs(ctx, `SELECT `+jobCols+` FROM jobs WHERE enabled = 1 AND ((next_run_at IS NOT NULL AND next_run_at <= ?) OR (retry_at IS NOT NULL AND retry_at <= ?))
		ORDER BY name`, n, n)
}

func (s *Store) jobs(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetEnabled pauses or resumes a job. Resuming sets the next occurrence afresh, so a job paused
// for a week doesn't count as having missed a week of runs.
func (s *Store) SetEnabled(ctx context.Context, id string, on bool, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE jobs SET enabled = ?, next_run_at = CASE WHEN ? THEN ? ELSE next_run_at END, retry_at = NULL, attempt = 0 WHERE id = ?`,
		boolInt(on), on, ms(next), id)
	return err
}

// Remove deletes a job and its run history. The runs and traces themselves stay.
func (s *Store) Remove(ctx context.Context, id string) error {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM job_runs WHERE job_id = ?`, id); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// RunNow makes a job due immediately (`schedule run`); its regular schedule carries on after.
func (s *Store) RunNow(ctx context.Context, id string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE jobs SET retry_at = ?, attempt = 0 WHERE id = ?`, now.UnixMilli(), id)
	return err
}

// claim moves a due job to its next occurrence. It succeeds only for the caller that saw the job
// in this state, so two schedulers can never start the same occurrence.
func (s *Store) claim(ctx context.Context, j Job, next time.Time, retry bool) (bool, error) {
	newNext, attempt := ms(next), 0
	if retry {
		newNext, attempt = ms(j.NextRunAt), j.Attempt
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE jobs SET next_run_at = ?, retry_at = NULL, attempt = ?
		WHERE id = ? AND next_run_at IS ? AND retry_at IS ? AND enabled = 1`,
		newNext, attempt, j.ID, ms(j.NextRunAt), ms(j.RetryAt))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) scheduleRetry(ctx context.Context, jobID string, at time.Time, attempt int) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE jobs SET retry_at = ?, attempt = ? WHERE id = ?`, at.UnixMilli(), attempt, jobID)
	return err
}

func (s *Store) setLastRun(ctx context.Context, jobID, runID string) {
	if runID != "" {
		s.DB.ExecContext(ctx, `UPDATE jobs SET last_run_id = ? WHERE id = ?`, runID, jobID)
	}
}

// ---- job runs ----

const runCols = `r.id, r.job_id, COALESCE(j.name, ''), r.run_id, r.scheduled_for, r.started_at, r.ended_at, r.status, r.error, r.result, r.approval_id, r.attempt`

func scanRun(r interface{ Scan(...any) error }) (JobRun, error) {
	var jr JobRun
	var run, errS, result, appr sql.NullString
	var sched int64
	var started, ended sql.NullInt64
	if err := r.Scan(&jr.ID, &jr.JobID, &jr.JobName, &run, &sched, &started, &ended, &jr.Status, &errS, &result, &appr, &jr.Attempt); err != nil {
		return JobRun{}, err
	}
	jr.RunID, jr.Error, jr.Result, jr.ApprovalID = run.String, errS.String, result.String, appr.String
	jr.ScheduledFor = time.UnixMilli(sched)
	if started.Valid {
		jr.StartedAt = time.UnixMilli(started.Int64)
	}
	if ended.Valid {
		jr.EndedAt = time.UnixMilli(ended.Int64)
	}
	return jr, nil
}

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func (s *Store) insertRun(ctx context.Context, jr JobRun) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO job_runs (id, job_id, run_id, scheduled_for, started_at, ended_at, status, error, result, approval_id, attempt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jr.ID, jr.JobID, ns(jr.RunID), jr.ScheduledFor.UnixMilli(), ms(jr.StartedAt), ms(jr.EndedAt), jr.Status, ns(jr.Error), ns(jr.Result), ns(jr.ApprovalID), jr.Attempt)
	return err
}

func (s *Store) updateRun(ctx context.Context, jr JobRun) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE job_runs SET run_id = ?, ended_at = ?, status = ?, error = ?, result = ?, approval_id = ? WHERE id = ?`,
		ns(jr.RunID), ms(jr.EndedAt), jr.Status, ns(jr.Error), ns(jr.Result), ns(jr.ApprovalID), jr.ID)
	return err
}

// casStatus changes a run's status only if it's still `from`.
func (s *Store) casStatus(ctx context.Context, id, from, to string) bool {
	res, err := s.DB.ExecContext(ctx, `UPDATE job_runs SET status = ? WHERE id = ? AND status = ?`, to, id, from)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// Runs lists recent runs, of one job or of all ("" ), newest first.
func (s *Store) Runs(ctx context.Context, jobID string, limit int) ([]JobRun, error) {
	q := `SELECT ` + runCols + ` FROM job_runs r LEFT JOIN jobs j ON j.id = r.job_id`
	var args []any
	if jobID != "" {
		q += ` WHERE r.job_id = ?`
		args = append(args, jobID)
	}
	q += ` ORDER BY r.scheduled_for DESC, r.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	return s.runs(ctx, q, args...)
}

// ActiveRuns lists runs that are running, waiting for an approval, or being stopped.
func (s *Store) ActiveRuns(ctx context.Context) ([]JobRun, error) {
	return s.runs(ctx, `SELECT `+runCols+` FROM job_runs r LEFT JOIN jobs j ON j.id = r.job_id
		WHERE r.status IN ('running', 'waiting', 'stopping') ORDER BY r.started_at`)
}

func (s *Store) runsWithStatus(ctx context.Context, status string) ([]JobRun, error) {
	return s.runs(ctx, `SELECT `+runCols+` FROM job_runs r LEFT JOIN jobs j ON j.id = r.job_id WHERE r.status = ? ORDER BY r.started_at`, status)
}

func (s *Store) runs(ctx context.Context, q string, args ...any) ([]JobRun, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRun
	for rows.Next() {
		jr, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, jr)
	}
	return out, rows.Err()
}

func (s *Store) hasActive(ctx context.Context, jobID string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_runs WHERE job_id = ? AND status IN ('running', 'waiting', 'stopping')`, jobID).Scan(&n)
	return n > 0, err
}

// RequestStop asks for a job run to stop (`jobs stop`). A run waiting for an approval stops at once;
// a running one is cancelled by the daemon within a few seconds. It returns how many it touched.
func (s *Store) RequestStop(ctx context.Context, runID string, now time.Time) (int, error) {
	where, args := `status IN ('running', 'waiting')`, []any{}
	if runID != "" {
		where += ` AND (id = ? OR job_id = ? OR job_id = (SELECT id FROM jobs WHERE name = ?))`
		args = append(args, runID, runID, runID)
	}
	a, err := s.DB.ExecContext(ctx, `UPDATE job_runs SET status = 'stopped', ended_at = ?, error = 'stopped by you' WHERE status = 'waiting' AND `+where,
		append([]any{now.UnixMilli()}, args...)...)
	if err != nil {
		return 0, err
	}
	b, err := s.DB.ExecContext(ctx, `UPDATE job_runs SET status = 'stopping' WHERE status = 'running' AND `+where, args...)
	if err != nil {
		return 0, err
	}
	n1, _ := a.RowsAffected()
	n2, _ := b.RowsAffected()
	return int(n1 + n2), nil
}

// ---- lease ----

// lease takes or renews the scheduler lease. It reports whether this holder has it, and whether
// it was just taken over (so runs left "running" by a dead process can be cleaned up).
func (s *Store) lease(ctx context.Context, holder string, now time.Time, ttl time.Duration) (held, fresh bool, err error) {
	var prev string
	s.DB.QueryRowContext(ctx, `SELECT holder FROM leases WHERE name = 'scheduler'`).Scan(&prev)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO leases (name, holder, expires_at) VALUES ('scheduler', ?, ?)
		ON CONFLICT(name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE leases.holder = excluded.holder OR leases.expires_at < ?`, holder, now.Add(ttl).UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, n == 1 && prev != holder, nil
}

func (s *Store) release(ctx context.Context, holder string) {
	s.DB.ExecContext(ctx, `DELETE FROM leases WHERE name = 'scheduler' AND holder = ?`, holder)
}

// interruptOrphans marks runs left active by a process that died (reboot, crash) as interrupted.
// mine skips runs this process is still running.
func (s *Store) interruptOrphans(ctx context.Context, now time.Time, mine func(id string) bool) ([]JobRun, error) {
	list, err := s.runs(ctx, `SELECT `+runCols+` FROM job_runs r LEFT JOIN jobs j ON j.id = r.job_id WHERE r.status IN ('running', 'stopping')`)
	if err != nil {
		return nil, err
	}
	var out []JobRun
	for _, jr := range list {
		if mine(jr.ID) {
			continue
		}
		jr.Status, jr.EndedAt = StatusInterrupted, now
		if jr.Error == "" {
			jr.Error = "noskill stopped while this ran"
		}
		s.updateRun(ctx, jr)
		out = append(out, jr)
	}
	return out, nil
}
