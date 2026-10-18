package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

func clonePath(src, dst string) error { return ErrUnsupported }

// cloneFD makes a reflink (Btrfs, XFS) into the new, empty out.
func cloneFD(out, in *os.File) error { return unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) }
