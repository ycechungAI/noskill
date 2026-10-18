package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// LearnItem is one row of learning_queue (LEARNING.md §2): a run to draft a skill from, a skill to
// refine, or two skills to merge.
type LearnItem struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // draft | refine | merge
	RunIDs    []string  `json:"run_ids,omitempty"`
	Skill     string    `json:"skill,omitempty"`
	Status    string    `json:"status"`            // pending | running | done | failed | skipped
	Reason    string    `json:"reason,omitempty"`  // novel | repeated | asked | failures | duplicate
	Pattern   string    `json:"pattern,omitempty"` // a draft's tool pattern; a merge's duplicate skill
	Result    string    `json:"result,omitempty"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Enqueue adds an item unless an equivalent one is already queued or was handled within dedupe:
// the same run for a draft, the same tool pattern for a draft, or the same skill for a refine.
// It returns whether the item was added.
func (db *DB) Enqueue(ctx context.Context, it LearnItem, now time.Time, dedupe time.Duration) (bool, error) {
	runs, _ := json.Marshal(it.RunIDs)
	if it.RunIDs == nil {
		runs = []byte("[]")
	}
	since := now.Add(-dedupe).UnixMilli()
	var one int
	var err error
	switch {
	case it.Kind == "draft" && len(it.RunIDs) > 0:
		err = db.QueryRowContext(ctx, `SELECT 1 FROM learning_queue, json_each(learning_queue.run_ids) j
			WHERE kind = 'draft' AND j.value = ? AND (status IN ('pending', 'running') OR updated_at >= ?) LIMIT 1`, it.RunIDs[0], since).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) && it.Pattern != "" && it.Reason != "asked" {
			err = db.QueryRowContext(ctx, `SELECT 1 FROM learning_queue WHERE kind = 'draft' AND pattern = ?
				AND (status IN ('pending', 'running') OR updated_at >= ?) LIMIT 1`, it.Pattern, since).Scan(&one)
		}
	case it.Skill != "":
		err = db.QueryRowContext(ctx, `SELECT 1 FROM learning_queue WHERE kind = ? AND skill = ?
			AND (status IN ('pending', 'running') OR updated_at >= ?) LIMIT 1`, it.Kind, it.Skill, since).Scan(&one)
	default:
		err = sql.ErrNoRows
	}
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO learning_queue (id, kind, run_ids, skill, status, reason, pattern, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		it.ID, it.Kind, string(runs), nullStr(it.Skill), nullStr(it.Reason), nullStr(it.Pattern), now.UnixMilli(), now.UnixMilli())
	return err == nil, err
}

const learnCols = `id, kind, run_ids, skill, status, reason, pattern, result, attempts, last_error, created_at, updated_at`

func scanLearn(r interface{ Scan(...any) error }) (LearnItem, error) {
	var it LearnItem
	var runs string
	var skill, reason, pattern, result, lastErr sql.NullString
	var created, updated int64
	if err := r.Scan(&it.ID, &it.Kind, &runs, &skill, &it.Status, &reason, &pattern, &result, &it.Attempts, &lastErr, &created, &updated); err != nil {
		return it, err
	}
	json.Unmarshal([]byte(runs), &it.RunIDs)
	it.Skill, it.Reason, it.Pattern, it.Result, it.LastError = skill.String, reason.String, pattern.String, result.String, lastErr.String
	it.CreatedAt, it.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return it, nil
}

// ClaimLearning marks the oldest pending item of one of kinds as running and returns it. A running
// item left behind by a process that stopped is pending again after stale.
func (db *DB) ClaimLearning(ctx context.Context, kinds []string, now time.Time, stale time.Duration) (LearnItem, bool, error) {
	if _, err := db.ExecContext(ctx, `UPDATE learning_queue SET status = 'pending' WHERE status = 'running' AND started_at < ?`,
		now.Add(-stale).UnixMilli()); err != nil {
		return LearnItem{}, false, err
	}
	for _, k := range kinds {
		it, err := scanLearn(db.QueryRowContext(ctx, `UPDATE learning_queue SET status = 'running', attempts = attempts + 1, started_at = ?, updated_at = ?
			WHERE id = (SELECT id FROM learning_queue WHERE status = 'pending' AND kind = ? ORDER BY created_at, id LIMIT 1)
			RETURNING `+learnCols, now.UnixMilli(), now.UnixMilli(), k))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return it, err == nil, err
	}
	return LearnItem{}, false, nil
}

// ClaimLearningID marks one pending item as running (`noskill learn` in the foreground).
func (db *DB) ClaimLearningID(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE learning_queue SET status = 'running', attempts = attempts + 1, started_at = ?, updated_at = ?
		WHERE id = ? AND status = 'pending'`, now.UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GetLearning returns one queue item.
