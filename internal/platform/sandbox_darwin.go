package platform

// macOS script sandbox, as proven in S9 (ADR-0008): sandbox-exec with a generated deny-default
// SBPL profile, rlimits applied by a re-exec of noskill inside the new process group, and a
// memory/process watchdog (macOS refuses memory rlimits).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// NewSandbox returns this OS's script sandbox.
func NewSandbox() Sandbox { return darwinSandbox{} }

type darwinSandbox struct{}

func (darwinSandbox) Strength() SandboxStrength {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return SandboxStrength{Mechanism: "none", Files: None, Network: None, Limits: Advisory,
			Note: "sandbox-exec is missing: scripts refuse to run (ADR-0008 fail rule)"}
	}
	return SandboxStrength{Mechanism: "sandbox-exec", Files: Enforced, Network: Enforced, Limits: Enforced,
		Note: "memory limit is a 50 ms watchdog (soft); deprecated by Apple but working on this macOS"}
}

func (s darwinSandbox) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	if len(spec.Argv) == 0 {
		return SandboxResult{}, errors.New("sandbox: empty argv")
	}
	prof, err := Profile(spec)
	if err != nil {
		return SandboxResult{}, err
	}
	pf := filepath.Join(spec.Dir, "profile.sb")
	if err := os.WriteFile(pf, []byte(prof), 0o600); err != nil {
		return SandboxResult{}, err
	}
	self, err := selfExe()
	if err != nil {
		return SandboxResult{}, err
	}
	args := append([]string{SandboxChildArg, "/usr/bin/sandbox-exec", "-f", pf}, spec.Argv...)
	cmd := exec.Command(self, args...)
	spec.Env = append(append([]string{}, spec.Env...), childEnv(spec)...)
	return runWatched(ctx, cmd, spec)
}

func childEnv(spec SandboxSpec) []string {
	cpu := 0
	if spec.Timeout > 0 {
		cpu = int(spec.Timeout.Seconds()) + 1
	}
	nproc := spec.MaxProcs
	if !spec.Subprocess {
		nproc = 0 // fork is denied by the profile; no rlimit needed
	}
	return []string{
		childEnvPrefix + "NPROC=" + strconv.Itoa(nproc),
		childEnvPrefix + "FSIZE=" + strconv.FormatInt(spec.MaxFileBytes, 10),
		childEnvPrefix + "CPU=" + strconv.Itoa(cpu),
	}
}

// MaybeRunSandboxChild handles the re-exec: apply rlimits, drop the NOSKILL_SBX_* variables, exec
// the wrapped command. It returns only when this process isn't a sandbox child.
func MaybeRunSandboxChild() {
	if len(os.Args) < 3 || os.Args[1] != SandboxChildArg {
		return
	}
	setLimits()
	argv := os.Args[2:]
	err := syscall.Exec(argv[0], argv, stripChildEnv(os.Environ()))
	fmt.Fprintln(os.Stderr, "noskill sandbox: exec:", err)
	os.Exit(127)
}

const rlimitNPROC = 7 // RLIMIT_NPROC on darwin; not exported by package syscall

// setLimits: RLIMIT_NPROC is per user, so the cap is the user's current process count + N.
func setLimits() {
	if n, _ := strconv.Atoi(os.Getenv(childEnvPrefix + "NPROC")); n > 0 {
		out, _ := exec.Command("/bin/ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=").Output()
		lim := uint64(len(strings.Fields(string(out))) + n)
		syscall.Setrlimit(rlimitNPROC, &syscall.Rlimit{Cur: lim, Max: lim})
	}
	if v, _ := strconv.ParseUint(os.Getenv(childEnvPrefix+"FSIZE"), 10, 64); v > 0 {
		syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: v, Max: v})
	}
	if v, _ := strconv.ParseUint(os.Getenv(childEnvPrefix+"CPU"), 10, 64); v > 0 {
		syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: v, Max: v})
	}
	syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
}

// sysReadPaths are what dyld and libSystem need: libraries, the shared cache (in the Cryptex on
// macOS 13+), time zones, TLS roots. Found by bisection in S9; macOS 27 doesn't log denials for
// unsigned CLI processes.
var sysReadPaths = []string{"/usr/lib", "/usr/share", "/System", "/Library/Apple", "/Library/Preferences/Logging",
	"/private/var/db/dyld", "/private/var/db/timezone", "/private/etc/ssl", "/private/var/select", "/private/preboot/Cryptexes"}

var sysReadLiterals = []string{"/", "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper", "/dev/tty",
	"/private/etc/localtime", "/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/passwd", "/private/etc/group"}

// Profile generates the deny-default SBPL profile for spec (ADR-0008 "Profile"). Paths are resolved
// to their real form first, because the sandbox matches real paths (/private/var, not /var).
func Profile(spec SandboxSpec) (string, error) {
	work, err := realPath(spec.Work)
	if err != nil {
		return "", fmt.Errorf("sandbox work folder: %w", err)
	}
	var runtime []string
	for _, p := range append(append([]string{}, spec.ReadOnly...), spec.Exec...) {
		r, err := realPath(p)
		if err != nil {
			return "", fmt.Errorf("sandbox path %s: %w", p, err)
		}
		runtime = append(runtime, r)
	}
	for _, p := range append([]string{work}, runtime...) {
		if strings.ContainsAny(p, "\x00\n\r") {
			return "", fmt.Errorf("sandbox path has control characters: %q", p)
		}
	}
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("(version 1)")
	w("(deny default)")
	w("(allow process-exec")
	for _, p := range runtime {
		w("  (subpath %s)", sbplString(p))
	}
	w(")")
	if spec.Subprocess {
		w("(allow process-fork)")
	}
	w("(allow signal (target same-sandbox))")
	w("(allow process-info* (target same-sandbox))")
	// Path lookups need metadata on ancestors (stat of /Users, …): names and sizes, not contents.
	w("(allow file-read-metadata)")
	w("(allow file-read*")
	for _, p := range sysReadPaths {
		w("  (subpath %s)", sbplString(p))
	}
	for _, p := range sysReadLiterals {
		w("  (literal %s)", sbplString(p))
	}
	for _, p := range runtime {
		w("  (subpath %s)", sbplString(p))
	}
	w("  (subpath %s))", sbplString(work))
	w("(allow file-map-executable")
	for _, p := range append(append([]string{}, sysReadPaths...), runtime...) {
		w("  (subpath %s)", sbplString(p))
	}
	w(")")
	w("(allow file-write* (subpath %s) (literal \"/dev/null\") (literal \"/dev/dtracehelper\") (literal \"/dev/tty\"))", sbplString(work))
	w("(allow file-ioctl (literal \"/dev/dtracehelper\") (literal \"/dev/tty\"))")
	w("(allow sysctl-read)")
	w("(allow mach-lookup (global-name \"com.apple.system.opendirectoryd.libinfo\") (global-name \"com.apple.system.notification_center\") (global-name \"com.apple.logd\"))")
	w("(allow ipc-posix-shm-read* (ipc-posix-name \"apple.shm.notification_center\"))")
	if spec.ProxyPort > 0 {
		// Only the egress proxy: no DNS (the proxy resolves names), no other localhost ports.
		w("(allow network-outbound (remote ip %s))", sbplString("localhost:"+strconv.Itoa(spec.ProxyPort)))
	}
	return b.String(), nil
}

// sbplString quotes s as an SBPL (TinyScheme) string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func realPath(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(a)
}
