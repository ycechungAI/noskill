package sandbox

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"noskill/internal/perms"
	"noskill/internal/platform"
)

func TestMain(m *testing.M) {
	platform.MaybeRunSandboxChild()
	os.Exit(m.Run())
}

func scope(t *testing.T, a perms.Access, dir string) perms.Scope {
	c, err := perms.Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return perms.Scope{Access: a, Root: c}
}

func TestWorkspaceInputsAndOutputs(t *testing.T) {
	runs, src, dest := t.TempDir(), t.TempDir(), t.TempDir()
	t.Cleanup(func() { // inputs are read-only folders; let TempDir's cleanup remove them
		filepath.WalkDir(runs, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o644)
	os.MkdirAll(filepath.Join(src, "d", "e"), 0o755)
	os.WriteFile(filepath.Join(src, "d", "e", "b.txt"), []byte("B"), 0o644)
	if runtime.GOOS != "windows" {
		os.Symlink("/etc", filepath.Join(src, "d", "escape"))
	}
	ws, err := NewWorkspace(runs, "run1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkspace(runs, "run1", "c1"); err == nil {
		t.Error("workspace reused")
	}
	if _, err := NewWorkspace(runs, "../x", "c1"); err == nil {
		t.Error("bad run id accepted")
	}
	if rel, err := ws.AddInput(filepath.Join(src, "a.txt"), ""); err != nil || rel != filepath.Join("in", "a.txt") {
		t.Fatalf("%q %v", rel, err)
	}
	if _, err := ws.AddInput(filepath.Join(src, "d"), "tree"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.In, "tree", "e", "b.txt")); string(b) != "B" {
		t.Errorf("nested input = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(ws.In, "tree", "escape")); err == nil {
		t.Error("symlink copied into the workspace")
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(filepath.Join(ws.In, "a.txt"), []byte("x"), 0o644); err == nil {
			t.Error("input is writable")
		}
	}

	os.MkdirAll(filepath.Join(ws.Out, "sub"), 0o755)
	os.WriteFile(filepath.Join(ws.Out, "sub", "r.txt"), []byte("R"), 0o644)
	os.WriteFile(filepath.Join(dest, "taken.txt"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(ws.Out, "taken.txt"), []byte("new"), 0o644)
	b := &perms.Broker{RunID: "run1", Perms: perms.Set{Files: []perms.Scope{scope(t, perms.Write, dest)}}}
	applied, err := ws.ApplyOutputs(context.Background(), b, dest)
	if !errors.Is(err, perms.ErrExists) {
		t.Errorf("overwrite: applied %v, err %v", applied, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "sub", "r.txt")); string(got) != "R" {
		t.Errorf("output not applied: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "taken.txt")); string(got) != "keep" {
		t.Errorf("existing file overwritten: %q", got)
	}
	if _, err := ws.ApplyOutputs(context.Background(), b, t.TempDir()); !perms.IsDenied(err) {
		t.Errorf("apply outside scope: %v", err)
	}
}

func TestPublicAddr(t *testing.T) {
	for s, want := range map[string]bool{
		"93.184.216.34": true, "2606:2800:220:1::": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false,
		"::ffff:127.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("%s = %v", s, got)
		}
	}
}

// egressFixture: a TLS server on 127.0.0.1 that the proxy may reach as "good.test" (loopback
// allowed only for that name, only in this test), and "rebind.test" also pointing at 127.0.0.1
// under the default address rule.
func egressFixture(t *testing.T) (*Egress, *httptest.Server, int) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from good.test") }))
	t.Cleanup(srv.Close)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	e, err := StartEgress()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	lo := netip.MustParseAddr("127.0.0.1")
	e.Dialer = PublicDialer{
		Lookup: func(_ context.Context, h string) ([]netip.Addr, error) {
			if h == "good.test" || h == "rebind.test" {
				return []netip.Addr{lo}, nil
			}
			return nil, fmt.Errorf("no such host %s", h)
		},
		AllowAddr: func(a netip.Addr) bool { return true },
	}
	return e, srv, port
}

func get(t *testing.T, proxy, target string) (string, error) {
	pu, _ := url.Parse(proxy)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := c.Get(target)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestEgress(t *testing.T) {
	e, _, port := egressFixture(t)
	var events []EgressEvent
	e.Log = func(ev EgressEvent) { events = append(events, ev) }
	allow := perms.Set{Network: []string{"good.test", "rebind.test"}}
	tok := e.Register("run1", allow, []int{port})
	target := fmt.Sprintf("https://good.test:%d/", port)

	if body, err := get(t, e.URL(tok), target); err != nil || body != "hello from good.test" {
		t.Fatalf("allowed host: %q %v", body, err)
	}
	if _, err := get(t, strings.Replace(e.URL(tok), tok, "wrong", 1), target); err == nil {
		t.Error("wrong credential accepted")
	}
	for name, u := range map[string]string{
		"not allowlisted": fmt.Sprintf("https://evil.test:%d/", port),
		"IP literal":      fmt.Sprintf("https://127.0.0.1:%d/", port),
		"other port":      "https://good.test:8443/",
	} {
		if _, err := get(t, e.URL(tok), u); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	// Default address rule: an allowlisted name that resolves to loopback is refused.
	e.Dialer.AllowAddr = nil
	if _, err := get(t, e.URL(tok), fmt.Sprintf("https://rebind.test:%d/", port)); err == nil {
		t.Error("name resolving to 127.0.0.1 allowed")
	}
	e.Revoke(tok)
	e.Dialer.AllowAddr = func(netip.Addr) bool { return true }
	if _, err := get(t, e.URL(tok), target); err == nil {
		t.Error("revoked credential still works")
	}
	if len(events) == 0 || !events[0].Allowed || events[0].Host != "good.test" {
		t.Errorf("events = %+v", events)
	}
}

type noSandbox struct{}

func (noSandbox) Strength() platform.SandboxStrength {
	return platform.SandboxStrength{Mechanism: "none", Files: platform.None}
}
func (noSandbox) Run(context.Context, platform.SandboxSpec) (platform.SandboxResult, error) {
	return platform.SandboxResult{}, platform.ErrNoSandbox
}

func TestModeWithoutSandbox(t *testing.T) {
	r := &Runner{Sandbox: noSandbox{}, RunsDir: t.TempDir(), Mode: "enforce"}
	if _, err := r.Run(context.Background(), Call{RunID: "r", CallID: "c", Argv: platform.CatArgv("x")}); !errors.Is(err, platform.ErrNoSandbox) {
		t.Fatalf("enforce without sandbox: %v", err)
	}
	r.Mode = "warn"
	if err := SelfTest(context.Background(), r); err != nil {
		t.Fatalf("warn mode self-test: %v", err)
	}
}

func darwinRunner(t *testing.T) *Runner {
	if runtime.GOOS != "darwin" {
		t.Skip("OS sandbox only on macOS so far (S9 Linux/Windows open)")
	}
	return &Runner{RunsDir: t.TempDir(), Mode: "enforce", Timeout: 20 * time.Second, MemBytes: 512 << 20, MaxFileBytes: 64 << 20}
}

func TestSelfTestDarwin(t *testing.T) {
	r := darwinRunner(t)
	if err := SelfTest(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestCurlThroughProxyDarwin(t *testing.T) {
	r := darwinRunner(t)
	e, _, port := egressFixture(t)
	r.Egress = e
	p := perms.Set{Network: []string{"good.test"}}
	run := func(id string, args ...string) Output {
		out, err := r.Run(context.Background(), Call{RunID: "net", CallID: id, Perms: p, Ports: []int{port},
			Argv: append([]string{"/usr/bin/curl", "-sk", "--max-time", "5"}, args...)})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return out
	}
	if o := run("ok", fmt.Sprintf("https://good.test:%d/", port)); string(o.Stdout) != "hello from good.test" {
		t.Errorf("through the proxy: exit %d, %q, %s", o.Result.Exit, o.Stdout, o.Stderr)
	}
	if o := run("evil", fmt.Sprintf("https://evil.test:%d/", port)); o.Result.Exit == 0 {
		t.Errorf("non-allowlisted host reached: %q", o.Stdout)
	}
	if o := run("bypass", "--noproxy", "*", fmt.Sprintf("https://127.0.0.1:%d/", port)); o.Result.Exit == 0 {
		t.Errorf("direct connection bypassed the proxy: %q", o.Stdout)
	}
}
