package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"noskill/internal/perms"
	"noskill/internal/platform"
)

// Runner runs one script tool call (SECURITY.md "Copy-in workspace", FR-S4).
type Runner struct {
	Sandbox     platform.Sandbox
	Mode        string // enforce | warn ([sandbox] mode)
	Egress      *Egress
	RunsDir     string
	RuntimeDirs []string // managed runtimes (uv, node): readable and executable
	PathDirs    []string // put first on PATH

	// Defaults from [sandbox]; a Call can only lower them.
	Timeout      time.Duration
	MemBytes     int64
	MaxProcs     int
	MaxFileBytes int64
	MaxOutput    int // bytes of stdout kept; default 1 MiB
}

type Call struct {
	RunID, CallID string
	Argv          []string // argv[0] absolute: the runtime noskill picked for the tool, never model input
	Exec          []string // extra executables the skill declared
	ReadOnly      []string // folders this call's interpreter and skill code are read from (added to the runner's)
	Stdin         []byte
	Env           map[string]string // this call's secrets and settings
	Inputs        map[string]string // name in work/in → real path (checked for read access)
	Perms         perms.Set         // the owning skill's permissions, narrowed by the caller
	Ports         []int             // CONNECT ports the skill declared; default 443
	Timeout       time.Duration
}

type Output struct {
	Stdout, Stderr []byte
	Truncated      bool
	Result         platform.SandboxResult
	Workspace      *Workspace
	Sandboxed      bool // false only in warn mode on an OS without a sandbox
}

// Run builds the workspace, copies the inputs in, opens the egress proxy for the run if the skill
// has network permission, and runs the command in the OS sandbox. The proxy credential is revoked
// when the command ends.
func (r *Runner) Run(ctx context.Context, c Call) (Output, error) {
	var out Output
	if len(c.Argv) == 0 {
		return out, errors.New("sandbox: empty argv")
	}
	sb, sandboxed, err := r.pick()
	if err != nil {
		return out, err
	}
	out.Sandboxed = sandboxed
	ws, err := NewWorkspace(r.RunsDir, c.RunID, c.CallID)
	if err != nil {
		return out, err
	}
	out.Workspace = ws
	names := make([]string, 0, len(c.Inputs))
	for n := range c.Inputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src, err := c.Perms.CheckRead(c.Inputs[n])
		if err != nil {
			return out, err
		}
		if _, err := ws.AddInput(src, n); err != nil {
			return out, fmt.Errorf("input %s: %w", n, err)
		}
	}

	env := platform.ScriptEnv(ws.Home, ws.Tmp, r.PathDirs)
	proxyPort := 0
	if len(c.Perms.Network) > 0 {
		if r.Egress == nil {
			return out, errors.New("skill needs network but the egress proxy isn't running")
		}
		tok := r.Egress.Register(c.RunID, c.Perms, c.Ports)
		defer r.Egress.Revoke(tok)
		u := r.Egress.URL(tok)
		proxyPort = r.Egress.Port()
		env = append(env, "HTTPS_PROXY="+u, "HTTP_PROXY="+u, "https_proxy="+u, "http_proxy="+u, "NO_PROXY=", "no_proxy=",
			"NODE_USE_ENV_PROXY=1") // Node's fetch ignores the proxy variables without it (S9)
	}
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+c.Env[k])
	}

	timeout := r.Timeout
	if c.Timeout > 0 && (timeout == 0 || c.Timeout < timeout) {
		timeout = c.Timeout
	}
	max := r.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	stdout := &capped{max: max}
	stderr := &capped{max: 64 << 10}
	res, err := sb.Run(ctx, platform.SandboxSpec{
		Dir: ws.Dir, Work: ws.Work, Argv: c.Argv, Env: env,
		Stdin: bytes.NewReader(c.Stdin), Stdout: stdout, Stderr: stderr,
		ReadOnly: append(append([]string{}, r.RuntimeDirs...), c.ReadOnly...), Exec: append(append([]string{}, c.Exec...), c.Argv[0]), ProxyPort: proxyPort, Subprocess: c.Perms.Subprocess,
		Timeout: timeout, MemBytes: r.MemBytes, MaxProcs: r.MaxProcs, MaxFileBytes: r.MaxFileBytes,
	})
	out.Stdout, out.Stderr, out.Truncated, out.Result = stdout.b.Bytes(), stderr.b.Bytes(), stdout.cut, res
	return out, err
}

// pick applies [sandbox] mode: without an OS sandbox, enforce refuses and warn runs unconfined.
func (r *Runner) pick() (platform.Sandbox, bool, error) {
	sb := r.Sandbox
	if sb == nil {
		sb = platform.NewSandbox()
	}
	if sb.Strength().Files != platform.None {
		return sb, true, nil
	}
	if r.Mode == "warn" {
		return platform.Unsandboxed(), false, nil
	}
	return nil, false, platform.ErrNoSandbox
}

type capped struct {
	b   bytes.Buffer
	max int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room < len(p) {
		c.cut = true
		if room > 0 {
			c.b.Write(p[:room])
		}
		return len(p), nil // keep draining so the script doesn't block on a full pipe
	}
	return c.b.Write(p)
}

// SelfTest is `noskill doctor`'s sandbox check (SECURITY.md "Per-OS sandbox"): a file inside the
// workspace must be readable and one outside must not. macOS shows no denial messages for CLI
// tools, so a broken profile would otherwise only surface as scripts crashing.
func SelfTest(ctx context.Context, r *Runner) error {
	if _, _, err := r.pick(); err != nil {
		return err
	}
	outside := filepath.Join(r.RunsDir, "selftest-outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		return err
	}
	src := filepath.Join(r.RunsDir, "selftest-input.txt")
	if err := os.WriteFile(src, []byte("inside"), 0o600); err != nil {
		return err
	}
	srcDir, _ := perms.Canonical(r.RunsDir)
	p := perms.Set{Files: []perms.Scope{{Access: perms.Read, Root: srcDir}}}
	id := fmt.Sprintf("c%d", time.Now().UnixNano())
	in, err := r.Run(ctx, Call{RunID: "selftest", CallID: id + "a", Argv: platform.CatArgv(filepath.Join("in", "f")),
		Inputs: map[string]string{"f": src}, Perms: p, Timeout: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("sandbox self-test: reading inside the workspace failed: %w", err)
	}
	if in.Result.Exit != 0 || string(in.Stdout) != "inside" {
		return fmt.Errorf("sandbox self-test: reading inside the workspace failed (exit %d): %s", in.Result.Exit, in.Stderr)
	}
	if !in.Sandboxed {
		return nil // warn mode: nothing to deny
	}
	o, err := r.Run(ctx, Call{RunID: "selftest", CallID: id + "b", Argv: platform.CatArgv(outside), Perms: p, Timeout: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("sandbox self-test: %w", err)
	}
	if o.Result.Exit == 0 || bytes.Contains(o.Stdout, []byte("outside")) {
		return errors.New("sandbox self-test: a file outside the workspace was readable; the sandbox profile isn't working")
	}
	return nil
}
