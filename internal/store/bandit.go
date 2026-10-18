package store

import (
	"context"
	"time"
)

// The routing bandit (ORCHESTRATOR.md "Learned routing", FR-O4). alpha and beta hold evidence
// only: reward and its complement summed over runs. The prior from benchmark scores is added when
// routing, so a new benchmark changes it without touching what was learned. model_id holds the
// model's name.

// Evidence is the learned part of one arm.
type Evidence struct{ Alpha, Beta float64 }

// SetReward counts a run's reward in the bandit, replacing what the run counted before.
func (db *DB) SetReward(ctx context.Context, r Run, reward float64, now time.Time) error {
	if r.TaskType == "" || r.DriverModel == "" {
		return nil
	}
	da, dbeta := reward, 1-reward
	if r.Counted {
		da, dbeta = reward-r.Reward, r.Reward-reward
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO bandit (task_type, difficulty_bucket, model_id, alpha, beta, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_type, difficulty_bucket, model_id) DO UPDATE SET alpha = max(0, alpha + excluded.alpha),
		beta = max(0, beta + excluded.beta), updated_at = excluded.updated_at`,
		r.TaskType, r.Bucket, r.DriverModel, da, dbeta, now.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET reward = ? WHERE id = ?`, reward, r.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// BanditEvidence returns the evidence per model for one task type and difficulty bucket.
func (db *DB) BanditEvidence(ctx context.Context, taskType string, bucket int) (map[string]Evidence, error) {
	rows, err := db.QueryContext(ctx, `SELECT model_id, alpha, beta FROM bandit WHERE task_type = ? AND difficulty_bucket = ?`, taskType, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Evidence{}
	for rows.Next() {
		var m string
		var e Evidence
		if err := rows.Scan(&m, &e.Alpha, &e.Beta); err != nil {
			return nil, err
		}
		out[m] = e
	}
	return out, rows.Err()
}
