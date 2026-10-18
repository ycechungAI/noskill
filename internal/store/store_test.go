package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// trashList records what would have gone to the trash and moves it aside (tests never touch the real trash).
type trashList struct{ got []string }

func (t *trashList) Trash(p string) (string, error) {
	t.got = append(t.got, filepath.Base(p))
	dst := p + ".trashed"
	return dst, os.Rename(p, dst)
}

func TestOpenMigratesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := db.Version(ctx)
	if v != Latest() {
		t.Fatalf("version = %d, want %d", v, Latest())
	}
	for _, tbl := range []string{"backends", "models", "model_bench", "eval_runs", "eval_results", "bandit", "decision_stats", "skills", "skill_versions",
		"skill_stats", "skill_vectors", "skill_kv", "runs", "run_skills", "undo_log", "learning_queue", "eval_results",
		"jobs", "job_runs", "approvals", "memory", "memory_vectors", "volumes", "schema_migrations"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing", tbl)
		}
	}
	var mode string
	db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %s", mode)
	}
	db.Close()
	db, err = Open(ctx, path, nil) // no pending migrations: no backup, no error
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if m, _ := filepath.Glob(path + ".bak-*"); len(m) != 0 {
		t.Errorf("unexpected backups %v", m)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (999, 0)`)
	db.Close()
	if _, err := Open(ctx, path, nil); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("err = %v, want ErrSchemaNewer", err)
	}
}

func TestBackupKeepsThreeAndTrashesTheRest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tr := &trashList{}
	for v := 1; v <= 5; v++ {
		if err := db.backup(ctx, v, tr); err != nil {
			t.Fatal(err)
		}
	}
	left, _ := filepath.Glob(path + ".bak-*")
	var kept []string
	for _, p := range left {
		if filepath.Ext(p) != ".trashed" {
			kept = append(kept, filepath.Base(p))
		}
	}
	want := []string{"state.db.bak-3", "state.db.bak-4", "state.db.bak-5"}
	if fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
	if len(tr.got) != 2 {
		t.Errorf("trashed %v, want bak-1 and bak-2", tr.got)
	}
}

func TestRunsAndUndoLog(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := Run{ID: "01RUN", Source: "cli", Status: "running", StartedAt: time.Now()}
	if err := db.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Status, r.Steps, r.EndedAt = "done", 3, time.Now()
	if err := db.FinishRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"move", "trash"} {
		if err := db.AppendUndo(ctx, UndoEntry{RunID: "01RUN", Op: op, From: "/a", To: "/b"}); err != nil {
			t.Fatal(err)
		}
	}
	es, err := db.UndoEntries(ctx, "01RUN")
	if err != nil || len(es) != 2 || es[0].Seq != 1 || es[1].Op != "trash" {
		t.Fatalf("entries = %+v, err = %v", es, err)
	}
}
