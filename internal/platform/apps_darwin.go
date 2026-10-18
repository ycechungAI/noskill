package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FindApp returns every place an app is installed: /Applications (everyone), ~/Applications (this
// user), anywhere else Spotlight knows the bundle id, and Homebrew or other programs on PATH.
func FindApp(n AppNames) []Install {
	var s installs
	h, _ := os.UserHomeDir()
	for _, b := range n.Mac {
		if p := filepath.Join("/Applications", b); exists(p) {
			s.add(p, "system")
		}
		if h != "" {
			if p := filepath.Join(h, "Applications", b); exists(p) {
				s.add(p, "user")
			}
		}
	}
	for _, id := range n.MacID {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := exec.CommandContext(ctx, "/usr/bin/mdfind", "kMDItemCFBundleIdentifier == '"+strings.ReplaceAll(id, "'", "")+"'").Output()
		cancel()
		if err != nil {
			continue
		}
		for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasSuffix(p, ".app") && !strings.Contains(p, "/.Trash/") && !strings.Contains(p, ".app/") {
				s.add(p, "")
			}
		}
	}
	for _, e := range n.Exe {
		if p, err := exec.LookPath(e); err == nil {
			s.add(p, "")
		}
	}
	return s.out
}
