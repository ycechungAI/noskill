package platform

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ListVolumes lists what is mounted under /Volumes (external drives, disk images, network shares).
func ListVolumes() ([]VolumeInfo, error) {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil, err
	}
	var out []VolumeInfo
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") { // .timemachine and other system mounts
			continue
		}
		p := filepath.Join("/Volumes", e.Name())
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			continue // "Macintosh HD" is a link to /
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		out = append(out, VolumeInfo{Label: e.Name(), Mount: p, FSType: cstr(st.Fstypename[:]),
			FreeBytes: uint64(st.Bavail) * uint64(st.Bsize), TotalBytes: uint64(st.Blocks) * uint64(st.Bsize), Removable: true})
	}
	return out, nil
}

func cstr(b []int8) string {
	var s []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}

// DiskFree returns the bytes available to this user on the volume holding path.
func DiskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
