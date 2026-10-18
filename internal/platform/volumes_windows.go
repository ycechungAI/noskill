package platform

import (
	"golang.org/x/sys/windows"
)

// ListVolumes lists fixed and removable drives with their labels. Untested on a real Windows
// machine (S7/S8 Windows haven't run); it uses the documented Win32 calls only.
func ListVolumes() ([]VolumeInfo, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var out []VolumeInfo
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rp, _ := windows.UTF16PtrFromString(root)
		typ := windows.GetDriveType(rp)
		if typ != windows.DRIVE_FIXED && typ != windows.DRIVE_REMOVABLE {
			continue
		}
		var label, fs [windows.MAX_PATH + 1]uint16
		windows.GetVolumeInformation(rp, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs)))
		var free, total, totalFree uint64
		windows.GetDiskFreeSpaceEx(rp, &free, &total, &totalFree)
		out = append(out, VolumeInfo{Label: windows.UTF16ToString(label[:]), Mount: root, FSType: windows.UTF16ToString(fs[:]),
			FreeBytes: free, TotalBytes: total, Removable: typ == windows.DRIVE_REMOVABLE || i > 2})
	}
	return out, nil
}

// DiskFree returns the bytes available to this user on the volume holding path.
func DiskFree(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
