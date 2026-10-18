package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"noskill/internal/backends"
	"noskill/internal/perms"
	"noskill/internal/platform"
	"noskill/internal/sandbox"
	"noskill/internal/schema"
	"noskill/internal/skills"
)

// Executor runs the tools offered in one run. It implements agent.Tools.
//
// Every call goes through the same steps: find the tool among those offered, check and repair
// the arguments against its schema, ask for approval if the skill lists the tool in `confirm`,
// then run it under the owning skill's permissions (narrowed by the caller's scopes).
type Executor struct {
	RunID    string
	Offered  []skills.Resolved
	Registry Registry
	Svc      Services
	Secrets  Secrets

	// Perms returns the owning skill's compiled permissions.
	Perms func(*skills.Skill) (perms.Set, error)
	// Narrow, if set, intersects them with the calling skill's scopes (SECURITY.md "Scope rule").
	Narrow func(owner perms.Set) perms.Set
	Trash  platform.Trasher
	Undo   perms.UndoLog

	Confirm Confirm // nil = confirm tools are refused
	Runner  *sandbox.Runner
	Interp  *Interpreters
	// Setting reads [skills.<name>] values.
	Setting func(skill, key string) (string, bool)
	// Expand resolves path tokens in tool arguments (~, $DOWNLOADS, volume:…). Default: perms.Resolver{}.
	Expand func(path string) (string, error)

	mu  sync.Mutex
	seq int
}

// Problems lists offered tools that can't run (a builtin that doesn't exist). The caller decides
// whether that's fatal; Defs leaves them out.
func (e *Executor) Problems() []string {
	var out []string
	for _, r := range e.Offered {
		if r.Tool.Runtime == "builtin" {
			if _, ok := e.Registry[r.Tool.Entry]; !ok {
				out = append(out, fmt.Sprintf("skill %s: builtin tool %q isn't implemented", r.Skill.Name, r.Tool.Entry))
			}
		}
	}
	return out
}

func (e *Executor) find(name string) (skills.Resolved, bool) {
	for _, r := range e.Offered {
		if r.Name == name {
			return r, true
		}
	}
	return skills.Resolved{}, false
}

// Defs is what the model sees.
func (e *Executor) Defs() []backends.ToolDef {
	var out []backends.ToolDef
	for _, r := range e.Offered {
		desc, schemaJSON := r.Tool.Description, r.Tool.InputSchema
		if r.Tool.Runtime == "builtin" {
			b, ok := e.Registry[r.Tool.Entry]
			if !ok {
				continue
			}
			if desc == "" {
				desc = b.Description
			}
			if len(schemaJSON) == 0 {
				schemaJSON = b.Schema
			}
		}
		if len(schemaJSON) == 0 {
			schemaJSON = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, backends.ToolDef{Name: r.Name, Description: desc, Parameters: schemaJSON})
	}
	return out
}

func (e *Executor) nextCall() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	return fmt.Sprintf("c%04d", e.seq)
}