func (db *DB) GetLearning(ctx context.Context, id string) (LearnItem, error) {
	return scanLearn(db.QueryRowContext(ctx, `SELECT `+learnCols+` FROM learning_queue WHERE id = ?`, id))
}

// FinishLearning records how an item ended: done, failed (lastErr) or skipped.
func (db *DB) FinishLearning(ctx context.Context, id, status, result, lastErr string, now time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE learning_queue SET status = ?, result = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, nullStr(result), nullStr(lastErr), now.UnixMilli(), id)
	return err
}

// ListLearning returns the newest items first.
func (db *DB) ListLearning(ctx context.Context, limit int) ([]LearnItem, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+learnCols+` FROM learning_queue ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearnItem
	for rows.Next() {
		it, err := scanLearn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// LearningStarted counts items of kind started since t: the nightly caps.
func (db *DB) LearningStarted(ctx context.Context, kind string, since time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM learning_queue WHERE kind = ? AND started_at >= ?`, kind, since.UnixMilli()).Scan(&n)
	return n, err
}

// PatternCount counts finished, successful top-level runs with this tool pattern since t.
func (db *DB) PatternCount(ctx context.Context, pattern string, since time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE tool_pattern = ? AND started_at >= ? AND no_learn = 0
		AND outcome IS NOT 'fail' AND status = 'done'`, pattern, since.UnixMilli()).Scan(&n)
	return n, err
}

// RunsToMine returns finished runs after the cursor (by end time) that may be worth learning from:
// not marked no-learn, not learning's own runs, with at least minCalls tool calls.
func (db *DB) RunsToMine(ctx context.Context, after time.Time, minCalls, limit int) ([]Run, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE ended_at > ? AND status = 'done' AND no_learn = 0
		AND source <> 'learning' AND tool_calls >= ? ORDER BY ended_at LIMIT ?`, after.UnixMilli(), minCalls, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastActivity is when the user last started or finished a run (learning's own runs don't count),
// and whether one is running now. The learning worker only works when this is long enough ago.
func (db *DB) LastActivity(ctx context.Context) (last time.Time, running bool, err error) {
	var started, ended sql.NullInt64
	var n int
	err = db.QueryRowContext(ctx, `SELECT max(started_at), max(ended_at) FROM runs WHERE source <> 'learning'`).Scan(&started, &ended)
	if err != nil {
		return
	}
	// A run marked running for hours belongs to a process that stopped without finishing it.
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE source <> 'learning' AND status = 'running' AND started_at > ?`,
		time.Now().Add(-2*time.Hour).UnixMilli()).Scan(&n)
	ms := max(started.Int64, ended.Int64)
	if ms > 0 {
		last = time.UnixMilli(ms)
	}
	return last, n > 0, err
}

// JobRunning says whether a scheduled job is running (learning waits for it).
func (db *DB) JobRunning(ctx context.Context) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_runs WHERE status IN ('running', 'stopping')`).Scan(&n)
	return n > 0, err
}

// ---- app_state ----

func (db *DB) GetState(ctx context.Context, key string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM app_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (db *DB) SetState(ctx context.Context, key, value string, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO app_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, now.UnixMilli())
	return err
}

// LastInteractive is when someone last used noskill directly: the latest run not started by a job
// or by learning.
func (db *DB) LastInteractive(ctx context.Context) (time.Time, error) {
	var started, ended sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT max(started_at), max(ended_at) FROM runs WHERE source NOT IN ('learning', 'job', 'eval')`).Scan(&started, &ended)
	if ms := max(started.Int64, ended.Int64); err == nil && ms > 0 {
		return time.UnixMilli(ms), nil
	}
	return time.Time{}, err
}

// TopDriverModel is the model that answered most of the runs since the given time ("" if none).
func (db *DB) TopDriverModel(ctx context.Context, since time.Time) (string, error) {
	var m sql.NullString
	err := db.QueryRowContext(ctx, `SELECT driver_model FROM runs WHERE started_at >= ? AND driver_model IS NOT NULL AND driver_model <> ''
		AND source IN ('cli', 'proxy', 'tray', 'job') GROUP BY driver_model ORDER BY count(*) DESC, driver_model LIMIT 1`, since.UnixMilli()).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m.String, err
}
