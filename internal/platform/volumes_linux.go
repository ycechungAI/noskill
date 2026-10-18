package platform

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ListVolumes lists mounts under /media, /run/media and /mnt (read from /proc/mounts).
func ListVolumes() ([]VolumeInfo, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []VolumeInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		mount := strings.ReplaceAll(fields[1], `\040`, " ")
		if !strings.HasPrefix(mount, "/media/") && !strings.HasPrefix(mount, "/run/media/") && !strings.HasPrefix(mount, "/mnt/") {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		out = append(out, VolumeInfo{Label: filepath.Base(mount), Mount: mount, FSType: fields[2],
			FreeBytes: st.Bavail * uint64(st.Bsize), TotalBytes: st.Blocks * uint64(st.Bsize), Removable: true})
	}
	return out, sc.Err()
}

// DiskFree returns the bytes available to this user on the volume holding path.
func DiskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
