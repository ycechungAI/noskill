package learning

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DiffDirs is `noskill skills diff` (FR-S9): a unified diff of two versions' files, leaving out
// what the library manages. Binary files are only named.
func DiffDirs(a, b, aLabel, bLabel string) (string, error) {
	fa, err := versionFiles(a)
	if err != nil {
		return "", err
	}
	fb, err := versionFiles(b)
	if err != nil {
		return "", err
	}
	names := map[string]bool{}
	for n := range fa {
		names[n] = true
	}
	for n := range fb {
		names[n] = true
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	slices.Sort(sorted)
	var out strings.Builder
	for _, n := range sorted {
		x, inA := fa[n]
		y, inB := fb[n]
		if inA && inB && bytes.Equal(x, y) {
			continue
		}
		from, to := aLabel+"/"+n, bLabel+"/"+n
		if !inA {
			from = "/dev/null"
		}
		if !inB {
			to = "/dev/null"
		}
		if bytes.IndexByte(x, 0) >= 0 || bytes.IndexByte(y, 0) >= 0 {
			fmt.Fprintf(&out, "Binary files %s and %s differ\n", from, to)
			continue
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
		out.WriteString(unified(splitLines(string(x)), splitLines(string(y)), 3))
	}
	return out.String(), nil
}

func versionFiles(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if slices.Contains(managed, rel) || rel == ".note" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = b
		}
		return nil
	})
	return out, err
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
}

type edit struct {
	op   byte // ' ', '-', '+'
	line string
}

// unified renders a line diff with ctx lines of context. Skill files are small, so a plain LCS
// table is fine; above maxCells it falls back to "all removed, all added".
func unified(a, b []string, ctx int) string {
	const maxCells = 4 << 20
	var es []edit
	if len(a)*len(b) > maxCells {
		for _, l := range a {
			es = append(es, edit{'-', l})
		}
		for _, l := range b {
			es = append(es, edit{'+', l})
		}
	} else {
		es = lcsEdits(a, b)
	}
	var out strings.Builder
	for i := 0; i < len(es); {
		if es[i].op == ' ' {
			i++
			continue
		}
		start := max(0, i-ctx)
		end := i
		for end < len(es) { // extend the hunk while changes are within 2*ctx of each other
			if es[end].op != ' ' {
				end++
				continue
			}
			run := end
			for run < len(es) && es[run].op == ' ' {
				run++
			}
			if run == len(es) || run-end > 2*ctx {
				end = min(len(es), end+ctx)
				break
			}
			end = run
		}
		aStart, bStart := 1, 1
		for _, e := range es[:start] {
			if e.op != '+' {
				aStart++
			}
			if e.op != '-' {
				bStart++
			}
		}
		var aN, bN int
		for _, e := range es[start:end] {
			if e.op != '+' {
				aN++
			}
			if e.op != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aN, bStart, bN)
		for _, e := range es[start:end] {
			out.WriteByte(e.op)
			out.WriteString(strings.TrimSuffix(e.line, "\n"))
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func lcsEdits(a, b []string) []edit {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var es []edit
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			es = append(es, edit{' ', a[i]})
			i, j = i+1, j+1
		case t[i+1][j] >= t[i][j+1]:
			es = append(es, edit{'-', a[i]})
			i++
		default:
			es = append(es, edit{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		es = append(es, edit{'-', a[i]})
	}
	for ; j < m; j++ {
		es = append(es, edit{'+', b[j]})
	}
	return es
}

// DiffText is a unified diff of two texts, "" when they're the same.
func DiffText(a, b, aLabel, bLabel string) string {
	if a == b {
		return ""
	}
	return fmt.Sprintf("--- %s\n+++ %s\n", aLabel, bLabel) + unified(splitLines(a), splitLines(b), 3)
}