func (e *Executor) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	r, ok := e.find(name)
	if !ok {
		return "", fmt.Errorf("tool %q isn't available", name)
	}
	sk, tool := r.Skill, r.Tool
	var b Builtin
	schemaJSON := tool.InputSchema
	if tool.Runtime == "builtin" {
		if b, ok = e.Registry[tool.Entry]; !ok {
			return "", fmt.Errorf("builtin tool %q isn't implemented", tool.Entry)
		}
		if len(schemaJSON) == 0 {
			schemaJSON = b.Schema
		}
	}
	norm, err := schema.Normalize(schemaJSON, args)
	if err != nil {
		return "", fmt.Errorf("bad arguments for %s: %w", name, err)
	}
	if slices.Contains(sk.Meta.Confirm, tool.Name) && !isDryRun(norm) {
		if e.Confirm == nil {
			return "", fmt.Errorf("%s needs the user's approval, which isn't available in this run", name)
		}
		if err := e.Confirm(ctx, ConfirmRequest{RunID: e.RunID, Skill: sk.Name, Tool: tool.Name, Args: norm}); err != nil {
			return "", err
		}
	}
	set, err := e.Perms(sk)
	if err != nil {
		return "", fmt.Errorf("permissions of skill %s: %w", sk.Name, err)
	}
	if e.Narrow != nil {
		set = e.Narrow(set)
	}
	callID := e.nextCall()
	var out string
	switch {
	case tool.Runtime == "builtin":
		env := e.env(sk, set, callID)
		out, err = b.Run(ctx, env, norm)
	case slices.Contains([]string{"python", "node", "bun", "sh", "pwsh"}, tool.Runtime):
		out, err = e.runScript(ctx, sk, tool, set, callID, norm)
	case tool.Runtime == "wasm":
		out, err = e.runWasm(ctx, sk, tool, set, callID, norm)
	default:
		err = fmt.Errorf("runtime %q isn't supported", tool.Runtime)
	}
	if err != nil {
		return "", e.redact(sk, err)
	}
	return capOutput(out, tool.MaxOutput), nil
}

func (e *Executor) env(sk *skills.Skill, set perms.Set, callID string) *Env {
	declared := map[string]bool{}
	for _, s := range sk.Meta.Permissions.SecretList() {
		declared[s.Name] = true
	}
	env := &Env{
		RunID: e.RunID, CallID: callID, Skill: sk, Perms: set, Svc: e.Svc, Expand: e.expand,
		Broker: &perms.Broker{RunID: e.RunID, Perms: set, Trash: e.Trash, Undo: e.Undo},
		Secret: func(n string) string {
			if !declared[n] || e.Secrets == nil { // undeclared secrets don't exist for this skill
				return ""
			}
			v, _ := e.Secrets.Get(n)
			return v
		},
		Setting: func(k string) (string, bool) {
			if e.Setting == nil {
				return "", false
			}
			return e.Setting(sk.Name, k)
		},
	}
	if e.Runner != nil && e.Interp != nil {
		env.Python = func(ctx context.Context, code string, inputs map[string]string, timeout time.Duration) (sandbox.Output, bool, error) {
			cmd, prepared, err := e.Interp.CodePython(ctx)
			if err != nil {
				return sandbox.Output{}, false, err
			}
			noNet := set
			noNet.Network = nil // run_python never gets the network (code-runner spec)
			out, err := e.Runner.Run(ctx, sandbox.Call{
				RunID: e.RunID, CallID: callID, Argv: cmd.Argv, Exec: cmd.Exec, ReadOnly: cmd.ReadOnly, Stdin: []byte(code),
				Env: map[string]string{"MPLBACKEND": "Agg"}, Inputs: inputs, Perms: noNet, Timeout: timeout,
			})
			return out, prepared, err
		}
	}
	return env
}

// redact removes the values of the skill's secrets from an error before the model reads it.
func (e *Executor) redact(sk *skills.Skill, err error) error {
	if e.Secrets == nil {
		return err
	}
	msg := err.Error()
	changed := false
	for _, s := range sk.Meta.Permissions.SecretList() {
		if v, ok := e.Secrets.Get(s.Name); ok && len(v) >= 4 && strings.Contains(msg, v) {
			msg, changed = strings.ReplaceAll(msg, v, "[secret]"), true
		}
	}
	if !changed {
		return err
	}
	return &redacted{msg: msg, err: err}
}

type redacted struct {
	msg string
	err error
}

func (r *redacted) Error() string { return r.msg }
func (r *redacted) Unwrap() error { return r.err }

// capOutput cuts a result to max bytes on a rune boundary and says so.
func capOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[output cut: %d of %d bytes shown]", cut, len(s))
}

// isDryRun: a call that says dry_run:true only reports what it would do, so it needs no approval.
func isDryRun(args json.RawMessage) bool {
	var a struct {
		DryRun bool `json:"dry_run"`
	}
	return json.Unmarshal(args, &a) == nil && a.DryRun
}
