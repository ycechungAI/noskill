package platform

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// LibraryDirs lists the folders holding the shared libraries an executable loads. Linux has no
// script sandbox yet (S9 open); this feeds its Landlock read rules when it lands.
func LibraryDirs(exe string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ldd", exe).Output()
	if err != nil {
		return nil
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(line, "=>"); ok {
			if p, _, _ := strings.Cut(strings.TrimSpace(rest), " "); strings.HasPrefix(p, "/") {
				found = append(found, p)
			}
		}
	}
	return uniqueDirs(found, nil)
}
