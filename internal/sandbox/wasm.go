package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"noskill/internal/perms"
	"noskill/internal/platform"
)

// WASM tools (FR-S5, SECURITY.md "WASM tools"). A WASI preview-1 module runs under wazero, which is
// pure Go: the sandbox is the same on every OS and needs no host interpreter. The module sees only
// the call's workspace: /in (read-only copies of the files the tool was given), /out and /tmp. It
// has no network and no other files; the OS sandbox isn't involved.
//
// The protocol is the script protocol: arguments as JSON on stdin, the result on stdout, logs on
// stderr, a non-zero exit is an error.

// WasmCall is one tool call on a module.
type WasmCall struct {
	RunID, CallID string
	Name          string // argv[0]
	Module        []byte
	Stdin         []byte
	Env           map[string]string
	Inputs        map[string]string // name in /in → real path (checked for read access)
	Perms         perms.Set
	Timeout       time.Duration
}

var (
	wasmCacheOnce sync.Once
	wasmCache     wazero.CompilationCache
)

// RunWasm builds the workspace, copies the inputs in and runs the module.
func (r *Runner) RunWasm(ctx context.Context, c WasmCall) (Output, error) {
	var out Output
	if len(c.Module) == 0 {
		return out, errors.New("wasm: empty module")
	}
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

	timeout := r.Timeout
	if c.Timeout > 0 && (timeout == 0 || c.Timeout < timeout) {
		timeout = c.Timeout
	}
	max := r.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	mem := r.MemBytes
	if mem <= 0 {
		mem = 512 << 20
	}
	pages := uint32(min(mem/65536, 65536))

	wasmCacheOnce.Do(func() { wasmCache = wazero.NewCompilationCache() })
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).WithMemoryLimitPages(pages).WithCompilationCache(wasmCache))
	defer rt.Close(context.WithoutCancel(ctx))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return out, err
	}
	// Compiling isn't the tool's time: the limit covers only the run.
	compiled, err := rt.CompileModule(ctx, c.Module)
	if err != nil {
		return out, fmt.Errorf("wasm: not a valid module: %w", err)
	}
	rctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	stdout := &capped{max: max}
	stderr := &capped{max: 64 << 10}
	fsc := wazero.NewFSConfig().WithReadOnlyDirMount(ws.In, "/in").WithDirMount(ws.Out, "/out").WithDirMount(ws.Tmp, "/tmp")
	cfg := wazero.NewModuleConfig().WithName("").WithArgs(c.Name).WithStdin(bytes.NewReader(c.Stdin)).
		WithStdout(stdout).WithStderr(stderr).WithFSConfig(fsc)
	// Relative paths resolve from "/", so a path argument "in/<name>" reaches the /in mount (Go's
	// wasip1 reads PWD for this).
	cfg = cfg.WithEnv("PWD", "/")
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cfg = cfg.WithEnv(k, c.Env[k])
	}

	start := time.Now()
	_, err = rt.InstantiateModule(rctx, compiled, cfg)
	res := platform.SandboxResult{Wall: time.Since(start)}
	var ee *sys.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.Exit = int(ee.ExitCode())
		if ee.ExitCode() == sys.ExitCodeDeadlineExceeded {
			res.Exit, res.Killed = -1, "timeout"
		}
		if ee.ExitCode() == sys.ExitCodeContextCanceled {
			res.Exit, res.Killed = -1, "cancelled"
		}
		err = nil // a non-zero exit is the tool's own error, reported from stderr
	case rctx.Err() != nil:
		res.Exit, res.Killed = -1, "timeout"
		if ctx.Err() != nil {
			res.Killed = "cancelled"
		}
		err = nil
	default:
		// A trap (out of memory, unreachable, bad import): the module crashed.
		res.Exit = 1
		stderr.Write([]byte("wasm trap: " + err.Error()))
		err = nil
	}
	out.Stdout, out.Stderr, out.Truncated, out.Result, out.Sandboxed = stdout.b.Bytes(), stderr.b.Bytes(), stdout.cut, res, true
	return out, err
}
