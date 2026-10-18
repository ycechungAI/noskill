// Package perms is the in-process permission broker (SECURITY.md "Enforcement layers", FR-S6):
// every path and host a built-in tool touches is checked here before the operation, the same way
// on every OS.
package perms

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"noskill/internal/platform"
)

// Denied is returned for every refused operation. Tools show Reason to the model; the trace gets
// all of it.
type Denied struct {
	Op     string // read | write | host | scope
	Target string
	Reason string
}

func (d *Denied) Error() string {
	return fmt.Sprintf("permission denied: %s %s: %s", d.Op, d.Target, d.Reason)
}

func deny(op, target, format string, a ...any) error {
	return &Denied{Op: op, Target: target, Reason: fmt.Sprintf(format, a...)}
}

// IsDenied reports whether err is a permission refusal.
func IsDenied(err error) bool {
	var d *Denied
	return errors.As(err, &d)
}

// Canonical returns p as an absolute, cleaned path with every symlink (and Windows junction) in
// its existing part resolved. The part that doesn't exist yet is appended as is; it can't contain
// ".." after cleaning. A dangling symlink anywhere in p is refused, since creating through it would
// write wherever it points.
func Canonical(p string) (string, error) {
	if err := rawCheck(p); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	dir, rest := abs, ""
	for {
		r, err := filepath.EvalSymlinks(dir)
		if err == nil {
			out := filepath.Join(r, rest)
			return out, rawCheck(out)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(dir); lerr == nil {
			return "", deny("path", p, "%s is a link to something that doesn't exist", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// rawCheck refuses path forms the checks below can't reason about (SECURITY.md security suite:
// UNC paths, device namespaces, alternate data streams).
func rawCheck(p string) error {
	if p == "" {
		return deny("path", p, "empty path")
	}
	if strings.ContainsRune(p, 0) {
		return deny("path", p, "NUL in path")
	}
	if runtime.GOOS != "windows" {
		return nil
	}
	s := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(s, `\\`) {
		return deny("path", p, `UNC and \\?\ paths aren't allowed`)
	}
	vol := filepath.VolumeName(s)
	if strings.Contains(s[len(vol):], ":") {
		return deny("path", p, "alternate data streams aren't allowed")
	}
	return nil
}

// fold makes comparisons case-insensitive where the default filesystem is (APFS, NTFS), so a
// case variant can neither escape a scope nor dodge a refusal.
func fold(p string) string {
	if platform.CaseInsensitive() {
		return strings.ToLower(p)
	}
	return p
}

// Within reports whether canonical path p is root or inside it.
func Within(p, root string) bool {
	p, root = fold(p), fold(root)
	if p == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(p, root)
}
