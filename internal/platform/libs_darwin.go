package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// LibraryDirs lists the folders holding the shared libraries an executable loads, other than
// system ones: the sandbox profile must allow reading them (Homebrew's Node needs about 14,
// S9). Libraries are followed two levels deep.
func LibraryDirs(exe string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := map[string]bool{exe: true}
	var found []string
	frontier := []string{exe}
	for depth := 0; depth < 3 && len(frontier) > 0; depth++ {
		var next []string
		for _, f := range frontier {
			out, err := exec.CommandContext(ctx, "/usr/bin/otool", "-L", f).Output()
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(out), "\n")[1:] {
				lib, _, _ := strings.Cut(strings.TrimSpace(line), " (")
				if !strings.HasPrefix(lib, "/") || seen[lib] {
					continue // @rpath and friends resolve to a folder that is already listed
				}
				seen[lib] = true
				found = append(found, lib)
				next = append(next, lib)
			}
		}
		frontier = next
	}
	return uniqueDirs(found, []string{"/usr/lib", "/System"})
}
