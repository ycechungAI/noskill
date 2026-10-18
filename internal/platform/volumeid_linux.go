package platform

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// VolumeUUID returns the filesystem UUID of the volume mounted at mount: its device from
// /proc/mounts, matched against the links in /dev/disk/by-uuid (PLATFORMS.md "External drives").
// "" with no error when the device has none.
func VolumeUUID(mount string) (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close()
	dev := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && strings.ReplaceAll(fields[1], `\040`, " ") == mount {
			dev = fields[0]
		}
	}
	if dev == "" || !strings.HasPrefix(dev, "/dev/") {
		return "", nil
	}
	if d, err := filepath.EvalSymlinks(dev); err == nil {
		dev = d
	}
	links, _ := os.ReadDir("/dev/disk/by-uuid")
	for _, l := range links {
		if t, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-uuid", l.Name())); err == nil && t == dev {
			return l.Name(), nil
		}
	}
	return "", nil
}
