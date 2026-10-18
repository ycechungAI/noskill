// S9 copy-in cost: copy a folder of inputs into a run's work/in (SECURITY.md "Copy-in workspace").
//
//	copyin -mode copy|clone SRC DST
//
// copy = read and write every byte (works on every filesystem); clone = copy-on-write clone where the
// filesystem supports it (APFS clonefile via `cp -c`, Btrfs/XFS reflink via `cp --reflink`, ReFS block clone).
// Inputs end up read-only so a script can't change them in place.
package main

import (
	"flag"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	mode := flag.String("mode", "copy", "copy | clone")
	flag.Parse()
	src, dst := flag.Arg(0), flag.Arg(1)
	if *mode == "clone" {
		args := []string{"-c", "-R", src, dst}
		if runtime.GOOS == "linux" {
			args = []string{"--reflink=auto", "-R", src, dst}
		}
		if err := exec.Command("cp", args...).Run(); err != nil {
			log.Fatal(err)
		}
		readOnly(dst)
		return
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(t, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(t, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		log.Fatal(err)
	}
}

func readOnly(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chmod(p, 0o444)
		}
		return nil
	})
}
