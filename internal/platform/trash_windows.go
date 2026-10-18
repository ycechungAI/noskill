package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OSTrash moves files to the Recycle Bin with SHFileOperationW(FO_DELETE, FOF_ALLOWUNDO).
// Not yet run on Windows (REVIEW_BLOCK3.md). The shell deletes for real, without recycling, when a
// drive has no Recycle Bin (some USB sticks, network shares), when the path is longer than MAX_PATH,
// and when the item is bigger than the bin allows; the first two are refused here, and the third
// is left to the shell's own "permanently delete?" prompt, which FOF_NOCONFIRMATION would have
// answered yes to. Without a user to answer it, the call fails and the file stays.
type OSTrash struct{}

var (
	shell32         = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOpW   = shell32.NewProc("SHFileOperationW")
	kernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procGetDriveTyp = kernel32.NewProc("GetDriveTypeW")
)

type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

const (
	foDelete           = 0x0003
	fofSilent          = 0x0004
	fofAllowUndo       = 0x0040
	fofNoErrorUI       = 0x0400
	fofWantNukeWarning = 0x4000
	driveFixed         = 3
)

func (OSTrash) Trash(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", err
	}
	if len(abs) >= 248 { // MAX_PATH less room for the shell's own handling
		return "", fmt.Errorf("trash %s: the path is too long for the Recycle Bin (it would be deleted for real)", abs)
	}
	vol := filepath.VolumeName(abs)
	if len(vol) != 2 { // UNC paths have no Recycle Bin
		return "", fmt.Errorf("trash %s: network paths have no Recycle Bin", abs)
	}
	root, _ := windows.UTF16PtrFromString(vol + `\`)
	if t, _, _ := procGetDriveTyp.Call(uintptr(unsafe.Pointer(root))); t != driveFixed {
		return "", fmt.Errorf("trash %s: drive %s has no Recycle Bin (removable or remote)", abs, vol)
	}
	from, err := windows.UTF16FromString(abs)
	if err != nil {
		return "", err
	}
	from = append(from, 0) // pFrom is a double-NUL-terminated list
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	r, _, _ := procSHFileOpW.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return "", fmt.Errorf("trash %s: SHFileOperation error 0x%x", abs, r)
	}
	if op.fAnyOperationsAborted != 0 {
		return "", fmt.Errorf("trash %s: aborted", abs)
	}
	return "", nil
}

// TrashRestored is called after a file was put back from the trash; the Recycle Bin needs nothing.
func TrashRestored(string) {}
