package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"noskill/internal/watch"
)

// Watches (skills/watcher). See migrations/0008_watch.sql.

var ErrNoWatch = errors.New("no such watch")

func (db *DB) AddWatch(ctx context.Context, w watch.Watch) error {
	_, err := db.ExecContext(ctx, `INSERT INTO watches (name, kind, target, topic, schedule, job_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.Name, string(w.Kind), w.Target, nullStr(w.Topic), w.Schedule, nullStr(w.JobID), w.CreatedAt.UnixMilli())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("a watch named %q already exists (watch_remove it first, or pick another name)", w.Name)
	}
	return err
}

const watchCols = `name, kind, target, coalesce(topic, ''), schedule, coalesce(job_id, ''), coalesce(etag, ''), coalesce(last_modified, ''), coalesce(last_checked, 0), created_at`

func scanWatch(r interface{ Scan(...any) error }) (watch.Watch, error) {
	var w watch.Watch
	var kind string
	var checked, created int64
	if err := r.Scan(&w.Name, &kind, &w.Target, &w.Topic, &w.Schedule, &w.JobID, &w.ETag, &w.LastModified, &checked, &created); err != nil {
		return w, err
	}
	w.Kind = watch.Kind(kind)
	if checked > 0 {
		w.LastChecked = time.UnixMilli(checked)
	}
	w.CreatedAt = time.UnixMilli(created)
	return w, nil
}

func (db *DB) GetWatch(ctx context.Context, name string) (watch.Watch, error) {
	w, err := scanWatch(db.QueryRowContext(ctx, `SELECT `+watchCols+` FROM watches WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fmt.Errorf("%w named %q (watch_list shows them)", ErrNoWatch, name)
	}
	return w, err
}

func (db *DB) ListWatches(ctx context.Context) ([]watch.Watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+watchCols+` FROM watches ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []watch.Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (db *DB) SetWatchJob(ctx context.Context, name, jobID string) error {
	_, err := db.ExecContext(ctx, `UPDATE watches SET job_id = ? WHERE name = ?`, jobID, name)
	return err
}

// RemoveWatch deletes the watch and its items and returns its job id.
func (db *DB) RemoveWatch(ctx context.Context, name string) (string, error) {
	w, err := db.GetWatch(ctx, name)
	if err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM watch_items WHERE watch = ?`, name); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watches WHERE name = ?`, name); err != nil {
		return "", err
	}
	return w.JobID, tx.Commit()
}

// SeenItems returns the fingerprints already recorded for a watch.
func (db *DB) SeenItems(ctx context.Context, name string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT fingerprint FROM watch_items WHERE watch = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		seen[fp] = true
	}
	return seen, rows.Err()
}

// RecordCheck stores a check's items as seen and the response validators for the next conditional
// request. etag and lastModified are kept as given (empty clears them).
func (db *DB) RecordCheck(ctx context.Context, name string, items []watch.Item, etag, lastModified string, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watch_items (watch, fingerprint, title, url, first_seen) VALUES (?, ?, ?, ?, ?)`,
			name, it.Fingerprint, it.Title, nullStr(it.URL), now.UnixMilli()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET etag = ?, last_modified = ?, last_checked = ? WHERE name = ?`,
		nullStr(etag), nullStr(lastModified), now.UnixMilli(), name); err != nil {
		return err
	}
	return tx.Commit()
}
