package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// FindApp returns every place an app is installed: for everyone (Program Files, HKLM's installed
// apps) and for this user (%LOCALAPPDATA%\Programs, %APPDATA%, HKCU's installed apps).
func FindApp(n AppNames) []Install {
	var s installs
	type root struct{ dir, scope string }
	var roots []root
	for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if d := os.Getenv(v); d != "" {
			roots = append(roots, root{d, "system"})
		}
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		roots = append(roots, root{filepath.Join(d, "Programs"), "user"}, root{d, "user"})
	}
	if d := os.Getenv("APPDATA"); d != "" {
		roots = append(roots, root{d, "user"}) // uTorrent and BitTorrent install per user here
	}
	for _, r := range roots {
		for _, w := range n.Win {
			p := filepath.Join(r.dir, w)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				s.add(p, r.scope)
			}
		}
	}
	for _, k := range []struct {
		key   registry.Key
		path  string
		scope string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "system"},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, "system"},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "user"},
	} {
		for _, p := range uninstallEntries(k.key, k.path, n.Display) {
			s.add(p, k.scope)
		}
	}
	for _, e := range n.Exe {
		if p, err := exec.LookPath(e); err == nil {
			s.add(p, "")
		}
	}
	return s.out
}

// uninstallEntries returns the install folders of installed apps whose display name starts with one of names.
func uninstallEntries(base registry.Key, path string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	k, err := registry.OpenKey(base, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	subs, _ := k.ReadSubKeyNames(-1)
	var out []string
	for _, sub := range subs {
		sk, err := registry.OpenKey(k, sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		name, _, _ := sk.GetStringValue("DisplayName")
		loc, _, _ := sk.GetStringValue("InstallLocation")
		icon, _, _ := sk.GetStringValue("DisplayIcon")
		sk.Close()
		for _, want := range names {
			if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(want)) {
				continue
			}
			switch {
			case loc != "" && exists(loc):
				out = append(out, loc)
			case icon != "":
				if p, _, _ := strings.Cut(strings.Trim(icon, `"`), ","); exists(p) {
					out = append(out, p)
				}
			}
		}
	}
	return out
}
