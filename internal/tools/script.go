package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"noskill/internal/perms"
	"noskill/internal/sandbox"
	"noskill/internal/skills"
)

// Script tool protocol (SKILL_FORMAT.md "Tool definition"): arguments as JSON on stdin, the result
// on stdout, logs on stderr, non-zero exit = error. Same on every OS.
//
// Two conventions let a script work with files without ever seeing real paths:
//   - A string argument whose schema says `format: path` names a file or folder the script reads.
//     It's checked against the skill's read scopes, copied into work/in/, and the argument the
//     script receives is the copy's path, relative to its working folder ("in/report.pdf").
//   - A script that wants to leave a file somewhere writes it under work/out/ and lists it in its
//     result: {"outputs": [{"from": "out/summary.md", "to": "~/Documents/summary.md"}]}. The
//     broker checks each destination against the skill's write scopes, never overwrites, and logs
//     it for undo.
//   - A script that changes files in a folder it was given lists the changes instead of making
//     them: {"ops": [{"op": "move", "from": "in/docs/a.txt", "to": "in/docs/old/a.txt"}, ...]}.
//     Paths name the copy in in/; the broker applies each op to the real folder (ops.go). With
//     dry_run true in the arguments, nothing is applied and the result says what would be done.
func (e *Executor) runScript(ctx context.Context, sk *skills.Skill, t skills.Tool, set perms.Set, callID string, args json.RawMessage) (string, error) {
	if e.Runner == nil || e.Interp == nil {
		return "", errors.New("script tools aren't set up in this run")
	}
	root, err := e.skillRoot(sk)
	if err != nil {
		return "", err
	}
	script := filepath.Join(root, filepath.FromSlash(t.Entry))
	if err := insideReal(root, script); err != nil {
		return "", fmt.Errorf("tool %s: %w", t.Name, err)
	}
	cmd, err := e.Interp.Resolve(ctx, sk, t, script)
	if err != nil {
		return "", err
	}

	var inputs map[string]string
	args, inputs, err = e.prepareInputs(t, set, args)
	if err != nil {
		return "", err
	}
	env, err := e.scriptEnv(sk, t)
	if err != nil {
		return "", err
	}

	// A script's own stdlib and tool code are read-only; its writes go to work/.
	ro := append(append([]string{}, cmd.ReadOnly...), root)
	out, err := e.Runner.Run(ctx, sandbox.Call{
		RunID: e.RunID, CallID: callID, Argv: cmd.Argv, Exec: cmd.Exec, ReadOnly: ro, Stdin: args, Env: env,
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

// finishScript applies {"outputs": [...]} through the broker and returns the result text.
func (e *Executor) finishScript(ctx context.Context, set perms.Set, out sandbox.Output, t skills.Tool, inputs map[string]string, dryRun bool) (string, error) {
	text := strings.TrimSpace(string(out.Stdout))
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &obj) == nil && obj["ops"] != nil {
		return e.applyOps(ctx, set, obj, t, inputs, dryRun)
	}
	if obj == nil || obj["outputs"] == nil {
		if files, _, err := out.Workspace.Outputs(); err == nil && len(files) > 0 {
			text += fmt.Sprintf("\n[%d file(s) in out/ were not saved: the tool didn't say where they go]", len(files))
		}
		if out.Truncated {
			text += "\n[output cut]"
		}
		return text, nil
	}
	var items []struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(obj["outputs"], &items); err != nil {
		return "", fmt.Errorf("tool %s: \"outputs\" must be a list of {from, to}", t.Name)
	}
	br := &perms.Broker{RunID: e.RunID, Perms: set, Trash: e.Trash, Undo: e.Undo}
	var saved []string
	var problems []string
	for _, it := range items {
		to, err := e.expand(it.To)
		if err == nil {
			err = out.Workspace.ApplyOutput(ctx, br, strings.TrimPrefix(filepath.ToSlash(it.From), "out/"), to)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", it.To, err))
			continue
		}
		saved = append(saved, to)
	}
	delete(obj, "outputs")
	obj["saved"], _ = json.Marshal(saved)
	if len(problems) > 0 {
		obj["not_saved"], _ = json.Marshal(problems)
	}
	b, _ := json.Marshal(obj)
	return string(b), nil
}

func (e *Executor) expand(p string) (string, error) {
	if e.Expand != nil {
		return e.Expand(p)
	}
	return perms.Resolver{}.Expand(p)
}

// prepareInputs finds `format: path` arguments, checks them for read access, and rewrites them to
// the workspace copy's name.
func (e *Executor) prepareInputs(t skills.Tool, set perms.Set, args json.RawMessage) (json.RawMessage, map[string]string, error) {
	var sch struct {
		Properties map[string]struct {
			Type   string `json:"type"`
			Format string `json:"format"`
			Items  struct {
				Format string `json:"format"`
			} `json:"items"`
		} `json:"properties"`
	}
	if len(t.InputSchema) == 0 || json.Unmarshal(t.InputSchema, &sch) != nil {
		return args, nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, nil, err
	}
	inputs := map[string]string{}
	used := map[string]bool{}
	bring := func(arg, raw string) (string, error) {
		abs, err := e.expand(raw)
		if err != nil {
			return "", fmt.Errorf("argument %q: %w", arg, err)
		}
		real, err := set.CheckRead(abs)
		if err != nil {
			return "", err
		}
		name := filepath.Base(real)
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%d-%s", i, filepath.Base(real))
		}
		used[name] = true
		inputs[name] = real
		return "in/" + name, nil
	}
	for arg, p := range sch.Properties {
		switch v := m[arg].(type) {
		case string:
			if p.Format == "path" {
				rel, err := bring(arg, v)
				if err != nil {
					return nil, nil, err
				}
				m[arg] = rel
			}
		case []any:
			if p.Items.Format == "path" {
				for i, x := range v {
					if s, ok := x.(string); ok {
						rel, err := bring(arg, s)
						if err != nil {
							return nil, nil, err
						}
						v[i] = rel
					}
				}
			}
		}
	}
	b, err := json.Marshal(m)
	return b, inputs, err
}

// scriptEnv holds only what the skill declared: its secrets and a few identifying variables.
func (e *Executor) scriptEnv(sk *skills.Skill, t skills.Tool) (map[string]string, error) {
	env := map[string]string{"NOSKILL_SKILL": sk.Name, "NOSKILL_TOOL": t.Name}
	for _, s := range sk.Meta.Permissions.SecretList() {
		v := ""
		if e.Secrets != nil {
			v, _ = e.Secrets.Get(s.Name)
		}
		switch {
		case v != "":
			env[s.Name] = v
		case !s.Optional:
			return nil, fmt.Errorf("secret %s isn't set: run `noskill secrets set %s`", s.Name, s.Name)
		}
	}
	return env, nil
}

// skillRoot returns a folder on disk holding the skill's files. Embedded built-in skills are
// written once to runtimes/skills/<name>-<hash>, so a script tool can be run from them.
func (e *Executor) skillRoot(sk *skills.Skill) (string, error) {
	if sk.Dir != "" {
		return sk.Dir, nil
	}
	if e.Interp == nil || e.Interp.RuntimesDir == "" {
		return "", errors.New("no runtimes folder to unpack the skill into")
	}
	dir := filepath.Join(e.Interp.RuntimesDir, "skills", sk.Name+"-"+shortHash(sk.Hash))
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return dir, nil
	}
	tmp := dir + ".tmp"
	os.RemoveAll(tmp) // our own half-written unpack from an earlier attempt, never a user file
	err := fs.WalkDir(sk.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(sk.FS, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, serr := os.Stat(dir); serr != nil { // lost a race to another process: fine if it exists
			return "", err
		}
	}
	return dir, nil
}

func shortHash(h string) string {
	if len(h) >= 8 {
		return h[:8]
	}
	s := sha256.Sum256([]byte(h))
	return hex.EncodeToString(s[:4])
}

// insideReal checks that target, after resolving symlinks, is still inside root.
func insideReal(root, target string) error {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	t, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	if !perms.Within(t, r) {
		return fmt.Errorf("entry %s resolves outside the skill folder", filepath.Base(target))
	}
	return nil
}
