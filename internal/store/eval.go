package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// EvalRun is one run of a task suite on one model (`noskill eval`).
type EvalRun struct {
	ID         string    `json:"id"`
	Model      string    `json:"model"`
	Suite      string    `json:"suite"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	Tasks      int       `json:"tasks"`
	Passed     int       `json:"passed"`
	Skipped    int       `json:"skipped"`
	SkillState string    `json:"skill_state,omitempty"` // json: learned skill → version
	Note       string    `json:"note,omitempty"`
}

// EvalResult is one task's result in a run.
type EvalResult struct {
	RunID   string `json:"run_id,omitempty"`
	TaskID  string `json:"task_id"`
	Type    string `json:"type"`
	Source  string `json:"source"`
	Passed  bool   `json:"passed"`
	Skipped bool   `json:"skipped,omitempty"`
	Why     string `json:"why,omitempty"`
	Steps   int    `json:"steps"`
	Tokens  int    `json:"tokens"`
	Ms      int64  `json:"ms"`
	// Learned are the learned skills the task's run used (FR-L3 regression gate, the learning exit
	// check's learned share).
	Learned []string `json:"learned,omitempty"`
}

func (db *DB) StartEvalRun(ctx context.Context, r EvalRun) error {
	_, err := db.ExecContext(ctx, `INSERT INTO eval_runs (id, model, suite, started_at, skill_state, note) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Model, r.Suite, r.StartedAt.UnixMilli(), r.SkillState, r.Note)
	return err
}

func (db *DB) AddEvalResult(ctx context.Context, r EvalResult) error {
	_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO eval_results (run_id, task_id, type, source, passed, skipped, why, steps, tokens, ms, learned)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.TaskID, r.Type, r.Source, b2i(r.Passed), b2i(r.Skipped), r.Why, r.Steps, r.Tokens, r.Ms, strings.Join(r.Learned, ","))
	return err
}

func (db *DB) FinishEvalRun(ctx context.Context, id string, now time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE eval_runs SET ended_at = ?,
		tasks = (SELECT count(*) FROM eval_results WHERE run_id = ? AND skipped = 0),
		passed = (SELECT count(*) FROM eval_results WHERE run_id = ? AND passed = 1),
		skipped = (SELECT count(*) FROM eval_results WHERE run_id = ? AND skipped = 1) WHERE id = ?`,
		now.UnixMilli(), id, id, id, id)
	return err
}

// EvalRuns lists finished runs, newest first. model "" = all models.
func (db *DB) EvalRuns(ctx context.Context, model string, limit int) ([]EvalRun, error) {
	q := `SELECT id, model, suite, started_at, ended_at, tasks, passed, skipped, skill_state, note FROM eval_runs WHERE ended_at IS NOT NULL`
	var args []any
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalRun
	for rows.Next() {
		var r EvalRun
		var st, en int64
		var ss, note sql.NullString
		if err := rows.Scan(&r.ID, &r.Model, &r.Suite, &st, &en, &r.Tasks, &r.Passed, &r.Skipped, &ss, &note); err != nil {
			return nil, err
		}
		r.StartedAt, r.EndedAt, r.SkillState, r.Note = time.UnixMilli(st), time.UnixMilli(en), ss.String, note.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func (db *DB) EvalResults(ctx context.Context, runID string) ([]EvalResult, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id, type, source, passed, skipped, why, steps, tokens, ms, learned FROM eval_results WHERE run_id = ? ORDER BY task_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalResult
	for rows.Next() {
		r := EvalResult{RunID: runID}
		var p, s int
		var why, learned sql.NullString
		var steps, tokens, ms sql.NullInt64
		if err := rows.Scan(&r.TaskID, &r.Type, &r.Source, &p, &s, &why, &steps, &tokens, &ms, &learned); err != nil {
			return nil, err
		}
		if learned.String != "" {
			r.Learned = strings.Split(learned.String, ",")
		}
		r.Passed, r.Skipped, r.Why, r.Steps, r.Tokens, r.Ms = p == 1, s == 1, why.String, int(steps.Int64), int(tokens.Int64), ms.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// PutScores stores a model's quality per task type from the micro-eval (FR-B4); the router reads
// them as the bandit's prior.
func (db *DB) PutScores(ctx context.Context, modelID string, scores map[string]float64, now time.Time) error {
	for t, s := range scores {
		if _, err := db.ExecContext(ctx, `INSERT INTO model_bench (model_id, task_type, score, measured_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(model_id, task_type) DO UPDATE SET score = excluded.score, measured_at = excluded.measured_at`,
			modelID, t, s, now.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// Scores returns a model's per-task-type quality, 0–1, and when it was measured (the oldest).
func (db *DB) Scores(ctx context.Context, modelID string) (map[string]float64, time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_type, score, measured_at FROM model_bench WHERE model_id = ? AND task_type != ? AND score IS NOT NULL`, modelID, perfTask)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := map[string]float64{}
	var at time.Time
	for rows.Next() {
		var t string
		var s float64
		var ms int64
		if err := rows.Scan(&t, &s, &ms); err != nil {
			return nil, at, err
		}
		out[t] = s
		if m := time.UnixMilli(ms); at.IsZero() || m.Before(at) {
			at = m
		}
	}
	return out, at, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
