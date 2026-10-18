package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"noskill/internal/platform"
	"noskill/internal/store"
)

// UndoStep is one reverted (or revertable) file effect.
type UndoStep struct {
	Seq    int    `json:"seq"`
	Op     string `json:"op"`
	Action string `json:"action"` // what undoing it means, in words
	Done   bool   `json:"done"`
	Note   string `json:"note,omitempty"` // why it was skipped
}

// Undo reverts a run's file effects, newest first. It only restores or trashes: moves go back,
// copies and created files go to the trash (never deleted), folders noskill made go to the trash
// when empty, and trashed files are restored from the trash. A file that changed after noskill
// wrote it, or a place that's now occupied, is left alone and reported. dryRun lists the plan.
func (a *App) Undo(ctx context.Context, runID string, dryRun bool) ([]UndoStep, error) {
	// A run id with file effects but no run row is a background job's (the anime tracker files under
	// its own id), and can be undone the same way.
	if _, err := a.DB.GetRun(ctx, runID); err != nil {
		if es, uerr := a.DB.UndoEntries(ctx, runID); uerr != nil || len(es) == 0 {
			return nil, fmt.Errorf("no such run %q", runID)
		}
	}
	entries, err := a.DB.UndoPending(ctx, runID)
	if err != nil {
		return nil, err
	}
	var steps []UndoStep
	for _, e := range entries {
		st := UndoStep{Seq: e.Seq, Op: e.Op}
		var skip, did string
		switch e.Op {
		case "move":
			did = fmt.Sprintf("move %s back to %s", e.To, e.From)
			skip = checkMoveBack(e)
		case "copy", "create":
			did = fmt.Sprintf("move the %s %s to the trash", map[string]string{"copy": "copy", "create": "new file"}[e.Op], e.To)
			skip = checkUnchanged(e)
		case "mkdir":
			did = fmt.Sprintf("move the empty folder %s to the trash", e.To)
			skip = checkEmptyDir(e.To)
		case "trash":
			did = fmt.Sprintf("restore %s from the trash to %s", e.From, e.From)
			skip = checkRestore(e)
		default:
			skip = "unknown operation " + e.Op
		}
		st.Action = did
		if skip != "" {
			st.Note = skip
			steps = append(steps, st)
			continue
		}
		if dryRun {
			steps = append(steps, st)
			continue
		}
		var err error
		switch e.Op {
		case "move":
			err = os.MkdirAll(filepath.Dir(e.From), 0o755)
			if err == nil {
				err = os.Rename(e.To, e.From)
			}
		case "copy", "create", "mkdir":
			_, err = a.Trash.Trash(e.To)
		case "trash":
			err = os.MkdirAll(filepath.Dir(e.From), 0o755)
			if err == nil {
				if err = os.Rename(e.To, e.From); err == nil {
					platform.TrashRestored(e.To)
				}
			}
		}
		if err != nil {
			if platform.IsCrossDevice(err) {
				st.Note = "it's on another drive now; move it back by hand"
			} else {
				st.Note = err.Error()
			}
			steps = append(steps, st)
			continue
		}
		st.Done = true
		a.DB.MarkUndone(ctx, runID, e.Seq)
		steps = append(steps, st)
	}
	return steps, nil
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func checkMoveBack(e store.UndoEntry) string {
	switch {
	case !exists(e.To):
		return "the moved file isn't at " + e.To + " any more"
	case exists(e.From):
		return "something is at " + e.From + " now; not replacing it"
	}
	return ""
}

func checkUnchanged(e store.UndoEntry) string {
	fi, err := os.Lstat(e.To)
	if err != nil {
		return "already gone"
	}
	if e.Hash != "" && fi.Mode().IsRegular() {
		f, err := os.Open(e.To)
		if err != nil {
			return err.Error()
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err.Error()
		}
		if hex.EncodeToString(h.Sum(nil)) != e.Hash {
			return "changed since noskill made it; left in place"
		}
	}
	return ""
}

func checkEmptyDir(p string) string {
	ents, err := os.ReadDir(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "already gone"
	case err != nil:
		return err.Error()
	case len(ents) > 0:
		return "not empty; left in place"
	}
	return ""
}

func checkRestore(e store.UndoEntry) string {
	switch {
	case e.To == "":
		return "this system doesn't say where the trash put it; restore it from the Trash or Recycle Bin"
	case !exists(e.To):
		return "no longer in the trash (emptied or restored already)"
	case exists(e.From):
		return "something is at " + e.From + " now; not replacing it"
	}
	return ""
}
