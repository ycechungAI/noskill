package platform

import "os"

// ReFS block cloning (FSCTL_DUPLICATE_EXTENTS_TO_FILE) isn't wired up yet, so Windows always copies.
func clonePath(src, dst string) error { return ErrUnsupported }

func cloneFD(out, in *os.File) error { return ErrUnsupported }
