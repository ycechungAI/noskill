package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/perms"
	"noskill/internal/platform"
	"noskill/internal/sandbox"
	"noskill/internal/skills"
	"noskill/internal/tools"
	"noskill/internal/tools/builtin"
	skillfiles "noskill/skills"
)

func TestMain(m *testing.M) {
	platform.MaybeRunSandboxChild()
	os.Exit(m.Run())
}

// codeRunner is an executor for the built-in code-runner skill, with extra read and write scopes
// standing in for --allow.
func codeRunner(t *testing.T, allow ...string) (*tools.Executor, context.Context) {
	t.Helper()
	if platform.NewSandbox().Strength().Files == platform.None {
		t.Skip("no OS sandbox on this platform yet")
	}
	set, _ := skills.LoadAll([]skills.Root{{Source: skills.SourceBuiltin, FS: skillfiles.Builtin()}})
	sk, ok := set.Get("code-runner")
	if !ok {
		t.Fatal("code-runner isn't built in")
	}
	data := t.TempDir()
	res := perms.Resolver{}
	e := &tools.Executor{
		RunID: "01CODETEST", Offered: skills.Tools([]*skills.Skill{sk}), Registry: builtin.Registry(),
		Perms: func(s *skills.Skill) (perms.Set, error) {
			ps, err := tools.SkillPerms(res, s)
			if err != nil {
				return ps, err
			}
			extra, err := res.Compile(allow)
			ps.Files = append(ps.Files, extra...)
			return ps, err
		},
		Trash:  platform.OSTrash{},
		Interp: &tools.Interpreters{RuntimesDir: filepath.Join(data, "runtimes")},
		Runner: &sandbox.Runner{RunsDir: filepath.Join(data, "runs"), Mode: "enforce", Timeout: 20 * time.Second, MemBytes: 512 << 20, MaxFileBytes: 16 << 20},
	}
	os.MkdirAll(e.Runner.RunsDir, 0o755)
	if _, _, err := e.Interp.CodePython(context.Background()); err != nil {
		t.Skipf("no Python: %v", err)
	}
	return e, context.Background()
}

func call(t *testing.T, e *tools.Executor, ctx context.Context, args map[string]any) (map[string]any, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	out, err := e.Call(ctx, "run_python", b)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	return m, nil
}

func TestRunPythonMathAndFiles(t *testing.T) {
	src, dest := t.TempDir(), t.TempDir()
	csv := filepath.Join(src, "sales.csv")
	os.WriteFile(csv, []byte("region,amount\nnorth,10\nsouth,5\nnorth,7\n"), 0o644)
	e, ctx := codeRunner(t, "read:"+src, "write:"+dest)

	m, err := call(t, e, ctx, map[string]any{"code": "print(sum(i*i for i in range(1, 101)))"})
	if err != nil || m["stdout"] != "338350" {
		t.Fatalf("%v %v", m, err)
	}
	code := `import csv, collections
t = collections.Counter()
for r in csv.DictReader(open("in/sales.csv")):
    t[r["region"]] += int(r["amount"])
print(dict(sorted(t.items())))
open("out/totals.txt", "w").write(str(t["north"]))`
	m, err = call(t, e, ctx, map[string]any{"code": code, "files": []string{csv}, "save_to": dest})
	if err != nil || m["stdout"] != "{'north': 17, 'south': 5}" {
		t.Fatalf("%v %v", m, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "totals.txt")); string(b) != "17" {
		t.Errorf("saved file = %q (%v)", b, m)
	}
	// A file outside the allowed folders can't be passed in.
	other := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(other, []byte("x"), 0o644)
	if _, err := call(t, e, ctx, map[string]any{"code": "print(1)", "files": []string{other}}); err == nil {
		t.Error("read a file outside the allowed scopes")
	}
}

func TestRunPythonIsContained(t *testing.T) {
	e, ctx := codeRunner(t)
	home, _ := os.UserHomeDir()
	// Errors come back with the traceback so the model can fix the script.
	_, err := call(t, e, ctx, map[string]any{"code": "print(1/0)"})
	if err == nil || !strings.Contains(err.Error(), "ZeroDivisionError") {
		t.Errorf("error: %v", err)
	}
	// No network, no reading outside the workspace, no endless loops.
	_, err = call(t, e, ctx, map[string]any{"code": "import socket\nsocket.create_connection(('1.1.1.1', 443), timeout=3)\nprint('connected')"})
	if err == nil || strings.Contains(err.Error(), "connected") {
		t.Errorf("network: %v", err)
	}
	_, err = call(t, e, ctx, map[string]any{"code": "import os\nprint(os.listdir(" + strconvQuote(home) + "))"})
	if err == nil {
		t.Error("listed the home folder")
	}
	_, err = call(t, e, ctx, map[string]any{"code": "while True: pass", "timeout": 2})
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Errorf("loop: %v", err)
	}
	// A missing package says how to install the analysis set (unless this Python has pandas).
	_, err = call(t, e, ctx, map[string]any{"code": "import pandas"})
	var unused *tools.ErrNotPrepared
	if err != nil && (errors.As(err, &unused) || !strings.Contains(err.Error(), "skills prepare code-runner")) {
		t.Errorf("missing module: %v", err)
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
