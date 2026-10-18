// Package store opens state.db (SQLite, WAL) and applies the embedded forward-only migrations
// (DATA_MODEL.md "Migrations").
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"noskill/internal/platform"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// keepBackups is how many state.db.bak-<version> copies stay; older ones go to the trash.
const keepBackups = 3

// ErrSchemaNewer means state.db was written by a newer noskill. `noskill update --to <older>`
// restores the matching backup instead of opening it.
var ErrSchemaNewer = errors.New("state.db was written by a newer noskill")

type DB struct {
	*sql.DB
	Path string
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("bad migration file name %s (want NNNN_name.sql)", e.Name())
		}
		b, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{v, name, string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps; found %04d at position %d", m.version, i+1)
		}
	}
	return out, nil
}

// Latest is the schema version this binary writes.
func Latest() int {
	ms, err := migrations()
	if err != nil {
		panic(err)
	}
	return len(ms)
}

// Open opens (creating if needed) the database at path and migrates it. trash receives backups
// beyond the last three; nil leaves them in place.
func Open(ctx context.Context, path string, trash platform.Trasher) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// _txlock=immediate: every transaction here reads and then writes. A deferred one that another
	// process (the daemon vs. the CLI) writes under fails at once with SQLITE_BUSY, without waiting
	// for busy_timeout; an immediate one takes the write lock first and waits its turn.
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{DB: sdb, Path: path}
	if err := db.migrate(ctx, trash); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// Version returns the applied schema version (0 for a new database).
func (db *DB) Version(ctx context.Context) (int, error) {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (db *DB) migrate(ctx context.Context, trash platform.Trasher) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	cur, err := db.Version(ctx)
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("%w (schema %d, this binary knows %d): update noskill, or restore %s.bak-%d", ErrSchemaNewer, cur, len(ms), db.Path, len(ms))
	}
	if cur == len(ms) {
		return nil
	}
	if cur > 0 {
		if err := db.backup(ctx, cur, trash); err != nil {
			return fmt.Errorf("backup before migrating: %w", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, m := range ms[cur:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// backup copies the database to state.db.bak-<version> with VACUUM INTO (consistent under WAL)
// and moves backups beyond the last three to the trash.
func (db *DB) backup(ctx context.Context, version int, trash platform.Trasher) error {
	dst := fmt.Sprintf("%s.bak-%d", db.Path, version)
	if _, err := os.Stat(dst); err == nil {
		if trash == nil {
			return fmt.Errorf("%s already exists", dst)
		}
		if _, err := trash.Trash(dst); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return err
	}
	if trash == nil {
		return nil
	}
	matches, err := filepath.Glob(db.Path + ".bak-*")
	if err != nil {
		return err
	}
	ver := func(p string) int {
		n, err := strconv.Atoi(strings.TrimPrefix(p, db.Path+".bak-"))
		if err != nil {
			return -1
		}
		return n
	}
	var old []string
	for _, p := range matches {
		if ver(p) >= 0 { // only our own state.db.bak-<n> files
			old = append(old, p)
		}
	}
	sort.Slice(old, func(i, j int) bool { return ver(old[i]) > ver(old[j]) })
	for _, p := range old[min(keepBackups, len(old)):] {
		if _, err := trash.Trash(p); err != nil {
			return err
		}
	}
	return nil
}
