// Package sandbox runs script tools (SECURITY.md "Script tools"): a copy-in workspace, the egress
// proxy and the OS sandbox from internal/platform, under the skill's permissions (FR-S4, FR-S6).
package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"noskill/internal/perms"
	"noskill/internal/platform"
)

// Workspace is runs/<run-id>/<call-id>/: the sandbox profile lives in Dir, the script sees only
// Work = Dir/work with in/ (read-only inputs), out/ (results), tmp/ and home/.
//
// Workspaces are kept after the call (traces point at them); retention and cleanup come with
// `noskill gc` in a later block.
type Workspace struct {
	Dir, Work, In, Out, Tmp, Home string
}

func NewWorkspace(runsDir, runID, callID string) (*Workspace, error) {
	for _, s := range []string{runID, callID} {
		if s == "" || strings.ContainsAny(s, `/\:.`) {
			return nil, fmt.Errorf("workspace: bad id %q", s)
		}
	}
	dir := filepath.Join(runsDir, runID, callID)
	w := &Workspace{Dir: dir, Work: filepath.Join(dir, "work")}
	w.In, w.Out, w.Tmp, w.Home = filepath.Join(w.Work, "in"), filepath.Join(w.Work, "out"), filepath.Join(w.Work, "tmp"), filepath.Join(w.Work, "home")
	if err := os.MkdirAll(w.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(w.Work, 0o700); err != nil {
		return nil, fmt.Errorf("workspace %s: %w", w.Work, err) // never reuse a call's folder
	}
	for _, d := range []string{w.In, w.Out, w.Tmp, w.Home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// AddInput copies a file or folder the skill may read into in/<name>, cloning where the
// filesystem can, and makes the copy read-only. Symlinks inside a folder are skipped: the script
// gets content, never a way back to the real tree. It returns the path the script should use,
// relative to Work.
func (w *Workspace) AddInput(src, name string) (string, error) {
	if name == "" {
		name = filepath.Base(src)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("input name %q", name)
	}
	dst := filepath.Join(w.In, name)
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		if _, err := platform.CloneOrCopy(src, dst); err != nil {
			return "", err
		}
		return filepath.Join("in", name), os.Chmod(dst, 0o444)
	}
	var dirs []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			dirs = append(dirs, t)
			return os.Mkdir(t, 0o700)
		case d.Type().IsRegular():
			if _, err := platform.CloneOrCopy(p, t); err != nil {
				return err
			}
			return os.Chmod(t, 0o444)
		}
		return nil // symlinks, devices, sockets
	})
	if err != nil {
		return "", err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chmod(dirs[i], 0o555)
	}
	return filepath.Join("in", name), nil
}

// Outputs lists the regular files the script left in out/, relative to it. Links and other special
// files are reported separately and never applied.
func (w *Workspace) Outputs() (files, skipped []string, err error) {
	err = filepath.WalkDir(w.Out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == w.Out || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.Out, p)
		if d.Type().IsRegular() {
			files = append(files, rel)
		} else {
			skipped = append(skipped, rel)
		}
		return nil
	})
	return files, skipped, err
}

// ApplyOutput copies one file from out/ to the path the script asked for, through the broker:
// the destination must be in the skill's write scopes, nothing is overwritten, and the copy is in
// the undo log. rel is relative to out/ and may not leave it.
func (w *Workspace) ApplyOutput(ctx context.Context, b *perms.Broker, rel, to string) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output %q isn't inside out/", rel)
	}
	src := filepath.Join(w.Out, rel)
	fi, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("output %q: %w", rel, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("output %q isn't a regular file", rel)
	}
	out, err := perms.Canonical(w.Out)
	if err != nil {
		return err
	}
	withOut := *b
	withOut.Perms.Files = append(append([]perms.Scope{}, b.Perms.Files...), perms.Scope{Access: perms.Read, Root: out, Decl: "read:<workspace out>"})
	if err := withOut.Mkdir(ctx, filepath.Dir(to)); err != nil {
		return err
	}
	return withOut.Copy(ctx, src, to)
}

// ApplyOutputs copies out/ into dest through the broker (SECURITY.md "Copy-in workspace" step 4):
// dest must be in the skill's write scopes, nothing is overwritten, and every file is in the undo
// log.
func (w *Workspace) ApplyOutputs(ctx context.Context, b *perms.Broker, dest string) ([]string, error) {
	files, _, err := w.Outputs()
	if err != nil {
		return nil, err
	}
	out, err := perms.Canonical(w.Out)
	if err != nil {
		return nil, err
	}
	withOut := *b
	withOut.Perms.Files = append(append([]perms.Scope{}, b.Perms.Files...), perms.Scope{Access: perms.Read, Root: out, Decl: "read:<workspace out>"})
	var applied []string
	for _, rel := range files {
		to := filepath.Join(dest, rel)
		if err := withOut.Mkdir(ctx, filepath.Dir(to)); err != nil {
			return applied, err
		}
		if err := withOut.Copy(ctx, filepath.Join(w.Out, rel), to); err != nil {
			return applied, err
		}
		applied = append(applied, to)
	}
	return applied, nil
}

// Remove deletes a workspace noskill made for itself (a draft script's tryout, LEARNING.md §3
// pass d). Its input copies are read-only, so they are made writable first. The copies are
// noskill's own scratch; the real inputs are never touched.
func (w *Workspace) Remove() error {
	filepath.WalkDir(w.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink == 0 {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(w.Dir)
}
