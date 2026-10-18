package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	MaybeRunSandboxChild() // the sandbox re-execs this test binary
	os.Exit(m.Run())
}

// newRun makes a run folder with work/ inside a temp dir and a secret file outside it.
func newRun(t *testing.T) (spec SandboxSpec, outside string) {
	t.Helper()
	root := t.TempDir()
	run := filepath.Join(root, "run")
	work := filepath.Join(run, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(root, "secret.txt")
	os.WriteFile(outside, []byte("canary"), 0o600)
	os.WriteFile(filepath.Join(work, "in.txt"), []byte("hello"), 0o600)
	return SandboxSpec{
		Dir: run, Work: work,
		Env:     []string{"PATH=/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work + "/"},
		Exec:    []string{"/bin/cat", "/bin/sh", "/bin/bash", "/usr/bin/nc"},
		Timeout: 10 * time.Second, MemBytes: 256 << 20, MaxProcs: 16, MaxFileBytes: 1 << 20,
	}, outside
}

func run(t *testing.T, spec SandboxSpec, argv ...string) (SandboxResult, string) {
	t.Helper()
	var out bytes.Buffer
	spec.Argv, spec.Stdout, spec.Stderr = argv, &out, &out
	res, err := NewSandbox().Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestSandboxReadsWorkOnly(t *testing.T) {
	spec, outside := newRun(t)
	if res, out := run(t, spec, "/bin/cat", filepath.Join(spec.Work, "in.txt")); res.Exit != 0 || out != "hello" {
		t.Fatalf("reading work/: exit %d, out %q", res.Exit, out)
	}
	if res, out := run(t, spec, "/bin/cat", outside); res.Exit == 0 || strings.Contains(out, "canary") {
		t.Fatalf("read outside work/ was allowed: exit %d, out %q", res.Exit, out)
	}
}

func TestSandboxDeniesForkAndOutsideWrites(t *testing.T) {
	spec, outside := newRun(t)
	// Two commands, so the shell must fork for cat (a single command would be exec'd directly).
	script := "/bin/cat " + filepath.Join(spec.Work, "in.txt") + "; echo"
	res, out := run(t, spec, "/bin/sh", "-c", script)
	if res.Exit == 0 && strings.Contains(out, "hello") {
		t.Fatalf("fork was allowed without subprocess: %q", out)
	}
	spec.Subprocess = true
	if res, out := run(t, spec, "/bin/sh", "-c", script); res.Exit != 0 || out != "hello\n" {
		t.Fatalf("fork with subprocess: exit %d, out %q", res.Exit, out)
	}
	if res, _ := run(t, spec, "/bin/sh", "-c", "echo x > "+outside+".new"); res.Exit == 0 {
		t.Fatal("write outside work/ was allowed")
	}
	if _, err := os.Stat(outside + ".new"); err == nil {
		t.Fatal("file outside work/ was created")
	}
}

func TestSandboxDeniesNetworkWithoutProxy(t *testing.T) {
	spec, _ := newRun(t)
	spec.Subprocess = true
	// Ollama's port: reachable outside the sandbox on this machine, never inside it.
	if res, _ := run(t, spec, "/usr/bin/nc", "-z", "-w", "2", "127.0.0.1", "11434"); res.Exit == 0 {
		t.Fatal("localhost connection allowed with no proxy")
	}
}

func TestSandboxTimeoutKillsTree(t *testing.T) {
	spec, _ := newRun(t)
	spec.Subprocess = true
	spec.Timeout = 300 * time.Millisecond
	res, _ := run(t, spec, "/bin/sh", "-c", "/bin/sh -c 'while :; do :; done' & while :; do :; done")
	if res.Killed != "timeout" {
		t.Fatalf("killed = %q, want timeout", res.Killed)
	}
}

func TestProfileQuotesPaths(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, `we"ird\dir`)
	os.MkdirAll(odd, 0o755)
	p, err := Profile(SandboxSpec{Work: odd})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, `we\"ird\\dir`) {
		t.Fatalf("path not escaped:\n%s", p)
	}
	if strings.Contains(p, "process-fork") || strings.Contains(p, "network-outbound") {
		t.Fatal("fork or network allowed by default")
	}
}
