package platform

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CloneOrCopy copies the file src to dst (which must not exist), as a copy-on-write clone where
// the filesystem supports it (APFS, Btrfs/XFS reflink) and a plain copy otherwise. Clones take
// 13–32 ms for 100 MB, copies 84–92 ms (S9).
func CloneOrCopy(src, dst string) (cloned bool, err error) {
	if clonePath(src, dst) == nil {
		return true, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return false, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return false, err
	}
	if cloneFD(out, in) == nil {
		return true, out.Close()
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, err
	}
	return false, out.Close()
}

// ScriptEnv is the base environment for a sandboxed script: just enough for interpreters to start,
// with home and temp inside the workspace. Nothing from noskill's own environment leaks in (no
// tokens, no proxy settings, no user paths). pathDirs go first on PATH (managed runtimes).
func ScriptEnv(home, tmp string, pathDirs []string) []string {
	sep := string(os.PathListSeparator)
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		sys := []string{filepath.Join(root, "System32"), root}
		return []string{
			"PATH=" + strings.Join(append(append([]string{}, pathDirs...), sys...), sep),
			"PATHEXT=.COM;.EXE;.BAT;.CMD",
			"SystemRoot=" + root,
			"SystemDrive=" + filepath.VolumeName(root),
			"USERPROFILE=" + home,
			"HOME=" + home,
			"APPDATA=" + filepath.Join(home, "AppData", "Roaming"),
			"LOCALAPPDATA=" + filepath.Join(home, "AppData", "Local"),
			"TEMP=" + tmp,
			"TMP=" + tmp,
			"PYTHONUTF8=1",
		}
	}
	return []string{
		"PATH=" + strings.Join(append(append([]string{}, pathDirs...), "/usr/bin", "/bin"), sep),
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"LANG=C.UTF-8",
		"PYTHONUTF8=1",
	}
}

// CatArgv prints one file to stdout with the OS's own tool: the sandbox self-test's probe.
func CatArgv(p string) []string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return []string{filepath.Join(root, "System32", "cmd.exe"), "/d", "/c", "type", p}
	}
	return []string{"/bin/cat", p}
}
