package chores

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// FileInfo is one file in a duplicate group.
type FileInfo struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified"`
}

// DupeGroup is files with the same content. Keep is the one to keep, by DupeKeeper's rules; the rest
// are candidates for the trash.
type DupeGroup struct {
	Size  int64      `json:"size"`
	Keep  string     `json:"keep"`
	Files []FileInfo `json:"files"`
}

// DupeResult is a scan.
type DupeResult struct {
	Groups      []DupeGroup `json:"groups"`
	Scanned     int         `json:"scanned"`
	WastedBytes int64       `json:"wasted_bytes"` // what the trash candidates add up to
	Truncated   string      `json:"truncated,omitempty"`
}

// DupeOptions bounds a scan, so a drive of millions of files can't hold the run for hours.
type DupeOptions struct {
	MinSize  int64         // smaller files are skipped (1 MB)
	MaxFiles int           // stop listing after this many (200000)
	Budget   time.Duration // stop hashing after this long (4 minutes)
}

func (o DupeOptions) withDefaults() DupeOptions {
	if o.MinSize <= 0 {
		o.MinSize = 1 << 20
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 200000
	}
	if o.Budget <= 0 {
		o.Budget = 4 * time.Minute
	}
	return o
}

var skipDirs = map[string]bool{"node_modules": true, ".git": true, "$RECYCLE.BIN": true, "System Volume Information": true}

// FindDuplicates finds files with identical content under roots. Size groups first, then a hash of
// the first and last 64 KB, then the whole file, so almost every non-duplicate costs one stat.
// Symlinks, hidden folders, app bundles and hard links to one file are skipped.
func FindDuplicates(ctx context.Context, roots []string, o DupeOptions) (DupeResult, error) {
	o = o.withDefaults()
	res := DupeResult{Groups: []DupeGroup{}}
	deadline := time.Now().Add(o.Budget)
	bySize := map[int64][]FileInfo{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil // unreadable: skipped
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if p != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()] || strings.HasSuffix(strings.ToLower(d.Name()), ".app")) {
					return fs.SkipDir
				}
				return nil
			}
			fi, err := d.Info()
			if err != nil || !fi.Mode().IsRegular() || fi.Size() < o.MinSize {
				return nil
			}
			if res.Scanned >= o.MaxFiles {
				res.Truncated = "stopped after listing " + strconvI(o.MaxFiles) + " files"
				return fs.SkipAll
			}
			res.Scanned++
			bySize[fi.Size()] = append(bySize[fi.Size()], FileInfo{Path: p, Size: fi.Size(), ModTime: fi.ModTime()})
			return nil
		})
		if err != nil && ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	sizes := make([]int64, 0, len(bySize))
	for s, fs := range bySize {
		if len(fs) > 1 {
			sizes = append(sizes, s)
		}
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] > sizes[j] }) // the biggest savings first
	for _, size := range sizes {
		if time.Now().After(deadline) {
			res.Truncated = "stopped hashing after " + o.Budget.String() + "; the biggest files were done first"
			break
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		files := dropSameFile(bySize[size])
		if len(files) < 2 {
			continue
		}
		for _, group := range groupByHash(files, headTailHash) {
			for _, g := range groupByHash(group, fullHash) {
				if len(g) < 2 {
					continue
				}
				keep := DupeKeeper(g)
				sort.Slice(g, func(i, j int) bool { return g[i].Path < g[j].Path })
				res.Groups = append(res.Groups, DupeGroup{Size: size, Keep: keep, Files: g})
				res.WastedBytes += size * int64(len(g)-1)
			}
		}
	}
	return res, nil
}

// dropSameFile removes hard links to a file already in the list: they take no extra space.
func dropSameFile(fs []FileInfo) []FileInfo {
	var out []FileInfo
	var seen []os.FileInfo
	for _, f := range fs {
		st, err := os.Stat(f.Path)
		if err != nil {
			continue
		}
		dup := false
		for _, s := range seen {
			if os.SameFile(s, st) {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, st)
			out = append(out, f)
		}
	}
	return out
}

func groupByHash(fs []FileInfo, h func(string, int64) (string, error)) [][]FileInfo {
	m := map[string][]FileInfo{}
	for _, f := range fs {
		k, err := h(f.Path, f.Size)
		if err != nil {
			continue
		}
		m[k] = append(m[k], f)
	}
	var out [][]FileInfo
	for _, g := range m {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	return out
}

func headTailHash(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	const n = 64 << 10
	if _, err := io.CopyN(h, f, min(size, n)); err != nil && err != io.EOF {
		return "", err
	}
	if size > 2*n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return "", err
		}
		if _, err := io.CopyN(h, f, n); err != nil && err != io.EOF {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fullHash(path string, _ int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var copyName = regexp.MustCompile(`(?i)( \(\d+\)| copy( \d+)?|-copy| - copy)(\.[^.]*)?$`)

// DupeKeeper picks the copy to keep: not in Downloads or a temp folder, not named like a copy
// ("report (1).pdf"), then the oldest, then the shortest path. The rule is plain so the user can
// predict it; the others are only candidates, and trashing is the user's approval.
func DupeKeeper(fs []FileInfo) string {
	score := func(f FileInfo) (int, time.Time, int) {
		s := 0
		low := strings.ToLower(filepath.ToSlash(f.Path))
		for _, bad := range []string{"/downloads/", "/tmp/", "/temp/", "/trash/", "/.trash/", "/desktop/"} {
			if strings.Contains(low, bad) {
				s += 2
			}
		}
		if copyName.MatchString(filepath.Base(f.Path)) {
			s++
		}
		return s, f.ModTime, len(f.Path)
	}
	best := 0
	for i := 1; i < len(fs); i++ {
		a, at, al := score(fs[i])
		b, bt, bl := score(fs[best])
		switch {
		case a != b:
			if a < b {
				best = i
			}
		case !at.Equal(bt):
			if at.Before(bt) {
				best = i
			}
		case al != bl:
			if al < bl {
				best = i
			}
		case fs[i].Path < fs[best].Path:
			best = i
		}
	}
	return fs[best].Path
}

// SameContent says whether two different files hold exactly the same bytes, and if not, why not.
// The trash tool checks it again at the moment of trashing: the files may have changed since the scan.
func SameContent(keep, other string) (bool, string) {
	a, err := os.Stat(keep)
	if err != nil {
		return false, "the copy to keep is gone"
	}
	b, err := os.Stat(other)
	if err != nil {
		return false, "it isn't there"
	}
	switch {
	case os.SameFile(a, b):
		return false, "it is the same file as the one to keep"
	case !b.Mode().IsRegular():
		return false, "not a plain file"
	case a.Size() != b.Size():
		return false, "its size differs"
	}
	h1, err1 := fullHash(keep, a.Size())
	h2, err2 := fullHash(other, b.Size())
	if err1 != nil || err2 != nil || h1 != h2 {
		return false, "its content differs"
	}
	return true, ""
}
