package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"time"

	"noskill/internal/backends"
)

// UpsertBackend records a configured backend and the version it reported.
func (db *DB) UpsertBackend(ctx context.Context, id, kind, url, version string, seen time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO backends (id, kind, url, version, last_seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, url = excluded.url,
		  version = COALESCE(NULLIF(excluded.version, ''), version), last_seen_at = excluded.last_seen_at`,
		id, kind, url, nullStr(version), seen.UnixMilli())
	return err
}

// UpsertModels refreshes model profiles from a backend listing. It keeps tool_strategy and
// benchmark rows: those are measured, not listed. Models that disappeared from the listing stay
// (a model that's briefly removed shouldn't lose its measurements); callers decide presence from
// the live listing.
func (db *DB) UpsertModels(ctx context.Context, backendID string, ms []backends.ModelInfo, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range ms {
		caps, _ := json.Marshal(m.Capabilities)
		if _, err := tx.ExecContext(ctx, `INSERT INTO models (id, backend_id, name, family, params_b, quant, ctx, capabilities, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET backend_id = excluded.backend_id, name = excluded.name, family = excluded.family,
			  params_b = excluded.params_b, quant = excluded.quant, ctx = excluded.ctx, capabilities = excluded.capabilities,
			  updated_at = excluded.updated_at`,
			m.ID, backendID, m.Name, nullStr(m.Family), m.ParamsB, nullStr(m.Quant), m.Context, string(caps), now.UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetToolStrategy stores the probe's verdict for a model (ADR-0005).
func (db *DB) SetToolStrategy(ctx context.Context, modelID string, s backends.ToolStrategy) error {
	res, err := db.ExecContext(ctx, `UPDATE models SET tool_strategy = ? WHERE id = ?`, string(s), modelID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("store: unknown model " + modelID)
	}
	return nil
}

// ToolStrategy returns the stored strategy; ok is false when the model was never probed.
func (db *DB) ToolStrategy(ctx context.Context, modelID string) (s backends.ToolStrategy, ok bool, err error) {
	var v sql.NullString
	err = db.QueryRowContext(ctx, `SELECT tool_strategy FROM models WHERE id = ?`, modelID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil || !v.Valid || v.String == "" {
		return "", false, err
	}
	return backends.ToolStrategy(v.String), true, nil
}

// StoredModel is a profile row.
type StoredModel struct {
	backends.ModelInfo
	ToolStrategy backends.ToolStrategy
	UpdatedAt    time.Time
}

func (db *DB) Models(ctx context.Context) ([]StoredModel, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, backend_id, name, family, params_b, quant, ctx, capabilities, tool_strategy, updated_at FROM models ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredModel
	for rows.Next() {
		var m StoredModel
		var family, quant, strat sql.NullString
		var params sql.NullFloat64
		var ctxLen sql.NullInt64
		var caps string
		var upd int64
		if err := rows.Scan(&m.ID, &m.Backend, &m.Name, &family, &params, &quant, &ctxLen, &caps, &strat, &upd); err != nil {
			return nil, err
		}
		m.Family, m.Quant, m.ParamsB, m.Context = family.String, quant.String, params.Float64, int(ctxLen.Int64)
		m.ToolStrategy = backends.ToolStrategy(strat.String)
		m.UpdatedAt = time.UnixMilli(upd)
		json.Unmarshal([]byte(caps), &m.Capabilities)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- skill vectors (embedding cache for the selector) ----

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// GetSkillVector looks up a cached vector. version is the skill's content hash, so an edited skill
// misses and is embedded again.
func (db *DB) GetSkillVector(ctx context.Context, name, version, model string) ([]float32, bool, error) {
	var b []byte
	err := db.QueryRowContext(ctx, `SELECT vector FROM skill_vectors WHERE name = ? AND version = ? AND embed_model = ?`, name, version, model).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return decodeVec(b), true, nil
}

func (db *DB) PutSkillVector(ctx context.Context, name, version, model string, v []float32) error {
	// Older versions of the same skill are dropped: only the current content is ever looked up.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM skill_vectors WHERE name = ? AND embed_model = ? AND version <> ?`, name, model, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO skill_vectors (name, version, embed_model, vector) VALUES (?, ?, ?, ?)`,
		name, version, model, encodeVec(v)); err != nil {
		return err
	}
	return tx.Commit()
}
