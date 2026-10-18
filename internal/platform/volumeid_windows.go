package platform

import (
	"strings"

	"golang.org/x/sys/windows"
)

// VolumeUUID returns the volume GUID of the drive at mount (e.g. `E:\`), from
// GetVolumeNameForVolumeMountPoint's `\\?\Volume{GUID}\` (PLATFORMS.md "External drives").
func VolumeUUID(mount string) (string, error) {
	if !strings.HasSuffix(mount, `\`) {
		mount += `\`
	}
	p, err := windows.UTF16PtrFromString(mount)
	if err != nil {
		return "", err
	}
	var name [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(p, &name[0], uint32(len(name))); err != nil {
		return "", err
	}
	s := windows.UTF16ToString(name[:])
	if i, j := strings.Index(s, "{"), strings.Index(s, "}"); i >= 0 && j > i {
		return s[i+1 : j], nil
	}
	return "", nil
}
