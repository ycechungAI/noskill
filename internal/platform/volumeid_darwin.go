package platform

import (
	"context"
	"os/exec"
	"regexp"
	"time"
)

var volumeUUIDRe = regexp.MustCompile(`<key>VolumeUUID</key>\s*<string>([0-9A-Fa-f-]+)</string>`)

// VolumeUUID returns the UUID of the volume mounted at mount, from `diskutil info -plist`
// (PLATFORMS.md "External drives"). "" with no error when the volume has none (some network shares).
func VolumeUUID(mount string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", mount).Output()
	if err != nil {
		return "", err
	}
	if m := volumeUUIDRe.FindSubmatch(out); m != nil {
		return string(m[1]), nil
	}
	return "", nil
}
