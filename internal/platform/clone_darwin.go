package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

func clonePath(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }

func cloneFD(out, in *os.File) error { return ErrUnsupported }
