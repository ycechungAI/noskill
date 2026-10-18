package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	wasmOnce sync.Once
	wasmFile []byte
	wasmErr  error
)

// buildWasm compiles the fixture tool with Go's own wasip1 port, once per test run.
func buildWasm(t *testing.T) []byte {
	t.Helper()
	wasmOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			wasmErr = err
			return
		}
		dir, err := os.MkdirTemp("", "noskill-wasm-")
		if err != nil {
			wasmErr = err
			return
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "tool.wasm")
		cmd := exec.Command(goBin, "build", "-o", out, "./testdata/wasmtool")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			wasmErr = &buildError{string(b)}
			return
		}
		wasmFile, wasmErr = os.ReadFile(out)
	})
	if wasmErr != nil {
		t.Skipf("can't build the wasip1 fixture: %v", wasmErr)
	}
	return wasmFile
}

type buildError struct{ s string }

func (e *buildError) Error() string { return e.s }

const wasmToolYAML = "name: sum\ndescription: Add two numbers.\nruntime: wasm\nentry: tool.wasm\ntimeout: 3s\n" +
	"input_schema: {type: object, properties: {a: {type: integer}, b: {type: integer}, file: {type: string, format: path}, mode: {type: string}, save_to: {type: string}}}\n"

func wasmSkill(t *testing.T, fs string) (*Executor, string) {
	t.Helper()
	module := buildWasm(t)
	root := t.TempDir()
	fm := "  tools: [tools/sum.tool.yaml]\n"
	if fs != "" {
		fm = "  permissions:\n    filesystem: " + fs + "\n" + fm
	}
	sk := loadSkill(t, root, "adder", fm, map[string]string{"tools/sum.tool.yaml": wasmToolYAML, "tools/tool.wasm": string(module)})
	return newExec(t, sk, nil), sk.Dir
}

// The wasm runtime needs no OS sandbox, so these run on every platform.
func TestWasmToolRunsWithTheScriptProtocol(t *testing.T) {
	e, _ := wasmSkill(t, "")
	out, err := e.Call(context.Background(), "sum", json.RawMessage(`{"a":19,"b":23}`))
	if err != nil || !strings.Contains(out, `"sum":42`) || !strings.Contains(out, `"env":"sum"`) {
		t.Fatalf("%q %v", out, err)
	}

	// A non-zero exit is an error carrying stderr; a loop is stopped by the time limit.
	_, err = e.Call(context.Background(), "sum", json.RawMessage(`{"mode":"fail"}`))
	if err == nil || !strings.Contains(err.Error(), "something broke") || !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("fail: %v", err)
	}
	start := time.Now()
	_, err = e.Call(context.Background(), "sum", json.RawMessage(`{"mode":"loop"}`))
	if err == nil || !strings.Contains(err.Error(), "timeout") || time.Since(start) > 15*time.Second {
		t.Errorf("loop: %v after %v", err, time.Since(start))
	}
}

func TestWasmToolSeesOnlyItsWorkspace(t *testing.T) {
	e, _ := wasmSkill(t, "")
	out, err := e.Call(context.Background(), "sum", json.RawMessage(`{"mode":"probe"}`))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]bool
	if json.Unmarshal([]byte(out), &r) != nil || !r["passwd_denied"] || !r["users_denied"] || !r["in_readonly"] {
		t.Errorf("the module saw more than its workspace: %s", out)
	}
}

func TestWasmToolFilesInAndOut(t *testing.T) {
	home := t.TempDir()
	docs, saveTo := filepath.Join(home, "docs"), filepath.Join(home, "saved")
	writeFile(t, filepath.Join(docs, "notes.txt"), "hello wasm")
	os.MkdirAll(saveTo, 0o755)
	e, _ := wasmSkill(t, `["read:`+docs+`", "write:`+saveTo+`"]`)
	dest := filepath.Join(saveTo, "copy.txt")
	out, err := e.Call(context.Background(), "sum", json.RawMessage(`{"file":"`+filepath.Join(docs, "notes.txt")+`","save_to":"`+dest+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "hello wasm" || !strings.Contains(out, `"bytes":10`) || !strings.Contains(out, `"saved"`) {
		t.Errorf("file in/out: %q (%s)", got, out)
	}
	// A file outside the read scopes never reaches the workspace.
	secret := filepath.Join(home, "secret.txt")
	writeFile(t, secret, "no")
	if _, err := e.Call(context.Background(), "sum", json.RawMessage(`{"file":"`+secret+`"}`)); err == nil {
		t.Error("read outside the scopes")
	}
}
