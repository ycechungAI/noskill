package platform

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FindApp returns every place an app is installed, for this user (~/.local/bin, ~/bin, user Flatpak,
// ~/Applications AppImages, ~/.local/share/applications) and for everyone (PATH, /opt, system
// Flatpak, Snap, /usr/share/applications). Flatpak and Snap launchers are named by app id or snap
// name, so list those in Exe.
func FindApp(n AppNames) []Install {
	var s installs
	h, _ := os.UserHomeDir()
	var userBins, userApps []string
	if h != "" {
		userBins = []string{filepath.Join(h, ".local", "bin"), filepath.Join(h, "bin"),
			filepath.Join(h, ".local", "share", "flatpak", "exports", "bin")}
		userApps = []string{filepath.Join(h, ".local", "share", "applications")}
	}
	systemBins := []string{"/usr/bin", "/usr/local/bin", "/var/lib/flatpak/exports/bin", "/snap/bin"}
	for _, e := range n.Exe {
		for _, d := range userBins {
			if p := filepath.Join(d, e); exists(p) {
				s.add(p, "user")
			}
		}
		for _, d := range systemBins {
			if p := filepath.Join(d, e); exists(p) {
				s.add(p, "system")
			}
		}
		if p, err := exec.LookPath(e); err == nil {
			s.add(p, "")
		}
	}
	// AppImages the user downloaded, and apps in /opt.
	var dirs []string
	if h != "" {
		dirs = append(dirs, filepath.Join(h, "Applications"), filepath.Join(h, "Downloads"))
	}
	dirs = append(dirs, "/opt")
	for _, d := range dirs {
		ents, _ := os.ReadDir(d)
		for _, ent := range ents {
			name := strings.ToLower(ent.Name())
			for _, e := range n.Exe {
				if strings.HasPrefix(name, strings.ToLower(e)) && (strings.HasSuffix(name, ".appimage") || d == "/opt") {
					s.add(filepath.Join(d, ent.Name()), "")
				}
			}
		}
	}
	// Desktop entries: an app installed some other way still has a launcher.
	for _, d := range append(userApps, "/usr/share/applications", "/usr/local/share/applications") {
		ents, _ := os.ReadDir(d)
		for _, ent := range ents {
			if !strings.HasSuffix(ent.Name(), ".desktop") {
				continue
			}
			if exe := desktopExec(filepath.Join(d, ent.Name())); exe != "" {
				for _, e := range n.Exe {
					if filepath.Base(exe) == e {
						if p, err := exec.LookPath(exe); err == nil {
							s.add(p, "")
						} else if filepath.IsAbs(exe) && exists(exe) {
							s.add(exe, "")
						}
					}
				}
			}
		}
	}
	return s.out
}

// desktopExec returns the program a .desktop file starts.
func desktopExec(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Exec="); ok {
			if fs := strings.Fields(v); len(fs) > 0 {
				if fs[0] == "env" && len(fs) > 1 { // Exec=env VAR=1 prog
					for _, w := range fs[1:] {
						if !strings.Contains(w, "=") {
							return w
						}
					}
				}
				return fs[0]
			}
		}
	}
	return ""
}
