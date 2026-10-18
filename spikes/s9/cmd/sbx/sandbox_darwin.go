package main

// macOS: sandbox-exec with a generated SBPL profile (SECURITY.md "Per-OS sandbox").
// Deny by default; reads only of system libraries, the runtime paths and work/; writes only to work/;
// network only to the egress proxy's loopback port (no DNS: the proxy resolves names).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func q(s string) string { return strconv.Quote(s) }

func profile(o Opts) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("(version 1)")
	w("(deny default)")
	// Exec: the runtimes and anything the skill declared, nothing else.
	w("(allow process-exec")
	for _, p := range append(append([]string{}, o.RO...), o.Exec...) {
		w("  (subpath %s)", q(p))
	}
	w(")")
	if o.Fork {
		w("(allow process-fork)")
	}
	w("(allow signal (target same-sandbox))")
	w("(allow process-info* (target same-sandbox))")
	// Path lookups need metadata on ancestors (stat of /Users, etc.). This leaks names and sizes, not contents.
	w("(allow file-read-metadata)")
	w("(allow file-read*")
	sys := []string{"/usr/lib", "/usr/share", "/System", "/Library/Apple", "/Library/Preferences/Logging",
		"/private/var/db/dyld", "/private/var/db/timezone", "/private/etc/ssl", "/private/var/select", "/private/preboot/Cryptexes"}
	for _, p := range sys {
		w("  (subpath %s)", q(p))
	}
	for _, p := range []string{"/", "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper", "/dev/tty",
		"/private/etc/localtime", "/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/passwd", "/private/etc/group"} {
		w("  (literal %s)", q(p))
	}
	for _, p := range append(append([]string{}, o.RO...), o.Exec...) {
		w("  (subpath %s)", q(p))
	}
	w("  (subpath %s))", q(o.Work))
	// dyld maps the shared cache (in the Cryptex on macOS 13+) and runtime libraries as executable.
	w("(allow file-map-executable")
	for _, p := range append(append(append([]string{}, sys...), o.RO...), o.Exec...) {
		w("  (subpath %s)", q(p))
	}
	w(")")
	w("(allow file-write* (subpath %s) (literal \"/dev/null\") (literal \"/dev/dtracehelper\") (literal \"/dev/tty\"))", q(o.Work))
	w("(allow file-ioctl (literal \"/dev/dtracehelper\") (literal \"/dev/tty\"))")
	w("(allow sysctl-read)")
	w("(allow mach-lookup (global-name \"com.apple.system.opendirectoryd.libinfo\") (global-name \"com.apple.system.notification_center\") (global-name \"com.apple.logd\"))")
	w("(allow ipc-posix-shm-read* (ipc-posix-name \"apple.shm.notification_center\"))")
	if o.Proxy != "" {
		_, port, _ := strings.Cut(o.Proxy, ":")
		w("(allow network-outbound (remote ip %s))", q("localhost:"+port))
	}
	return b.String()
}

// wrap writes the profile next to work/ (outside the script's reach) and wraps argv in sandbox-exec.
func wrap(o Opts, argv, env []string) ([]string, []string) {
	pf := filepath.Join(o.Run, "profile.sb")
	os.WriteFile(pf, []byte(profile(o)), 0o644)
	return append([]string{"/usr/bin/sandbox-exec", "-f", pf}, argv...), env
}

const rlimitNPROC = 7 // darwin

// setLimits: macOS has no cgroups and refuses memory rlimits below the current virtual size (~466 GB on
// arm64), so memory is left to the watchdog. RLIMIT_NPROC is per user, so the cap is "current user
// processes + N".
func setLimits() {
	n, _ := strconv.Atoi(os.Getenv("SBX_NPROC"))
	if n > 0 {
		out, _ := exec.Command("ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=").Output()
		cur := len(strings.Fields(string(out)))
		lim := uint64(cur + n)
		syscall.Setrlimit(rlimitNPROC, &syscall.Rlimit{Cur: lim, Max: lim})
	}
	if v, _ := strconv.ParseUint(os.Getenv("SBX_FSIZE"), 10, 64); v > 0 {
		syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: v, Max: v})
	}
	if v, _ := strconv.ParseUint(os.Getenv("SBX_CPU"), 10, 64); v > 0 {
		syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: v, Max: v})
	}
	syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
}

// groupUsage returns total RSS (bytes) and process count for a process group, via ps.
// The real daemon uses proc_pidinfo (macOS) / cgroup memory.current (Linux) / Job object (Windows).
func groupUsage(pgid int) (int64, int) {
	out, err := exec.Command("ps", "-o", "rss=", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		return 0, 0
	}
	var total int64
	n := 0
	for _, f := range strings.Fields(string(out)) {
		if v, err := strconv.ParseInt(f, 10, 64); err == nil {
			total += v << 10
			n++
		}
	}
	return total, n
}

func killTree(pgid int) { syscall.Kill(-pgid, syscall.SIGKILL) }

func beforeStart() {}

func outer(o Opts, mode string, argv []string) []string { return argv }

const innerArg = "__sbx_inner"

func inner() { os.Exit(2) } // Linux only
