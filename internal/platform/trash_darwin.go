package platform

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OSTrash moves files to the macOS Trash with /usr/bin/trash, which uses NSFileManager (so Finder's
// "Put Back" works and external drives use their own .Trashes). Older macOS without that tool falls
// back to asking Finder, which shows an Automation prompt the first time.
type OSTrash struct{}

func (OSTrash) Trash(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", err
	}
	if _, err := os.Stat("/usr/bin/trash"); err == nil {
		var out, errb bytes.Buffer
		// No "--": abs starts with "/", so it can't be read as a flag, and this trash takes "--"
		// as a file name and fails after trashing the real one (seen on macOS 27).
		cmd := exec.Command("/usr/bin/trash", "-v", abs)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("trash %s: %v: %s", abs, err, strings.TrimSpace(errb.String()))
		}
		return parseTrashVerbose(out.String() + errb.String()), nil
	}
	script := `on run argv
tell application "Finder" to set t to delete (POSIX file (item 1 of argv))
return POSIX path of (t as alias)
end run`
	out, err := exec.Command("/usr/bin/osascript", "-e", script, abs).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("trash %s via Finder: %v: %s", abs, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// parseTrashVerbose reads `# Moved "<from>" to "<to>"` from `trash -v`.
func parseTrashVerbose(s string) string {
	i := strings.LastIndex(s, `" to "`)
	if i < 0 {
		return ""
	}
	rest := s[i+len(`" to "`):]
	if j := strings.LastIndex(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}

// TrashRestored is called after a file was put back from the trash; the macOS Trash needs nothing.
func TrashRestored(string) {}
