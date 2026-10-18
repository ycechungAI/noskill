//go:build darwin && !cgo

package main

import (
	"fmt"
	"os"
)

// macOS draws the tray with Cocoa, which needs CGO. A build without it (a cross-compile from another
// OS or architecture without a C toolchain) still compiles and says so when run.
func main() {
	fmt.Fprintln(os.Stderr, "noskill-tray: this build has no CGO, so it can't draw the macOS menu bar; build it on a Mac")
	os.Exit(1)
}
