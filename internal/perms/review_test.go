package perms

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"noskill/internal/platform"
)

// REVIEW_BLOCK3.md: a tool call can't move or trash a whole granted folder, only what is inside it.
func TestWholeGrantedFolderIsNeverMovedOrTrashed(t *testing.T) {
	root, s := tree(t)
	trash := t.TempDir()
	b := &Broker{RunID: "r", Perms: s, Undo: &memUndo{}, Trash: fakeTrash{trash}}
	ctx := context.Background()
	a := filepath.Join(root, "allowed")
	if err := b.TrashPath(ctx, a); !IsDenied(err) {
		t.Errorf("trash of the granted folder: %v", err)
	}
	if err := b.Move(ctx, a, filepath.Join(a, "inner")); !IsDenied(err) {
		t.Errorf("move of the granted folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a, "a.txt")); err != nil {
		t.Errorf("the folder was touched: %v", err)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 0 {
		t.Errorf("trash has %d entries", len(ents))
	}
	// Something inside is fine.
	mkfile(t, filepath.Join(a, "sub", "f.txt"), "f")
	if err := b.TrashPath(ctx, filepath.Join(a, "sub")); err != nil {
		t.Errorf("trash inside: %v", err)
	}
}

// REVIEW_BLOCK3.md: a link in a granted folder that leads out of it is not followed.
func TestLinksOutOfScopeAreNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symbolic links")
	}
	root, s := tree(t)
	b := &Broker{RunID: "r", Perms: s, Undo: &memUndo{}, Trash: fakeTrash{t.TempDir()}}
	ctx := context.Background()
	a := filepath.Join(root, "allowed")
	if err := b.Copy(ctx, filepath.Join(a, "link", "key"), filepath.Join(a, "stolen")); !IsDenied(err) {
		t.Errorf("copy through a link: %v", err)
	}
	if err := b.TrashPath(ctx, filepath.Join(a, "link", "key")); !IsDenied(err) {
		t.Errorf("trash through a link: %v", err)
	}
	if err := b.Copy(ctx, filepath.Join(a, "a.txt"), filepath.Join(a, "link", "planted")); !IsDenied(err) {
		t.Errorf("copy into a linked folder: %v", err)
	}
	if err := b.Mkdir(ctx, filepath.Join(a, "link", "new")); !IsDenied(err) {
		t.Errorf("mkdir through a link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secret", "planted")); err == nil {
		t.Error("a file was written outside the scope")
	}
}

// "Never overwrite" holds when the existing file differs only by case on a case-insensitive file system.
func TestCaseVariantIsTheSameFile(t *testing.T) {
	if !platform.CaseInsensitive() {
		t.Skip("this file system tells Episode.mkv and episode.mkv apart")
	}
	root, s := tree(t)
	b := &Broker{RunID: "r", Perms: s, Undo: &memUndo{}, Trash: fakeTrash{t.TempDir()}}
	a := filepath.Join(root, "allowed")
	mkfile(t, filepath.Join(a, "Episode.mkv"), "mine")
	if err := b.Copy(context.Background(), filepath.Join(a, "a.txt"), filepath.Join(a, "episode.mkv")); !errors.Is(err, ErrExists) {
		t.Errorf("copy over a case variant: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(a, "Episode.mkv")); string(got) != "mine" {
		t.Errorf("overwritten: %q", got)
	}
}

// A copy that died leaves "<name>.part". After a while it is moved to the trash so the next try can
// go ahead; a fresh one (a copy in progress) is left alone.
func TestDeadPartFileIsTrashedFreshOneIsNot(t *testing.T) {
	root, s := tree(t)
	trash := t.TempDir()
	undo := &memUndo{}
	b := &Broker{RunID: "r", Perms: s, Undo: undo, Trash: fakeTrash{trash}}
	ctx := context.Background()
	a := filepath.Join(root, "allowed")
	dst := filepath.Join(a, "ep.mkv")
	mkfile(t, dst+".part", "half")

	if err := b.Copy(ctx, filepath.Join(a, "a.txt"), dst); !errors.Is(err, ErrExists) {
		t.Fatalf("a fresh .part must block the copy: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(dst+".part", old, old)
	if err := b.Copy(ctx, filepath.Join(a, "a.txt"), dst); err != nil {
		t.Fatalf("copy after the .part went stale: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "a" {
		t.Errorf("copied %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(trash, "ep.mkv.part")); string(got) != "half" {
		t.Errorf("the dead .part isn't in the trash: %q", got)
	}
	var ops []string
	for _, e := range undo.e {
		ops = append(ops, e.Op)
	}
	if len(ops) != 2 || ops[0] != "trash" || ops[1] != "copy" {
		t.Errorf("undo log: %v", ops)
	}
}
