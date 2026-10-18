package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"noskill/internal/tools"
)

// The code-runner's tool (skills/code-runner, FR-S4): short Python programs in the script sandbox,
// with no network, the user's files copied in read-only, and a time limit.

const (
	codeTimeout    = 30 * time.Second
	codeMaxTimeout = 120 * time.Second
	codeMaxStdout  = 8000 // characters kept for the model; the rest is cut from the middle
)

// Code returns the code-runner builtins.
func Code() []tools.Builtin {
	return []tools.Builtin{
		{ID: "run_python", Description: "Run a short Python program in a sandbox and get what it prints. No network. Files you pass are in in/<name>; save files to out/.",
			Schema: json.RawMessage(`{"type":"object","required":["code"],"properties":{` +
				`"code":{"type":"string","minLength":1,"description":"The whole program. Print the results."},` +
				`"files":{"type":"array","maxItems":10,"items":{"type":"string","format":"path"},"description":"Files the program reads, copied to in/<name>"},` +
				`"timeout":{"type":"integer","minimum":1,"maximum":120,"default":30,"description":"Seconds"},` +
				`"save_to":{"type":"string","format":"path","description":"A folder to copy the files in out/ to (only when the user asked for files)"}}}`),
			Run: runPython},
	}
}

var missingModule = regexp.MustCompile(`ModuleNotFoundError: No module named '([A-Za-z0-9_]+)'`)

func runPython(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	if env.Python == nil {
		return "", errors.New("running code isn't set up in this run")
	}
	var a struct {
		Code    string
		Files   []string
		Timeout int
		SaveTo  string `json:"save_to"`
	}
	if err := arg(raw, &a); err != nil {
		return "", err
	}
	timeout := codeTimeout
	if a.Timeout > 0 {
		timeout = min(time.Duration(a.Timeout)*time.Second, codeMaxTimeout)
	}
	inputs := map[string]string{}
	var names []string
	for _, f := range a.Files {
		p, err := env.Expand(f)
		if err != nil {
			return "", fmt.Errorf("%s: %w", f, err)
		}
		name := filepath.Base(p)
		for i := 2; inputs[name] != ""; i++ {
			ext := filepath.Ext(filepath.Base(p))
			name = strings.TrimSuffix(filepath.Base(p), ext) + "-" + strconv.Itoa(i) + ext
		}
		inputs[name] = p
		names = append(names, "in/"+name)
	}
	out, prepared, err := env.Python(ctx, a.Code, inputs, timeout)
	if err != nil {
		return "", err
	}
	stdout, stderr := strings.TrimSpace(string(out.Stdout)), strings.TrimSpace(string(out.Stderr))
	if k := out.Result.Killed; k != "" {
		what := map[string]string{"timeout": fmt.Sprintf("the %s time limit", timeout), "memory": "the memory limit", "procs": "the process limit"}[k]
		if what == "" {
			what = k
		}
		return "", fmt.Errorf("the program was stopped: it hit %s. Make it do less, or fix the loop that doesn't end", what)
	}
	if out.Result.Exit != 0 {
		msg := fmt.Sprintf("the program failed (exit %d):\n%s", out.Result.Exit, clipMiddle(stderr, 2000))
		if m := missingModule.FindStringSubmatch(stderr); m != nil && !prepared {
			msg += fmt.Sprintf("\n%s isn't installed: use the standard library, or ask the user to run `noskill skills prepare code-runner` (installs numpy, pandas, openpyxl and matplotlib)", m[1])
		}
		if stdout != "" {
			msg += "\nIt printed before failing:\n" + clipMiddle(stdout, 2000)
		}
		return "", errors.New(msg)
	}
	res := map[string]any{"stdout": clipMiddle(stdout, codeMaxStdout)}
	if out.Truncated {
		res["stdout_cut"] = true
	}
	if stderr != "" {
		res["stderr"] = clipMiddle(stderr, 1000)
	}
	if len(names) > 0 {
		res["inputs"] = names
	}
	if files, _, err := out.Workspace.Outputs(); err == nil && len(files) > 0 {
		var made []string
		for _, f := range files {
			made = append(made, filepath.Join(out.Workspace.Out, f))
		}
		res["files"] = made
		if a.SaveTo != "" {
			dest, err := env.Expand(a.SaveTo)
			if err == nil {
				var saved []string
				saved, err = out.Workspace.ApplyOutputs(ctx, env.Broker, dest)
				res["saved"] = saved
			}
			if err != nil {
				res["not_saved"] = err.Error()
			}
		}
	}
	if stdout == "" && res["files"] == nil {
		res["note"] = "The program printed nothing. Print the results."
	}
	return jsonOut(res)
}

// clipMiddle keeps the start and the end of long output, where the useful parts usually are.
func clipMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	half := n / 2
	return string(r[:half]) + fmt.Sprintf("\n…[%d characters cut]…\n", len(r)-2*half) + string(r[len(r)-half:])
}
