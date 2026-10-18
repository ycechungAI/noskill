package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// AppNames names one desktop app the way each OS installs it, for FindApp.
type AppNames struct {
	Mac   []string // bundles, e.g. "Transmission.app"
	MacID []string // bundle ids for Spotlight, e.g. "org.m0k.transmission": finds copies outside the Applications folders
	Exe   []string // program names: PATH, ~/.local/bin, Flatpak/Snap exports (app id or snap name), AppImages
	Win   []string // paths under Program Files or %LOCALAPPDATA%\Programs, e.g. `qBittorrent\qbittorrent.exe`
	// Display is the name in Windows' installed-apps list (registry Uninstall keys), matched as a prefix.
	Display []string
}

// Install is one place an app is installed. An app can be installed both for one user
// (~/Applications, ~/.local, %LOCALAPPDATA%, HKCU) and for everyone (/Applications, /usr, Program Files, HKLM).
type Install struct {
	Path  string `json:"path"`
	Scope string `json:"scope"` // user | system
}

// scopeOf says whether a path is the user's own install or a system-wide one.
func scopeOf(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rel, err := filepath.Rel(h, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "user"
		}
	}
	return "system"
}

// installs collects unique paths in order, with their scope.
type installs struct {
	out  []Install
	seen map[string]bool
}

func (s *installs) add(p, scope string) {
	if p == "" {
		return
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	key := strings.ToLower(filepath.Clean(p))
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	if scope == "" {
		scope = scopeOf(p)
	}
	s.out = append(s.out, Install{Path: p, Scope: scope})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
