package platform

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// uniqueDirs returns the sorted, de-duplicated parent folders of paths, skipping the ones under
// skip prefixes (system libraries the sandbox profile already allows).
func uniqueDirs(paths []string, skip []string) []string {
	seen := map[string]bool{}
	for _, p := range paths {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		d := filepath.Dir(p)
		skipIt := false
		for _, s := range skip {
			if d == s || strings.HasPrefix(d, s+string(os.PathSeparator)) {
				skipIt = true
			}
		}
		if !skipIt {
			seen[d] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
