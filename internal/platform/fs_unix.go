//go:build !windows

package platform

import (
	"errors"
	"path/filepath"
	"runtime"
	"syscall"
)

// IsCrossDevice reports whether a rename failed because source and target are on different
// filesystems, so the caller must copy, verify and trash instead.
func IsCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// IsVolumeRoot reports whether p is a filesystem or removable-drive root, which an approval may
// never add as a whole write scope (SECURITY.md "Folders picked in approvals").
func IsVolumeRoot(p string) bool {
	p = filepath.Clean(p)
	if filepath.Dir(p) == p {
		return true
	}
	parent := filepath.Dir(p)
	switch runtime.GOOS {
	case "darwin":
		return parent == "/Volumes"
	default: // /mnt/X, /media/X, /media/<user>/X, /run/media/<user>/X
		if parent == "/mnt" || parent == "/media" {
			return true
		}
		gp := filepath.Dir(parent)
		return gp == "/media" || gp == "/run/media"
	}
}
