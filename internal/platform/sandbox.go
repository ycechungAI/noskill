package platform

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Sandbox runs one script command confined to its workspace (SECURITY.md "Per-OS sandbox",
// FR-S6). The caller builds the workspace and the clean environment; the sandbox applies the OS
// mechanism and the limits, and kills the process tree when a limit or the context ends it.
type Sandbox interface {
	Strength() SandboxStrength
	Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error)
}

// Enforcement says how a limit holds on this OS: shown by `noskill doctor`.
type Enforcement string

const (
	Enforced Enforcement = "enforced"
	Advisory Enforcement = "advisory" // logged, not blocked
	None     Enforcement = "none"
)

type SandboxStrength struct {
	Mechanism string // "sandbox-exec", "none", …
	Files     Enforcement
	Network   Enforcement
	Limits    Enforcement
	Note      string
}

// ErrNoSandbox means this OS has no script sandbox yet. Scripts refuse to run unless
// [sandbox] mode = "warn" (SECURITY.md fallback row).
var ErrNoSandbox = errors.New("no script sandbox on this OS yet (S9 not run here); set [sandbox] mode = \"warn\" to run scripts unsandboxed")

type SandboxSpec struct {
	Dir    string   // the run folder; the profile goes here, outside the script's reach
	Work   string   // the only writable folder, and the cwd
	Argv   []string // argv[0] is an absolute path
	Env    []string // the complete environment; nothing is inherited
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	ReadOnly   []string // runtime folders: readable, executable, mappable
	Exec       []string // extra executables the skill declared
	ProxyPort  int      // egress proxy port on 127.0.0.1; 0 = no network at all
	Subprocess bool     // allow fork (the skill declared `subprocess: true`)

	Timeout      time.Duration
	MemBytes     int64 // watchdog limit for the whole process group
	MaxProcs     int   // RLIMIT_NPROC headroom over the user's current count
	MaxFileBytes int64 // RLIMIT_FSIZE
}

type SandboxResult struct {
	Exit      int           `json:"exit"`
	Signal    string        `json:"signal,omitempty"`
	Killed    string        `json:"killed,omitempty"` // memory | procs | timeout | cancelled
	Wall      time.Duration `json:"wall"`
	PeakRSS   int64         `json:"peak_rss"`
	PeakProcs int           `json:"peak_procs"`
}

// watchInterval is how often the watchdog samples memory and process count. ADR-0008 measured an
// overshoot of 512 → 869 MB at 50 ms; keep headroom in the memory budget.
const watchInterval = 50 * time.Millisecond

// Unsandboxed runs scripts with limits and the watchdog but no OS confinement:
// [sandbox] mode = "warn" only.
func Unsandboxed() Sandbox { return unsandboxed{} }

type unsandboxed struct{}

func (unsandboxed) Strength() SandboxStrength {
	return SandboxStrength{Mechanism: "none", Files: None, Network: Advisory, Limits: Advisory,
		Note: "[sandbox] mode = \"warn\": scripts can read and write anything you can"}
}

func (unsandboxed) Run(ctx context.Context, spec SandboxSpec) (SandboxResult, error) {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	return runWatched(ctx, cmd, spec)
}

// runWatched starts cmd in its own process group and enforces timeout, memory and process count.
func runWatched(ctx context.Context, cmd *exec.Cmd, spec SandboxSpec) (SandboxResult, error) {
	if len(spec.Argv) == 0 {
		return SandboxResult{}, errors.New("sandbox: empty argv")
	}
	cmd.Env = spec.Env
	cmd.Dir = spec.Work
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	newProcessGroup(cmd)
	var res SandboxResult
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return res, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	var deadline <-chan time.Time
	if spec.Timeout > 0 {
		t := time.NewTimer(spec.Timeout)
		defer t.Stop()
		deadline = t.C
	}
	kill := func(why string) {
		if res.Killed == "" {
			res.Killed = why
		}
		killProcessGroup(cmd)
	}
	var waitErr error
loop:
	for {
		select {
		case waitErr = <-done:
			break loop
		case <-ctx.Done():
			kill("cancelled")
		case <-deadline:
			kill("timeout")
		case <-tick.C:
			rss, n := processGroupUsage(cmd)
			res.PeakRSS = max(res.PeakRSS, rss)
			res.PeakProcs = max(res.PeakProcs, n)
			if spec.MemBytes > 0 && rss > spec.MemBytes {
				kill("memory")
			} else if spec.MaxProcs > 0 && n > 2*spec.MaxProcs { // the rlimit acts first; this catches what it misses
				kill("procs")
			}
		}
	}
	killProcessGroup(cmd) // leftovers that stayed in the group
	res.Wall = time.Since(t0)
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.Exit = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
		}
	} else if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

// SandboxChildArg marks the re-exec of the noskill binary that applies rlimits inside the new
// process group and then execs the real command. main() must call MaybeRunSandboxChild first.
const SandboxChildArg = "__noskill_sandbox_child"

const childEnvPrefix = "NOSKILL_SBX_"

func stripChildEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, childEnvPrefix) {
			out = append(out, e)
		}
	}
	return out
}

func selfExe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return p, nil
}
