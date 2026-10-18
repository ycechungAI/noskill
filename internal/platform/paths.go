package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Dirs are noskill's own folders (PLATFORMS.md "Paths", DATA_MODEL.md "Storage layout").
// macOS keeps everything in ~/.noskill; Linux and Windows split config and data. NOSKILL_HOME puts
// everything in one folder on every OS.
type Dirs struct {
	Config string // config.toml
	Data   string // state.db, skills/, traces/, runs/, runtimes/, logs/, run/
}

// DefaultDirs returns the per-OS folders. home overrides both when non-empty (the --home flag or
// [general] home); otherwise NOSKILL_HOME, then the OS default.
func DefaultDirs(home string) (Dirs, error) {
	if home == "" || home == "auto" {
		home = os.Getenv("NOSKILL_HOME")
	}
	if home != "" {
		h, err := ExpandHome(home)
		if err != nil {
			return Dirs{}, err
		}
		h, err = filepath.Abs(h)
		if err != nil {
			return Dirs{}, err
		}
		return Dirs{Config: h, Data: h}, nil
	}
	return osDirs()
}

func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.toml") }
func (d Dirs) StateDB() string    { return filepath.Join(d.Data, "state.db") }
func (d Dirs) Skills() string     { return filepath.Join(d.Data, "skills") }
func (d Dirs) Learned() string    { return filepath.Join(d.Data, "learned") }
func (d Dirs) Tuned() string      { return filepath.Join(d.Data, "tuned") } // tuned built-in skills (ADR-0014)
func (d Dirs) Traces() string     { return filepath.Join(d.Data, "traces") }
func (d Dirs) Runs() string       { return filepath.Join(d.Data, "runs") }
func (d Dirs) Runtimes() string   { return filepath.Join(d.Data, "runtimes") }
func (d Dirs) Logs() string       { return filepath.Join(d.Data, "logs") }

// IPC is the CLI ↔ daemon endpoint: a Unix socket, or a named pipe on Windows.
func (d Dirs) IPC() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\noskill`
	}
	return filepath.Join(d.Data, "run", "noskill.sock")
}

// Own returns noskill's folders. Approved folders inside them are refused (SECURITY.md
// "Folders picked in approvals").
func (d Dirs) Own() []string {
	if d.Config == d.Data {
		return []string{d.Data}
	}
	return []string{d.Config, d.Data}
}

// Ensure creates the data folders noskill writes to.
func (d Dirs) Ensure() error {
	for _, p := range []string{d.Config, d.Data, d.Skills(), d.Learned(), d.Traces(), d.Runs(), d.Runtimes(), d.Logs()} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
	}
	if runtime.GOOS != "windows" {
		return os.MkdirAll(filepath.Dir(d.IPC()), 0o700)
	}
	return nil
}

// ExpandHome replaces a leading "~" with the home folder.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, p[1:]), nil
}

// FolderTokens are the portable path tokens skills may use (PLATFORMS.md "Paths").
var FolderTokens = []string{"$DOWNLOADS", "$DOCUMENTS", "$DESKTOP", "$PICTURES", "$VIDEOS", "$MUSIC", "$TEMP"}

// KnownFolder resolves one portable token ("$DOWNLOADS", …) to this user's folder.
func KnownFolder(token string) (string, error) {
	if token == "$TEMP" {
		return os.TempDir(), nil
	}
	p, err := knownFolder(token)
	if err != nil {
		return "", fmt.Errorf("%s: %w", token, err)
	}
	return p, nil
}

// CaseInsensitive reports whether paths on this OS's default filesystem compare without case
// (APFS and NTFS defaults). Path checks fold case here so "~/DOWNLOADS" and "~/Downloads" are the
// same folder for both allow and refuse decisions.
func CaseInsensitive() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

// SystemDirs are OS folders an approval may never add as a write scope.
func SystemDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/System", "/Library", "/Applications", "/usr", "/bin", "/sbin", "/private", "/etc", "/var", "/opt", "/cores", "/dev"}
	case "windows":
		var out []string
		for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData", "ProgramW6432"} {
			if v := os.Getenv(env); v != "" {
				out = append(out, v)
			}
		}
		return out
	default:
		return []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/usr", "/var", "/snap"}
	}
}
