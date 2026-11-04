package anime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"noskill/internal/store"
)

func TestBackfillFiledRecordsExistingEpisodes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := &State{DB: db, Now: func() time.Time { return time.Unix(0, 0) }}
	if err := st.PutTitle(ctx, TitleState{ID: 7, Title: "Romelia War Chronicle", Status: Approved, Dest: dir}); err != nil {
		t.Fatal(err)
	}
	show := filepath.Join(dir, "Romelia War Chronicle")
	os.MkdirAll(show, 0o755)
	for _, n := range []string{"Romelia War Chronicle S01E01 [1080p].mkv", "Romelia War Chronicle S01E02 [480p].mkv", "notes.txt"} {
		os.WriteFile(filepath.Join(show, n), []byte("x"), 0o644)
	}
	added, err := BackfillFiled(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added %v, want 2 episodes", added)
	}
	got, _, _ := st.Title(ctx, 7)
	if got.Filed["1"].Resolution != "1080p" || got.Filed["2"].Resolution != "480p" {
		t.Fatalf("filed %+v", got.Filed)
	}
	again, _ := BackfillFiled(ctx, st, dir)
	if len(again) != 0 {
		t.Fatalf("second run added %v, want none", again)
	}
}
