package store

import (
	"context"
	"database/sql"
	"time"
)

// Run is the index row for one trace (FR-A4); the full record is the JSONL file at TracePath.
type Run struct {
	ID             string
	ParentID       string
	Source         string // cli | proxy | job | tray
	Client         string
	ConversationFP string
	DriverModel    string
	Status         string
	Outcome        string // success | partial | fail; "" = unknown
	OutcomeConf    float64
	OutcomeSource  string
	Steps          int
	Tokens         int
	Ms             int64
	Swaps          int
	NoLearn        bool
	Pinned         bool
	TracePath      string
	ToolCalls      int    // successful tool calls
	ToolPattern    string // their names joined with ">" (LEARNING.md §2)
	TaskType       string // the routing class; "" for pinned runs
	Bucket         int    // difficulty bucket (orchestrator.Bucket)
	Reward         float64
	Counted        bool // Reward is in the bandit
	StartedAt      time.Time
	EndedAt        time.Time
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func (db *DB) InsertRun(ctx context.Context, r Run) error {
	_, err := db.ExecContext(ctx, `INSERT INTO runs (id, parent_id, source, client, conversation_fp, driver_model, status, no_learn, pinned, trace_path, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, nullStr(r.ParentID), r.Source, nullStr(r.Client), nullStr(r.ConversationFP), nullStr(r.DriverModel),
		r.Status, r.NoLearn, r.Pinned, nullStr(r.TracePath), r.StartedAt.UnixMilli())
	return err
}

func (db *DB) FinishRun(ctx context.Context, r Run) error {
	_, err := db.ExecContext(ctx, `UPDATE runs SET status = ?, driver_model = ?, steps = ?, tokens = ?, ms = ?, swaps = ?, tool_calls = ?, tool_pattern = ?,
		task_type = ?, difficulty_bucket = ?, ended_at = ? WHERE id = ?`,
		r.Status, nullStr(r.DriverModel), r.Steps, r.Tokens, r.Ms, r.Swaps, r.ToolCalls, nullStr(r.ToolPattern), nullStr(r.TaskType), r.Bucket,
		r.EndedAt.UnixMilli(), r.ID)
	return err
}

// SetRunStatus changes a run's status alone: a parked run becomes "resumed" when a later run
// continues it.
func (db *DB) SetRunStatus(ctx context.Context, id, status string) error {
	_, err := db.ExecContext(ctx, `UPDATE runs SET status = ? WHERE id = ?`, status, id)
	return err
}

// UndoEntry is one applied file effect (SECURITY.md "Deletion policy"): `noskill undo <run>`
// replays these backwards.
type UndoEntry struct {
	RunID string
	Seq   int
	Op    string // move | copy | create | mkdir | trash
	From  string
	To    string
	Hash  string
}

// AppendUndo records a file effect with the next sequence number for its run.
func (db *DB) AppendUndo(ctx context.Context, e UndoEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM undo_log WHERE run_id = ?`, e.RunID).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO undo_log (run_id, seq, op, "from", "to", hash, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, seq, e.Op, nullStr(e.From), nullStr(e.To), nullStr(e.Hash), time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// UndoEntries returns a run's file effects in the order they were applied.
func (db *DB) UndoEntries(ctx context.Context, runID string) ([]UndoEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq, op, COALESCE("from", ''), COALESCE("to", ''), COALESCE(hash, '') FROM undo_log WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UndoEntry
	for rows.Next() {
		e := UndoEntry{RunID: runID}
		if err := rows.Scan(&e.Seq, &e.Op, &e.From, &e.To, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UndoSince returns every file effect recorded at or after since, oldest first, with the run that
// made it (the week-run audit compares them with what is on disk).
func (db *DB) UndoSince(ctx context.Context, since time.Time) ([]UndoEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT run_id, seq, op, COALESCE("from", ''), COALESCE("to", ''), COALESCE(hash, '') FROM undo_log WHERE created_at >= ? ORDER BY created_at, run_id, seq`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UndoEntry
	for rows.Next() {
		var e UndoEntry
		if err := rows.Scan(&e.RunID, &e.Seq, &e.Op, &e.From, &e.To, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddRunSkills records which skills (and versions) a run used.
func (db *DB) AddRunSkills(ctx context.Context, runID string, skills map[string]string) error {
	for name, version := range skills {
		if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO run_skills (run_id, skill, version) VALUES (?, ?, ?)`, runID, name, nullStr(version)); err != nil {
			return err
		}
	}
	return nil
}

// SetOutcome records how a run turned out (FR-L1). A more certain source replaces a less certain
// one: an explicit rating always wins over a heuristic.
func (db *DB) SetOutcome(ctx context.Context, runID, outcome string, conf float64, source string) error {
	res, err := db.ExecContext(ctx, `UPDATE runs SET outcome = ?, outcome_conf = ?, outcome_source = ?
		WHERE id = ? AND (outcome_source IS NULL OR outcome_source <> 'explicit' OR ? = 'explicit')`,
		outcome, conf, source, runID, source)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID).Scan(&one) != nil {
			return sql.ErrNoRows
		}
	}
	return nil
}

const runCols = `id, parent_id, source, client, conversation_fp, driver_model, status, outcome, outcome_conf, outcome_source,
	steps, tokens, ms, swaps, no_learn, pinned, trace_path, tool_calls, tool_pattern, task_type, COALESCE(difficulty_bucket, 0), reward,
	started_at, ended_at`

func scanRun(r interface{ Scan(...any) error }) (Run, error) {
	var run Run
	var parent, client, fp, model, outcome, osrc, trace, pattern, task sql.NullString
	var oconf, reward sql.NullFloat64
	var started int64
	var ended sql.NullInt64
	if err := r.Scan(&run.ID, &parent, &run.Source, &client, &fp, &model, &run.Status, &outcome, &oconf, &osrc,
		&run.Steps, &run.Tokens, &run.Ms, &run.Swaps, &run.NoLearn, &run.Pinned, &trace, &run.ToolCalls, &pattern, &task, &run.Bucket, &reward, &started, &ended); err != nil {
		return run, err
	}
	run.ParentID, run.Client, run.ConversationFP, run.DriverModel = parent.String, client.String, fp.String, model.String
	run.Outcome, run.OutcomeConf, run.OutcomeSource, run.TracePath = outcome.String, oconf.Float64, osrc.String, trace.String
	run.ToolPattern, run.TaskType, run.Reward, run.Counted = pattern.String, task.String, reward.Float64, reward.Valid
	run.StartedAt = time.UnixMilli(started)
	if ended.Valid {
		run.EndedAt = time.UnixMilli(ended.Int64)
	}
	return run, nil
}

func (db *DB) GetRun(ctx context.Context, id string) (Run, error) {
	return scanRun(db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

// LatestRun returns the most recent run, optionally only top-level ones started by source.
func (db *DB) LatestRun(ctx context.Context, source string) (Run, error) {
	q := `SELECT ` + runCols + ` FROM runs WHERE parent_id IS NULL`
	var args []any
	if source != "" {
		q += ` AND source = ?`
		args = append(args, source)
	}
	return scanRun(db.QueryRowContext(ctx, q+` ORDER BY started_at DESC, id DESC LIMIT 1`, args...))
}

// ListRuns returns recent top-level runs, newest first.
func (db *DB) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE parent_id IS NULL ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
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

// LatestRunByFP finds the most recent run of a conversation (feedback from chat apps).
func (db *DB) LatestRunByFP(ctx context.Context, fp string) (Run, error) {
	return scanRun(db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE conversation_fp = ? AND parent_id IS NULL ORDER BY started_at DESC, id DESC LIMIT 1`, fp))
}

// RunSkills returns the skills a run used, name → version.
func (db *DB) RunSkills(ctx context.Context, runID string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT skill, COALESCE(version, '') FROM run_skills WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

// UndoPending returns a run's file effects that haven't been undone, newest first (the order to undo them in).
func (db *DB) UndoPending(ctx context.Context, runID string) ([]UndoEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq, op, COALESCE("from", ''), COALESCE("to", ''), COALESCE(hash, '') FROM undo_log WHERE run_id = ? AND undone_at IS NULL ORDER BY seq DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UndoEntry
	for rows.Next() {
		e := UndoEntry{RunID: runID}
		if err := rows.Scan(&e.Seq, &e.Op, &e.From, &e.To, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkUndone records that an effect was reverted.
func (db *DB) MarkUndone(ctx context.Context, runID string, seq int) error {
	_, err := db.ExecContext(ctx, `UPDATE undo_log SET undone_at = ? WHERE run_id = ? AND seq = ?`, time.Now().UnixMilli(), runID, seq)
	return err
}

// UnjudgedRuns lists finished top-level runs that the LLM judge should look at (LEARNING.md §1,
// tier 5): no outcome yet, or only an unsure decision judge's. Newest first.
func (db *DB) UnjudgedRuns(ctx context.Context, endedBefore, startedAfter time.Time, unsure float64, limit int) ([]Run, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE parent_id IS NULL AND status = 'done' AND no_learn = 0
		AND source NOT IN ('learning', 'background') AND trace_path IS NOT NULL AND ended_at < ? AND started_at > ?
		AND (outcome IS NULL OR (outcome_source = 'decision' AND outcome_conf < ?))
		ORDER BY started_at DESC, id DESC LIMIT ?`, endedBefore.UnixMilli(), startedAfter.UnixMilli(), unsure, limit)
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
