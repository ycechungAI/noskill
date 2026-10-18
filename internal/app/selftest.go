package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"noskill/internal/ids"
	"noskill/internal/perms"
)

// The file self-test (Phase 3 exit on Windows and Linux, docs/REVIEW_BLOCK3.md): one real move, copy,
// trash and `noskill undo` in a folder of noskill's own, so the check that was done by hand on macOS
// is one command anywhere: `noskill doctor --files`. It uses the real OS trash.

// SelfTestStep is one line of the self-test.
type SelfTestStep struct {
	Name   string `json:"step"`
	Status string `json:"status"` // ok | warn | failed
	Detail string `json:"detail"`
}

// FilesSelfTest runs the self-test. The folder it works in is <data>/selftest-<id>; at the end the
// folder itself goes to the trash too (nothing noskill makes is deleted for good).
func (a *App) FilesSelfTest(ctx context.Context) []SelfTestStep {
	var out []SelfTestStep
	add := func(name, status, format string, args ...any) bool {
		out = append(out, SelfTestStep{name, status, fmt.Sprintf(format, args...)})
		return status != "failed"
	}
	runID := "selftest-" + ids.New()
	dir := filepath.Join(a.Dirs.Data, runID)
	canon := func(p string) string {
		if c, err := perms.Canonical(p); err == nil {
			return c
		}
		return p
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		add("folder", "failed", "can't make %s: %v", dir, err)
		return out
	}
	dir = canon(dir)
	add("folder", "ok", "working in %s", dir)

	scopes, err := perms.Resolver{}.Compile([]string{"write:" + dir})
	if err != nil {
		add("scope", "failed", "%v", err)
		return out
	}
	b := &perms.Broker{RunID: runID, Perms: perms.Set{Files: scopes}, Trash: a.Trash, Undo: a.DB}
	orig := []byte("noskill file self-test " + runID + "\n")
	pa, pb, pc := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt"), filepath.Join(dir, "c.txt")
	same := func(p string) bool { got, err := os.ReadFile(p); return err == nil && bytes.Equal(got, orig) }
	gone := func(p string) bool { _, err := os.Lstat(p); return err != nil }

	if err := os.WriteFile(pa, orig, 0o600); err != nil {
		add("create", "failed", "%v", err)
		return out
	}
	steps := []struct {
		name string
		do   func() error
		want func() bool
		said string
	}{
		{"move", func() error { return b.Move(ctx, pa, pb) }, func() bool { return same(pb) && gone(pa) }, "a.txt → b.txt, source gone"},
		{"copy", func() error { return b.Copy(ctx, pb, pc) }, func() bool { return same(pc) && same(pb) }, "b.txt → c.txt, checked against the original"},
		{"trash", func() error { return b.TrashPath(ctx, pc) }, func() bool { return gone(pc) }, "c.txt went to the OS trash"},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			add(s.name, "failed", "%v", err)
			return out
		}
		if !s.want() {
			add(s.name, "failed", "it reported success but the files aren't as expected")
			return out
		}
		add(s.name, "ok", "%s", s.said)
	}

	// Where did the trash put it? Windows' Recycle Bin doesn't say, so noskill can't restore from it.
	entries, _ := a.DB.UndoEntries(ctx, runID)
	trashKnown := len(entries) > 0 && entries[len(entries)-1].Op == "trash" && entries[len(entries)-1].To != ""
	if !trashKnown {
		add("trash location", "warn", "this OS doesn't say where its trash put the file, so `noskill undo` can't restore a trashed file here: restore it from the Recycle Bin or Trash by hand")
	} else {
		add("trash location", "ok", "recorded: %s", entries[len(entries)-1].To)
	}

	// Undo, for real.
	undone, err := a.Undo(ctx, runID, false)
	if err != nil {
		add("undo", "failed", "%v", err)
		return out
	}
	var notDone, unexpected []string
	for _, s := range undone {
		if !s.Done {
			notDone = append(notDone, fmt.Sprintf("step %d (%s): %s", s.Seq, s.Op, s.Note))
			// Where the trash doesn't say where it put c.txt, undoing the trash can't restore it, and
			// undoing the copy then finds nothing to put in the trash: those two are expected.
			if trashKnown || (s.Op != "trash" && s.Op != "copy") {
				unexpected = append(unexpected, notDone[len(notDone)-1])
			}
		}
	}
	switch {
	case len(unexpected) > 0:
		add("undo", "failed", "%d step(s) weren't undone: %v", len(unexpected), unexpected)
	default:
		add("undo", "ok", "%d of %d step(s) undone", len(undone)-len(notDone), len(undone))
	}
	if same(pa) && gone(pb) && gone(pc) {
		add("after undo", "ok", "a.txt is back with its original content; b.txt and c.txt are gone")
	} else {
		add("after undo", "failed", "the files aren't as they were: a.txt back=%v, b.txt gone=%v, c.txt gone=%v", same(pa), gone(pb), gone(pc))
	}

	// Tidy up: the folder goes to the trash as well.
	if _, err := a.Trash.Trash(dir); err != nil {
		add("tidy up", "warn", "couldn't move %s to the trash: %v (it holds one small text file)", dir, err)
	} else {
		add("tidy up", "ok", "the test folder is in the trash (%s)", time.Now().Format("15:04:05"))
	}
	return out
}
