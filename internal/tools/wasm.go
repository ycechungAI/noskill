package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"noskill/internal/perms"
	"noskill/internal/sandbox"
	"noskill/internal/skills"
)

// runWasm runs a `runtime: wasm` tool (FR-S5): the module is read from the skill's own files and
// runs under wazero with the script protocol (arguments on stdin, result on stdout). The same
// copy-in rules apply as for scripts: `format: path` arguments become /in/<name>, and results the
// tool lists under "outputs" are saved through the broker.
func (e *Executor) runWasm(ctx context.Context, sk *skills.Skill, t skills.Tool, set perms.Set, callID string, args json.RawMessage) (string, error) {
	if e.Runner == nil {
		return "", errors.New("wasm tools aren't set up in this run")
	}
	module, err := fs.ReadFile(sk.FS, t.Entry)
	if err != nil {
		return "", fmt.Errorf("tool %s: %w", t.Name, err)
	}
	args, inputs, err := e.prepareInputs(t, set, args)
	if err != nil {
		return "", err
	}
	env, err := e.scriptEnv(sk, t)
	if err != nil {
		return "", err
	}
	out, err := e.Runner.RunWasm(ctx, sandbox.WasmCall{
		RunID: e.RunID, CallID: callID, Name: t.Name, Module: module, Stdin: args, Env: env,
		Inputs: inputs, Perms: set, Timeout: t.Timeout,
	})
	if err != nil {
		return "", err
	}
	if k := out.Result.Killed; k != "" {
		return "", fmt.Errorf("tool %s was stopped (%s limit)", t.Name, k)
	}
	if out.Result.Exit != 0 {
		return "", fmt.Errorf("tool %s failed (exit %d): %s", t.Name, out.Result.Exit, strings.TrimSpace(tail(string(out.Stderr), 600)))
	}
	return e.finishScript(ctx, set, out, t, inputs, isDryRun(args))
}
