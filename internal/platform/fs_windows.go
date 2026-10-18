package platform

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// IsCrossDevice reports whether a rename failed because source and target are on different
// volumes, so the caller must copy, verify and trash instead.
func IsCrossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }

// IsVolumeRoot reports whether p is a drive root (C:\, D:\), which an approval may never add as a
// whole write scope.
func IsVolumeRoot(p string) bool {
	p = filepath.Clean(p)
	return filepath.Dir(p) == p
}
