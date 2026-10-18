package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SkillRecord is the database's index of a skill folder (files are the source of truth).
type SkillRecord struct {
	Name        string
	Origin      string
	Status      string
	Version     string
	Path        string
	Description string
	Triggers    []string
	Permissions json.RawMessage
	Hash        string
	Enabled     bool
	// NeedsApproval skills (from a project folder) start disabled, and are disabled again whenever
	// their content changes, so a repository can't swap in new permissions after the user said yes.
	NeedsApproval bool
}

// SyncSkill inserts or updates a skill's row and returns whether it is enabled.
func (db *DB) SyncSkill(ctx context.Context, r SkillRecord, now time.Time) (enabled bool, err error) {
	trig, _ := json.Marshal(r.Triggers)
	if r.Triggers == nil {
		trig = []byte("[]")
	}
	perms := string(r.Permissions)
	if perms == "" {
		perms = "{}"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var oldHash sql.NullString
	var oldEnabled bool
	err = tx.QueryRowContext(ctx, `SELECT content_hash, enabled FROM skills WHERE name = ?`, r.Name).Scan(&oldHash, &oldEnabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		enabled = !r.NeedsApproval
		_, err = tx.ExecContext(ctx, `INSERT INTO skills (name, origin, status, version, path, description, triggers, permissions, content_hash, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Name, r.Origin, r.Status, nullStr(r.Version), r.Path, r.Description, string(trig), perms, r.Hash, enabled, now.UnixMilli(), now.UnixMilli())
	case err == nil:
		enabled = oldEnabled
		if r.NeedsApproval && oldHash.String != r.Hash {
			enabled = false
		}
		_, err = tx.ExecContext(ctx, `UPDATE skills SET origin = ?, status = ?, version = ?, path = ?, description = ?, triggers = ?, permissions = ?,
			content_hash = ?, enabled = ?, updated_at = ? WHERE name = ?`,
			r.Origin, r.Status, nullStr(r.Version), r.Path, r.Description, string(trig), perms, r.Hash, enabled, now.UnixMilli(), r.Name)
	}
	if err != nil {
		return false, err
	}
	return enabled, tx.Commit()
}

// SetSkillEnabled turns a skill on or off (`noskill skills enable|disable`).
func (db *DB) SetSkillEnabled(ctx context.Context, name string, on bool) error {
	res, err := db.ExecContext(ctx, `UPDATE skills SET enabled = ? WHERE name = ?`, on, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// EnabledSkills returns the names of enabled skills.
func (db *DB) EnabledSkills(ctx context.Context) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, enabled FROM skills`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		var e bool
		if err := rows.Scan(&n, &e); err != nil {
			return nil, err
		}
		out[n] = e
	}
	return out, rows.Err()
}

// ---- measured model performance ----

// Perf is what `noskill models bench` measured for a model at one context size.
type Perf struct {
	TokPerSec  float64
	TTFTMs     float64
	LoadMsCold float64
	LoadMsWarm float64
	MemBytes   int64
	Ctx        int
	MeasuredAt time.Time
}

const perfTask = "_perf"

func (db *DB) PutPerf(ctx context.Context, modelID string, p Perf) error {
	_, err := db.ExecContext(ctx, `INSERT INTO model_bench (model_id, task_type, tok_s, ttft_ms, load_ms_cold, load_ms_warm, mem_bytes, ctx, measured_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(model_id, task_type) DO UPDATE SET tok_s = excluded.tok_s, ttft_ms = excluded.ttft_ms,
		  load_ms_cold = COALESCE(NULLIF(excluded.load_ms_cold, 0), load_ms_cold), load_ms_warm = COALESCE(NULLIF(excluded.load_ms_warm, 0), load_ms_warm),
		  mem_bytes = excluded.mem_bytes, ctx = excluded.ctx, measured_at = excluded.measured_at`,
		modelID, perfTask, p.TokPerSec, p.TTFTMs, p.LoadMsCold, p.LoadMsWarm, p.MemBytes, p.Ctx, p.MeasuredAt.UnixMilli())
	return err
}

func (db *DB) GetPerf(ctx context.Context, modelID string) (Perf, bool, error) {
	var p Perf
	var tok, ttft, cold, warm sql.NullFloat64
	var mem, c sql.NullInt64
	var at int64
	err := db.QueryRowContext(ctx, `SELECT tok_s, ttft_ms, load_ms_cold, load_ms_warm, mem_bytes, ctx, measured_at FROM model_bench WHERE model_id = ? AND task_type = ?`,
		modelID, perfTask).Scan(&tok, &ttft, &cold, &warm, &mem, &c, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p = Perf{TokPerSec: tok.Float64, TTFTMs: ttft.Float64, LoadMsCold: cold.Float64, LoadMsWarm: warm.Float64,
		MemBytes: mem.Int64, Ctx: int(c.Int64), MeasuredAt: time.UnixMilli(at)}
	return p, true, nil
}

// SkillUsage is how often a skill was used and how its runs turned out.
type SkillUsage struct {
	Uses    int
	Rated   int // runs with an outcome
	Success int
}

func (db *DB) SkillUsage(ctx context.Context) (map[string]SkillUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT rs.skill, count(*),
		sum(CASE WHEN r.outcome IS NOT NULL THEN 1 ELSE 0 END), sum(CASE WHEN r.outcome = 'success' THEN 1 ELSE 0 END)
		FROM run_skills rs JOIN runs r ON r.id = rs.run_id GROUP BY rs.skill`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SkillUsage{}
	for rows.Next() {
		var n string
		var u SkillUsage
		if err := rows.Scan(&n, &u.Uses, &u.Rated, &u.Success); err != nil {
			return nil, err
		}
		out[n] = u
	}
	return out, rows.Err()
}

// SetInstalledFrom records where an installed skill came from (git URL and commit).
func (db *DB) SetInstalledFrom(ctx context.Context, name, from string) error {
	_, err := db.ExecContext(ctx, `UPDATE skills SET installed_from = ? WHERE name = ?`, from, name)
	return err
}

// ForgetSkill drops a skill's index rows; its files are the source of truth and are handled separately.
func (db *DB) ForgetSkill(ctx context.Context, name string) error {
	for _, q := range []string{`DELETE FROM skill_vectors WHERE name = ?`, `DELETE FROM skills WHERE name = ?`} {
		if _, err := db.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	return nil
}

// SkillVersion is one row of skill_versions (FR-S9); the files are in the skill's .versions folder.
type SkillVersion struct {
	Name, Version, Hash, Parent, Changelog, CreatedBy string
}

func (db *DB) AddSkillVersion(ctx context.Context, v SkillVersion, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO skill_versions (name, version, content_hash, parent_version, changelog, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(name, version) DO UPDATE SET content_hash = excluded.content_hash`,
		v.Name, v.Version, v.Hash, nullStr(v.Parent), v.Changelog, v.CreatedBy, now.UnixMilli())
	return err
}

// SkillStats is how one version of a skill has done (skill_stats). Streak counts the latest rated
// runs in a row that succeeded with at least the confidence asked for.
type SkillStats struct {
	Uses, Successes, Failures, Skips int
	Streak                           int
	Recent                           []RatedRun // the latest rated runs, newest first (at most 10)
}

// RatedRun is a run's outcome as the trial and refinement rules see it: "success" only when it
// was sure enough.
type RatedRun struct {
	RunID   string
	Outcome string // success | partial | fail
}

// RefreshSkillStats recounts a skill version's uses and outcomes from its runs and stores them.
// Outcomes change after the fact (a guess, then the user's rating), so counts are recomputed, not
// incremented. Skips are kept as they were.
func (db *DB) RefreshSkillStats(ctx context.Context, name, version string, minConf float64) (SkillStats, error) {
	var st SkillStats
	rows, err := db.QueryContext(ctx, `SELECT r.id, COALESCE(r.outcome, ''), COALESCE(r.outcome_conf, 0), r.started_at
		FROM run_skills rs JOIN runs r ON r.id = rs.run_id
		WHERE rs.skill = ? AND COALESCE(rs.version, '') = ? AND r.no_learn = 0 ORDER BY r.started_at DESC`, name, version)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var last int64
	streak := true
	for rows.Next() {
		var id, outcome string
		var conf float64
		var at int64
		if err := rows.Scan(&id, &outcome, &conf, &at); err != nil {
			return st, err
		}
		st.Uses++
		last = max(last, at)
		ok := outcome == "success" && conf >= minConf
		switch {
		case ok:
			st.Successes++
		case outcome == "fail":
			st.Failures++
		}
		if outcome == "" {
			continue // not rated yet: neither keeps nor breaks the streak
		}
		if len(st.Recent) < 10 {
			label := outcome
			if outcome == "success" && !ok {
				label = "partial"
			}
			st.Recent = append(st.Recent, RatedRun{RunID: id, Outcome: label})
		}
		if streak && ok {
			st.Streak++
		} else {
			streak = false
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	err = db.QueryRowContext(ctx, `INSERT INTO skill_stats (name, version, uses, successes, failures, last_used_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name, version) DO UPDATE SET uses = excluded.uses, successes = excluded.successes, failures = excluded.failures,
		last_used_at = excluded.last_used_at RETURNING skips`, name, version, st.Uses, st.Successes, st.Failures, sql.NullInt64{Int64: last, Valid: last > 0}).Scan(&st.Skips)
	return st, err
}

// AddSkip records that the user turned a skill version down and returns the skips so far.
func (db *DB) AddSkip(ctx context.Context, name, version string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `INSERT INTO skill_stats (name, version, skips) VALUES (?, ?, 1)
		ON CONFLICT(name, version) DO UPDATE SET skips = skips + 1 RETURNING skips`, name, version).Scan(&n)
	return n, err
}

// SkillLastUse is when a skill was last used by a run that counts (not learning's own), or when
// noskill first saw it if it was never used.
func (db *DB) SkillLastUse(ctx context.Context, name string) (time.Time, error) {
	var ms sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT max(r.started_at) FROM run_skills rs JOIN runs r ON r.id = rs.run_id WHERE rs.skill = ? AND r.no_learn = 0),
		(SELECT created_at FROM skills WHERE name = ?))`, name, name).Scan(&ms)
	if err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64), nil
}

// LearningDone counts finished queue items of a kind since a time whose result starts with prefix
// (the curator's weekly report).
func (db *DB) LearningDone(ctx context.Context, kind, prefix string, since time.Time) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM learning_queue WHERE kind = ? AND status = 'done' AND result LIKE ? AND updated_at >= ?`,
		kind, prefix+"%", since.UnixMilli()).Scan(&n)
	return n, err
}
