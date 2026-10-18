package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Decision calibration (FR-B6c). See migrations/0007_calibration.sql.

// Decision is one thresholded decision made for a run.
type Decision struct {
	RunID     string
	Name      string
	Model     string
	Emulated  bool
	Answer    string
	P         float64
	Threshold float64
	At        time.Time
}

func (db *DB) AddDecision(ctx context.Context, d Decision) error {
	_, err := db.ExecContext(ctx, `INSERT INTO decisions (run_id, name, model, emulated, answer, p, threshold, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.RunID, d.Name, d.Model, b2i(d.Emulated), d.Answer, d.P, d.Threshold, d.At.UnixMilli())
	return err
}

// DecisionOutcome is a decision joined with its run's outcome.
type DecisionOutcome struct {
	P       float64
	Answer  string
	Outcome string // success | partial | fail
	Source  string // outcome_source
	Conf    float64
}

// DecisionOutcomes lists the most recent decisions of name by model, since since, whose run has an
// outcome, newest first.
func (db *DB) DecisionOutcomes(ctx context.Context, name, model string, since time.Time, limit int) ([]DecisionOutcome, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.p, coalesce(d.answer, ''), r.outcome, coalesce(r.outcome_source, ''), coalesce(r.outcome_conf, 0)
		FROM decisions d JOIN runs r ON r.id = d.run_id
		WHERE d.name = ? AND d.model = ? AND d.at >= ? AND r.outcome IN ('success', 'partial', 'fail')
		ORDER BY d.at DESC LIMIT ?`, name, model, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionOutcome
	for rows.Next() {
		var o DecisionOutcome
		if err := rows.Scan(&o.P, &o.Answer, &o.Outcome, &o.Source, &o.Conf); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DecisionKey is one (decision, model) pair with how many times it was made.
type DecisionKey struct {
	Name, Model string
	Emulated    bool
	Count       int
	Last        time.Time
}

func (db *DB) DecisionKeys(ctx context.Context, since time.Time) ([]DecisionKey, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, model, max(emulated), count(*), max(at) FROM decisions WHERE at >= ?
		GROUP BY name, model ORDER BY name, model`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionKey
	for rows.Next() {
		var k DecisionKey
		var emu int
		var last int64
		if err := rows.Scan(&k.Name, &k.Model, &emu, &k.Count, &last); err != nil {
			return nil, err
		}
		k.Emulated, k.Last = emu == 1, time.UnixMilli(last)
		out = append(out, k)
	}
	return out, rows.Err()
}

// Threshold is one change of a decision's threshold.
type Threshold struct {
	ID        int64
	Name      string
	Model     string
	Threshold float64
	Previous  float64
	Samples   int
	Gain      float64
	Source    string // auto | manual | revert
	Why       string
	At        time.Time
}

func (db *DB) AddThreshold(ctx context.Context, t Threshold) error {
	_, err := db.ExecContext(ctx, `INSERT INTO decision_thresholds (name, model, threshold, previous, samples, gain, source, why, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.Name, t.Model, t.Threshold, t.Previous, t.Samples, t.Gain, t.Source, t.Why, t.At.UnixMilli())
	return err
}

// Thresholds lists the changes for (name, model), newest first. An empty name lists all.
func (db *DB) Thresholds(ctx context.Context, name, model string, limit int) ([]Threshold, error) {
	q := `SELECT id, name, model, threshold, previous, samples, gain, source, coalesce(why, ''), at FROM decision_thresholds`
	var args []any
	if name != "" {
		q += ` WHERE name = ? AND model = ?`
		args = append(args, name, model)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Threshold
	for rows.Next() {
		var t Threshold
		var at int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Model, &t.Threshold, &t.Previous, &t.Samples, &t.Gain, &t.Source, &t.Why, &at); err != nil {
			return nil, err
		}
		t.At = time.UnixMilli(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CurrentThreshold is the threshold in force for (name, model); ok is false when it was never changed.
func (db *DB) CurrentThreshold(ctx context.Context, name, model string) (Threshold, bool, error) {
	ts, err := db.Thresholds(ctx, name, model, 1)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return Threshold{}, false, err
	}
	if len(ts) == 0 {
		return Threshold{}, false, nil
	}
	return ts[0], true, nil
}
