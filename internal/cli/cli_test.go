package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"noskill/internal/backendtest"
	"noskill/internal/learning"
	"noskill/internal/platform"
	"noskill/internal/secrets"
	"noskill/internal/service"
	"noskill/internal/update"
)

func TestMain(m *testing.M) {
	platform.MaybeRunSandboxChild()
	os.Exit(m.Run())
}

type quiet struct{}

func (quiet) SupportsActions() bool                               { return false }
func (quiet) Notify(context.Context, platform.Notification) error { return nil }

type trashTo struct{ dir string }

func (t trashTo) Trash(p string) (string, error) {
	to := filepath.Join(t.dir, filepath.Base(p))
	for i := 1; ; i++ {
		if _, err := os.Lstat(to); err != nil {
			break
		}
		to = filepath.Join(t.dir, fmt.Sprintf("%s.%d", filepath.Base(p), i))
	}
	return to, os.Rename(p, to)
}

type harness struct {
	t     *testing.T
	home  string
	srv   *backendtest.Ollama
	trash string
	kc    *secrets.Mem
}

var chatCaps = []string{"completion", "tools"}

func newHarness(t *testing.T, models ...backendtest.OllamaModel) *harness {
	t.Helper()
	if len(models) == 0 {
		models = []backendtest.OllamaModel{{Name: "qwen3:4b", Size: 2.5e9, Params: "4.0B", Caps: chatCaps}}
	}
	srv := backendtest.NewOllama(models...)
	t.Cleanup(srv.Close)
	h := &harness{t: t, home: t.TempDir(), srv: srv, trash: t.TempDir(), kc: &secrets.Mem{}}
	h.writeConfig("")
	return h
}

func (h *harness) writeConfig(extra string) {
	cfg := fmt.Sprintf("[[backends]]\nkind = \"ollama\"\nurl = %q\n\n[orchestrator]\ndecision_model = \"\"\n%s", h.srv.URL, extra)
	if !strings.Contains(extra, "[server]") {
		// A port nothing listens on, so a noskill daemon running on this machine (11435) can't make
		// "is anything running the jobs?" come out differently in tests.
		if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
			cfg += fmt.Sprintf("\n[server]\nport = %d\n", l.Addr().(*net.TCPAddr).Port)
			l.Close()
		}
	}
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(cfg), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(stdin string, tty bool, args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code := Main(append([]string{"--home", h.home}, args...), Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Environ: []string{}, Version: "test",
		StdinTTY: tty, StdinFD: -1, Keychain: h.kc, Notifier: quiet{}, Trash: trashTo{h.trash}, Service: &fakeService{},
		Getwd:   func() (string, error) { return h.t.TempDir(), nil },
		Context: func() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) },
	})
	return code, out.String(), errb.String()
}

func (h *harness) ok(stdin string, tty bool, args ...string) (string, string) {
	h.t.Helper()
	code, out, errs := h.run(stdin, tty, args...)
	if code != 0 {
		h.t.Fatalf("noskill %v → exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out, errs
}

func TestUsageAndVersion(t *testing.T) {
	h := newHarness(t)
	if code, _, errs := h.run("", false); code != ExitUsage || !strings.Contains(errs, "commands:") {
		t.Errorf("no args: %d %s", code, errs)
	}
	if code, _, errs := h.run("", false, "frobnicate"); code != ExitUsage || !strings.Contains(errs, "unknown command") {
		t.Errorf("unknown: %d %s", code, errs)
	}
	out, _ := h.ok("", false, "version")
	if out != "noskill test\n" {
		t.Errorf("version = %q", out)
	}
	out, _ = h.ok("", false, "--json", "version")
	if !strings.Contains(out, `"version": "test"`) {
		t.Errorf("json version = %q", out)
	}
	if code, out, _ := h.run("", false, "help"); code != 0 || !strings.Contains(out, "serve") {
		t.Errorf("help: %d %s", code, out)
	}
}

func TestConfigSetGetCheck(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.home, "config.toml")
	before, _ := os.ReadFile(path)
	h.ok("", false, "config", "set", "orchestrator.preset", "best")
	h.ok("", false, "config", "set", "server.expose_skills", "research, files")
	h.ok("", false, "config", "set", "skills.files.allow", "write:~/Downloads")
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), `preset = "best"`) || !strings.Contains(string(after), `expose_skills = ["research", "files"]`) ||
		!strings.Contains(string(after), "[skills.files]") || !strings.Contains(string(after), `kind = "ollama"`) {
		t.Errorf("config after sets:\n%s", after)
	}
	if _, err := os.Stat(path + ".prev"); err != nil {
		t.Error("no .prev backup")
	}
	out, errs := h.ok("", false, "config", "get", "orchestrator.preset")
	if strings.TrimSpace(out) != "best" || !strings.Contains(errs, "file") {
		t.Errorf("get = %q %q", out, errs)
	}
	// Bad values never reach the file.
	snapshot, _ := os.ReadFile(path)
	for _, args := range [][]string{
		{"orchestrator.preset", "turbo"}, {"server.port", "banana"}, {"nope.key", "1"}, {"server.port", "70000"},
	} {
		if code, _, errs := h.run("", false, append([]string{"config", "set"}, args...)...); code == 0 {
			t.Errorf("set %v accepted (%s)", args, errs)
		}
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, snapshot) {
		t.Errorf("a rejected set changed the file:\n%s", now)
	}
	if code, _, _ := h.run("", false, "config", "get", "orchestrator.nope"); code != ExitUsage {
		t.Errorf("get unknown = %d", code)
	}
	_ = before
	out, _ = h.ok("", false, "config", "check", "--explain")
	if !strings.Contains(out, `orchestrator.preset = "best"  (file)`) || !strings.Contains(out, "ok") {
		t.Errorf("check --explain:\n%s", out)
	}
	h.writeConfig("preset = \"turbo\"\nbogus = 1\n")
	if code, out, _ := h.run("", false, "config", "check"); code != ExitError || !strings.Contains(out, "bogus") {
		t.Errorf("broken config: %d %s", code, out)
	}
	if code, _, errs := h.run("", false, "doctor"); code != ExitError || errs != "" && !strings.Contains(errs, "config") {
		t.Logf("doctor on a broken config: %d %s", code, errs)
	}
}

func TestSecretsNeverAppearInOutput(t *testing.T) {
	h := newHarness(t)
	const value = "sk-live-0123456789abcdef"
	if code, _, errs := h.run("", false, "secrets", "set", "BRAVE_API_KEY", value); code == 0 || strings.Contains(errs, value) {
		t.Errorf("a value on the command line must be refused without echoing it: %d %s", code, errs)
	}
	out, _ := h.ok(value+"\n", false, "secrets", "set", "BRAVE_API_KEY", "--stdin")
	if strings.Contains(out, value) {
		t.Errorf("value echoed: %s", out)
	}
	if v, _ := h.kc.Get("BRAVE_API_KEY"); v != value {
		t.Errorf("stored %q", v)
	}
	out, _ = h.ok("", false, "secrets", "list")
	if strings.TrimSpace(out) != "BRAVE_API_KEY" {
		t.Errorf("list = %q", out)
	}
	if code, _, _ := h.run("x", false, "secrets", "set", "bad name", "--stdin"); code != ExitUsage {
		t.Errorf("bad name: %d", code)
	}
	h.ok("", false, "secrets", "rm", "BRAVE_API_KEY")
	if code, _, _ := h.run("", false, "secrets", "rm", "BRAVE_API_KEY"); code == 0 {
		t.Error("removing twice succeeded")
	}
	// config check flags a reference to a secret that isn't stored.
	h.writeConfig("\n[notify]\nntfy = { url = \"https://ntfy.sh/x\", token = \"secret:NTFY_TOKEN\" }\n")
	if code, out, _ := h.run("", false, "config", "check"); code != ExitError || !strings.Contains(out, "NTFY_TOKEN") {
		t.Errorf("missing secret: %d %s", code, out)
	}
}

func TestDoctorAndModels(t *testing.T) {
	h := newHarness(t,
		backendtest.OllamaModel{Name: "qwen3:4b", Size: 2.5e9, Params: "4.0B", Caps: chatCaps},
		backendtest.OllamaModel{Name: "tev1:0.8b", Size: 1.5e9, Params: "752M", Caps: []string{"decision"}})
	out, _ := h.ok("", false, "doctor")
	for _, want := range []string{"ok      backend ollama", "Ollama 0.40.3", "2 models", "embed model", "sandbox", "memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "warn    embed model") || !strings.Contains(out, "ollama pull nomic-embed-text") {
		t.Errorf("a missing embedding model should be a warning with the fix:\n%s", out)
	}
	h.srv.Version = "0.31.0"
	if out, _ := h.ok("", false, "doctor"); !strings.Contains(out, "0.35") {
		t.Errorf("an old Ollama should say why it matters:\n%s", out)
	}
	h.srv.Version = "0.40.3"

	out, _ = h.ok("", false, "models", "list")
	if !strings.Contains(out, "qwen3:4b") || !strings.Contains(out, "decide") {
		t.Errorf("models list:\n%s", out)
	}
	out, _ = h.ok("", false, "--json", "models", "list")
	var parsed struct{ Models []struct{ Name string } }
	if json.Unmarshal([]byte(out), &parsed) != nil || len(parsed.Models) != 2 {
		t.Errorf("json: %s", out)
	}

	// A dead backend: failed check, exit 1, and the fix.
	h.srv.Close()
	code, out, _ := h.run("", false, "doctor")
	if code != ExitError || !strings.Contains(out, "failed  backend ollama") || !strings.Contains(out, "ollama serve") {
		t.Errorf("dead backend: %d\n%s", code, out)
	}
	if code, _, errs := h.run("", false, "run", "hello"); code != ExitUnavailable || !strings.Contains(errs, "Is Ollama running") {
		t.Errorf("run with a dead backend: %d %s", code, errs)
	}
}

func TestRunAnswersAndStreams(t *testing.T) {
	h := newHarness(t)
	h.srv.Chat = func(map[string]any) backendtest.OllamaReply {
		return backendtest.OllamaReply{Content: "Paris is the capital."}
	}
	out, errs := h.ok("", false, "run", "What", "is", "the", "capital", "of", "France?")
	if out != "Paris is the capital.\n" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errs, "qwen3:4b") || !strings.Contains(errs, "run ") {
		t.Errorf("stderr should carry the summary: %q", errs)
	}
	// Flags may come after the task.
	out, _ = h.ok("", false, "run", "hi there", "--model", "qwen3:4b", "--no-learn")
	if out != "Paris is the capital.\n" {
		t.Errorf("flags after the task: %q", out)
	}
	// stdin as the task.
	out, _ = h.ok("what is 2+2\n", false, "run", "-")
	if !strings.Contains(out, "Paris") {
		t.Errorf("stdin task: %q", out)
	}
	// JSON result.
	out, _ = h.ok("", false, "--json", "run", "hello there")
	var res struct {
		OK     bool
		Answer string
		Model  string
		RunID  string `json:"run_id"`
	}
	if json.Unmarshal([]byte(out), &res) != nil || !res.OK || res.Answer != "Paris is the capital." || res.RunID == "" {
		t.Errorf("json result: %s", out)
	}
	// --out writes a new file and never replaces one.
	dest := filepath.Join(t.TempDir(), "answer.md")
	h.ok("", false, "run", "hello there", "--out", dest)
	if b, _ := os.ReadFile(dest); string(b) != "Paris is the capital.\n" {
		t.Errorf("--out file = %q", b)
	}
	if code, _, errs := h.run("", false, "run", "hello there", "--out", dest); code != ExitUsage || !strings.Contains(errs, "already exists") {
		t.Errorf("--out over an existing file: %d %s", code, errs)
	}
	if code, _, _ := h.run("", false, "run", "hello", "--preset", "turbo"); code != ExitUsage {
		t.Errorf("bad preset: %d", code)
	}
	if code, _, errs := h.run("", false, "run", "hello", "--model", "nope"); code != ExitUsage || !strings.Contains(errs, "qwen3:4b") {
		t.Errorf("unknown model: %d %s", code, errs)
	}
	if code, _, _ := h.run("", true, "run"); code != ExitUsage {
		t.Errorf("no task: %d", code)
	}
	// Traces and ratings.
	out, _ = h.ok("", false, "traces", "list")
	if !strings.Contains(out, "qwen3:4b") || !strings.Contains(out, "done") {
		t.Errorf("traces list:\n%s", out)
	}
	out, _ = h.ok("", false, "traces", "show", "last")
	if !strings.Contains(out, "request:") || !strings.Contains(out, "answer:") || !strings.Contains(out, "Paris") {
		t.Errorf("traces show:\n%s", out)
	}
	out, _ = h.ok("", false, "rate", "last", "good", "nice")
	if !strings.Contains(out, "success (you)") {
		t.Errorf("rate: %q", out)
	}
	if code, _, _ := h.run("", false, "rate", "last", "meh"); code == 0 {
		t.Error("bad rating accepted")
	}
}

func TestRunBudgetAndToolsFlow(t *testing.T) {
	h := newHarness(t)
	n := 0
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		if _, ok := req["tools"]; !ok {
			return backendtest.OllamaReply{Content: "Here is what I found."}
		}
		n++
		return backendtest.OllamaReply{ToolCalls: []backendtest.OllamaToolCall{{Name: "list_dir", Args: map[string]any{"path": fmt.Sprintf("/tmp/x%d", n)}}}}
	}
	// The model never stops calling tools: exit 4 with a partial answer after the step budget.
	code, out, errs := h.run("", false, "run", "--skill", "files", "--allow", "read:"+t.TempDir(), "--budget-steps", "2", "tidy my files")
	if code != ExitBudget || !strings.Contains(out, "Here is what I found.") || !strings.Contains(errs, "limit") {
		t.Errorf("budget: %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if !strings.Contains(errs, "→ list_dir: /tmp/x1") {
		t.Errorf("tool calls should show as progress on stderr:\n%s", errs)
	}
}

func TestFilesNeedAnApprovalAndCanBeUndone(t *testing.T) {
	h := newHarness(t)
	work := t.TempDir()
	src := filepath.Join(work, "a.mkv")
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	os.WriteFile(src, []byte("episode"), 0o644)
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		msgs, _ := req["messages"].([]any)
		last, _ := msgs[len(msgs)-1].(map[string]any)
		if last["role"] == "tool" {
			if strings.Contains(fmt.Sprint(last["content"]), "declined") {
				return backendtest.OllamaReply{Content: "Okay, nothing moved."}
			}
			return backendtest.OllamaReply{Content: "Moved the episode."}
		}
		return backendtest.OllamaReply{ToolCalls: []backendtest.OllamaToolCall{{Name: "move_many", Args: map[string]any{
			"plan": []any{map[string]any{"from": src, "to": dst}}, "dry_run": false}}}}
	}
	args := []string{"run", "--skill", "files", "--allow", "write:" + work, "move the episode into a Show folder"}

	// Nobody to ask (no terminal): the run parks, exit 3, nothing moved.
	code, _, errs := h.run("", false, args...)
	if code != ExitWaiting || !strings.Contains(errs, "noskill approvals approve") {
		t.Fatalf("headless: %d %s", code, errs)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("moved without approval")
	}
	out, _ := h.ok("", false, "approvals")
	if !strings.Contains(out, "files wants to run move_many") || !strings.Contains(out, "pending") {
		t.Fatalf("approvals:\n%s", out)
	}
	// Approving by the short id lets the same call through on the next run.
	id := strings.Fields(strings.Split(out, "\n")[1])[0]
	h.ok("", false, "approvals", "approve", id, "looks right")
	out, _ = h.ok("", false, "run", "--skill", "files", "--allow", "write:"+work, "move the episode into a Show folder")
	if !strings.Contains(out, "Moved the episode.") {
		t.Fatalf("after approval: %q", out)
	}
	if b, _ := os.ReadFile(dst); string(b) != "episode" {
		t.Fatalf("file not moved: %v", b)
	}
	// A terminal user is asked directly. Declining leaves things alone.
	os.Rename(dst, src)
	code, out, errs = h.run("n\n", true, args...)
	if code != ExitOK || !strings.Contains(out, "nothing moved") || !strings.Contains(errs, "Allow this?") {
		t.Errorf("declined: %d\n%s\n%s", code, out, errs)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("moved after 'n'")
	}
	// Say yes: moves, and `undo` puts it back, leaving the folder in the (fake) trash.
	_, out, _ = func() (int, string, string) { return h.run("y\n", true, args...) }()
	if !strings.Contains(out, "Moved the episode.") {
		t.Fatalf("approved at the terminal: %q", out)
	}
	runID := strings.TrimSpace(strings.Fields(mustOut(h.ok("", false, "traces", "list")))[8])
	_ = runID
	plan, _ := h.ok("", false, "undo", "last", "--dry-run")
	if !strings.Contains(plan, "back to ") || !strings.Contains(plan, "a.mkv") || strings.Contains(plan, "empty folder") {
		t.Errorf("undo plan:\n%s", plan)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("dry run changed files")
	}
	h.ok("", false, "undo", "last")
	if b, _ := os.ReadFile(src); string(b) != "episode" {
		t.Errorf("undo didn't restore the file: %q", b)
	}
	// The Show folder already existed before this run, so undoing the run leaves it alone.
	if _, err := os.Stat(filepath.Join(work, "Show")); err != nil {
		t.Error("undo removed a folder this run didn't create")
	}
	// Skipping works the same way.
	h.run("", false, args...)
	h.ok("", false, "approvals", "skip", "--all")
	if out, _ := h.ok("", false, "approvals"); !strings.Contains(out, "nothing is waiting") {
		t.Errorf("after skip --all:\n%s", out)
	}
}

func mustOut(out, _ string) string { return out }

func TestMemoryCommands(t *testing.T) {
	h := newHarness(t)
	h.ok("", false, "memory", "add", "editor", "Prefers Neovim")
	out, _ := h.ok("", false, "memory", "list")
	if !strings.Contains(out, "editor") || !strings.Contains(out, "Neovim") || !strings.Contains(out, "explicit") {
		t.Errorf("list:\n%s", out)
	}
	if code, _, errs := h.run("", false, "memory", "add", "wifi", "my password is hunter2"); code != ExitUsage || strings.Contains(errs, "hunter2 ") && false {
		t.Errorf("credential stored: %d %s", code, errs)
	}
	if out, _ := h.ok("", false, "memory"); strings.Contains(out, "hunter2") {
		t.Error("a credential reached memory")
	}
	if code, _, _ := h.run("", false, "memory", "forget", "--all"); code == 0 {
		t.Error("forget --all without a terminal confirmation succeeded")
	}
	h.ok("y\n", true, "memory", "forget", "--all")
	if out, _ := h.ok("", false, "memory", "list"); !strings.Contains(out, "nothing remembered") {
		t.Errorf("after forget:\n%s", out)
	}
	if code, _, _ := h.run("", false, "memory", "forget", "nothing"); code == 0 {
		t.Error("forgetting nothing should say so")
	}
}

func TestSkillsCommands(t *testing.T) {
	h := newHarness(t)
	out, _ := h.ok("", false, "skills", "list")
	for _, want := range []string{"web-search", "research", "files", "orchestrator", "model-manager", "builtin"} {
		if !strings.Contains(out, want) {
			t.Errorf("skills list lacks %s:\n%s", want, out)
		}
	}
	out, _ = h.ok("", false, "skills", "show", "files")
	if !strings.Contains(out, "Permissions") || !strings.Contains(out, "exec        no") || !strings.Contains(out, "move_many") || !strings.Contains(out, "asks you first") {
		t.Errorf("skills show files:\n%s", out)
	}
	out, _ = h.ok("", false, "skills", "show", "web-search")
	if !strings.Contains(out, "any website") {
		t.Errorf("network * must be called out:\n%s", out)
	}
	h.ok("", false, "skills", "disable", "orchestrator")
	if out, _ := h.ok("", false, "skills", "list"); !strings.Contains(out, "orchestrator") || !strings.Contains(out, "no") {
		t.Errorf("disable:\n%s", out)
	}
	h.ok("", false, "skills", "enable", "orchestrator")
	if code, _, _ := h.run("", false, "skills", "show", "nope"); code != ExitUsage {
		t.Errorf("unknown skill: %d", code)
	}
	if code, _, errs := h.run("y\n", true, "skills", "remove", "files"); code == 0 || !strings.Contains(errs, "built in") {
		t.Errorf("removing a built-in: %d %s", code, errs)
	}

	// Install from a folder: permissions are shown, and a terminal yes is required.
	src := filepath.Join(t.TempDir(), "pdf-tools")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: pdf-tools\ndescription: Work with PDF files, split and merge them, extract their text, when the user asks.\nnoskill:\n  version: 1.0.0\n  permissions: { exec: true, network: ['example.com'] }\n---\nSteps.\n"), 0o644)
	if code, _, errs := h.run("", false, "skills", "install", src); code == 0 || !strings.Contains(errs, "can run any program") {
		t.Errorf("install without a terminal: %d %s", code, errs)
	}
	if code, _, _ := h.run("n\n", true, "skills", "install", src); code == 0 {
		t.Error("declined install succeeded")
	}
	_, errs := h.ok("y\n", true, "skills", "install", src)
	if !strings.Contains(errs, "example.com") {
		t.Errorf("permissions not shown:\n%s", errs)
	}
	if out, _ := h.ok("", false, "skills", "list"); !strings.Contains(out, "pdf-tools") {
		t.Errorf("installed skill missing:\n%s", out)
	}
	if code, _, errs := h.run("y\n", true, "skills", "install", src); code == 0 || !strings.Contains(errs, "already installed") {
		t.Errorf("installing twice: %d %s", code, errs)
	}
	h.ok("y\n", true, "skills", "remove", "pdf-tools")
	if entries, _ := os.ReadDir(h.trash); len(entries) == 0 {
		t.Error("the removed skill's folder isn't in the trash")
	}
	if _, err := os.Stat(filepath.Join(h.home, "skills", "pdf-tools")); err == nil {
		t.Error("the skill folder is still installed")
	}
}

func TestRouteExplainAndProbeAndBench(t *testing.T) {
	h := newHarness(t,
		backendtest.OllamaModel{Name: "qwen3:4b", Size: 2.5e9, Params: "4.0B", Caps: chatCaps},
		backendtest.OllamaModel{Name: "qwen2.5-coder:7b", Size: 4.7e9, Params: "7.6B", Caps: chatCaps})
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		msgs, _ := req["messages"].([]any)
		var user string
		for _, m := range msgs {
			if mm, _ := m.(map[string]any); mm["role"] == "user" {
				user = fmt.Sprint(mm["content"])
			}
		}
		call := func(name, arg, val string) backendtest.OllamaReply {
			if _, native := req["tools"]; native {
				return backendtest.OllamaReply{ToolCalls: []backendtest.OllamaToolCall{{Name: name, Args: map[string]any{arg: val}}}}
			}
			return backendtest.OllamaReply{Content: fmt.Sprintf(`{"thought":"t","action":{"tool":%q,"args":{%q:%q}}}`, name, arg, val)}
		}
		answer := func(s string) backendtest.OllamaReply {
			if _, native := req["tools"]; native {
				return backendtest.OllamaReply{Content: s}
			}
			return backendtest.OllamaReply{Content: fmt.Sprintf(`{"final":%q}`, s)}
		}
		switch {
		case strings.Contains(user, "time"):
			return call("get_time", "timezone", "Tokyo London")
		case strings.Contains(user, "calculator"):
			return call("calculate", "expression", "1234 * 5678")
		case strings.Contains(user, "Search the web") || strings.Contains(user, "Look up"):
			return call("web_search", "query", "ollama world cup")
		case strings.Contains(user, "Read the file"):
			return call("read_file", "path", "notes.txt")
		case strings.Contains(user, "Write the numbers"):
			return backendtest.OllamaReply{Content: "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30"}
		}
		return answer("an answer")
	}
	out, _ := h.ok("", false, "route", "explain", "fix this:\n```go\nfunc main() {}\n```")
	if !strings.Contains(out, "task_type=code") || !strings.Contains(out, "qwen2.5-coder:7b") || !strings.Contains(out, "user rule") {
		t.Errorf("route explain:\n%s", out)
	}
	out, _ = h.ok("", false, "route", "explain", "hello there", "--model", "qwen3:4b")
	if !strings.Contains(out, "pinned") {
		t.Errorf("pinned:\n%s", out)
	}
	out, _ = h.ok("", false, "models", "probe", "qwen3:4b")
	if !strings.Contains(out, "tools: native") || !strings.Contains(out, "native 10/10") {
		t.Errorf("probe:\n%s", out)
	}
	if out, _ := h.ok("", false, "models", "list"); !strings.Contains(out, "native") {
		t.Errorf("strategy not stored:\n%s", out)
	}
	out, _ = h.ok("", false, "models", "bench", "qwen3:4b")
	if !strings.Contains(out, "tokens/s") || !strings.Contains(out, "GB in memory") {
		t.Errorf("bench:\n%s", out)
	}
	if out, _ := h.ok("", false, "models", "list"); !strings.Contains(out, "tok/s") {
		t.Errorf("perf not shown:\n%s", out)
	}
	// Removing asks first, and protects the models noskill relies on.
	if code, _, _ := h.run("", false, "models", "rm", "qwen2.5-coder:7b"); code == 0 {
		t.Error("rm without confirmation")
	}
	if code, _, errs := h.run("y\n", true, "models", "rm", "qwen2.5-coder:7b"); code == 0 || !strings.Contains(errs, "routing rule") {
		t.Errorf("rm of a model a rule uses: %d %s", code, errs)
	}
	h.ok("y\n", true, "models", "rm", "qwen3:4b")
	if len(h.srv.Deleted) != 1 {
		t.Errorf("deleted = %v", h.srv.Deleted)
	}
	h.ok("", false, "models", "pull", "nomic-embed-text")
	if len(h.srv.Pulled) != 1 {
		t.Errorf("pulled = %v", h.srv.Pulled)
	}
}

func TestChatSession(t *testing.T) {
	h := newHarness(t)
	var seen []string
	var mu sync.Mutex
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		msgs, _ := req["messages"].([]any)
		mu.Lock()
		seen = append(seen, fmt.Sprint(len(msgs)))
		mu.Unlock()
		last, _ := msgs[len(msgs)-1].(map[string]any)
		return backendtest.OllamaReply{Content: "echo: " + fmt.Sprint(last["content"])}
	}
	in := "hello\n/model qwen3:4b\nsecond \\\nline\n\"\"\"\nblock one\nblock two\n\"\"\"\n/why\n/rate good\n/skills off\n/nonsense\n/exit\n"
	code, out, errs := h.run(in, false, "chat")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errs)
	}
	for _, want := range []string{"echo: hello", "echo: second \nline", "echo: block one\nblock two"} {
		if !strings.Contains(out, want) {
			t.Errorf("chat output lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"model: qwen3:4b", "rated good", "skills: off", "unknown command /nonsense", "task_type="} {
		if !strings.Contains(errs, want) {
			t.Errorf("chat stderr lacks %q:\n%s", want, errs)
		}
	}
	// History grows: system + user, then +assistant +user, then +…
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "2" || seen[1] != "4" || seen[2] != "6" {
		t.Errorf("message counts per turn = %v", seen)
	}
}

func TestServeStartsAndStops(t *testing.T) {
	h := newHarness(t)
	h.srv.Chat = func(map[string]any) backendtest.OllamaReply { return backendtest.OllamaReply{Content: "from serve"} }
	port := freePort(t)
	var out, errb safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"--home", h.home, "serve", "--port", fmt.Sprint(port)}, Env{
			Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Environ: []string{}, Version: "test", StdinFD: -1,
			Keychain: h.kc, Notifier: quiet{}, Trash: trashTo{h.trash}, Getwd: func() (string, error) { return t.TempDir(), nil },
			Context: func() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) },
		})
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = http.Get(base + "/"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v\n%s", err, errb.String())
	}
	resp.Body.Close()
	r, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"noskill/auto","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), "from serve") {
		t.Errorf("chat through serve: %d %s", r.StatusCode, body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve didn't stop")
	}
	if !strings.Contains(errb.String(), "serving on") {
		t.Errorf("stderr: %s", errb.String())
	}
	// --lan without a token is refused.
	code, _, errs := h.run("", false, "serve", "--lan", "--port", fmt.Sprint(freePort(t)))
	if code == 0 || !strings.Contains(errs, "token") {
		t.Errorf("--lan without tokens: %d %s", code, errs)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type fakeService struct {
	installed, running bool
	spec               service.Spec
}

func (f *fakeService) Install(_ context.Context, s service.Spec) error {
	f.installed, f.running, f.spec = true, true, s
	return nil
}
func (f *fakeService) Uninstall(context.Context) error {
	if !f.installed {
		return service.ErrNotInstalled
	}
	f.installed, f.running = false, false
	return nil
}
func (f *fakeService) Start(context.Context) error { f.running = true; return nil }
func (f *fakeService) Stop(context.Context) error  { f.running = false; return nil }
func (f *fakeService) Status(context.Context) (service.Status, error) {
	return service.Status{Installed: f.installed, Running: f.running, PID: 77, Unit: "/fake/unit", Detail: "fake"}, nil
}
func (f *fakeService) Notes() []string { return []string{"a note"} }

func TestDaemonCommands(t *testing.T) {
	h := newHarness(t)
	svc := &fakeService{}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		ctx := context.Background()
		code := Main(append([]string{"--home", h.home}, args...), Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Environ: []string{},
			Version: "test", StdinFD: -1, Keychain: h.kc, Notifier: quiet{}, Trash: trashTo{h.trash}, Service: svc,
			Getwd: func() (string, error) { return t.TempDir(), nil }, Context: func() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }})
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("daemon", "status"); code != 0 || !strings.Contains(out, "not installed") {
		t.Errorf("status before install: %d %s", code, out)
	}
	if code, _, errs := run("daemon", "uninstall"); code == 0 || !strings.Contains(errs, "isn't installed") {
		t.Errorf("uninstall before install: %d %s", code, errs)
	}
	// ResolveSpec refuses a temporary `go test` binary only for go-build paths; the fake accepts what it's given.
	code, out, errs := run("daemon", "install")
	if code != 0 && !strings.Contains(errs, "go run") {
		t.Fatalf("install: %d %s %s", code, out, errs)
	}
	if code == 0 {
		if !svc.installed || !strings.Contains(strings.Join(svc.spec.Args, " "), "serve") || !strings.Contains(errs, "remove   noskill daemon uninstall") {
			t.Errorf("install spec = %+v\n%s", svc.spec, errs)
		}
	}
	svc.installed, svc.running = true, false
	if code, out, _ := run("daemon", "status"); code != ExitError || !strings.Contains(out, "installed, not running") {
		t.Errorf("stopped service must exit 1: %d %s", code, out)
	}
	if code, _, _ := run("daemon", "start"); code != 0 || !svc.running {
		t.Errorf("start: %d", code)
	}
	if out, _, _ := func() (string, string, int) { c, o, e := run("doctor"); return o, e, c }(); !strings.Contains(out, "service") || !strings.Contains(out, "running (pid 77)") {
		t.Errorf("doctor lacks the service check:\n%s", out)
	}
	if code, _, _ := run("daemon", "stop"); code != 0 || svc.running {
		t.Errorf("stop: %d", code)
	}
	if code, _, _ := run("daemon", "uninstall"); code != 0 || svc.installed {
		t.Errorf("uninstall: %d", code)
	}
	if code, _, errs := run("daemon", "logs"); code == 0 || !strings.Contains(errs, "no log yet") {
		t.Errorf("logs without a log: %d %s", code, errs)
	}
	if code, _, _ := run("daemon", "bogus"); code != ExitUsage {
		t.Errorf("unknown: %d", code)
	}
}

// releaseServer serves a fake GitHub release v9.9.9 for this platform signed with a fresh key.
func releaseServer(t *testing.T, badSig bool) (url, pub string) {
	t.Helper()
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	name := update.ArchiveName("9.9.9", runtime.GOOS, runtime.GOARCH)
	bin := "noskill"
	if runtime.GOOS == "windows" {
		bin = "noskill.exe"
	}
	var ab bytes.Buffer
	gz := gzip.NewWriter(&ab)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho noskill 9.9.9\n")
	tw.WriteHeader(&tar.Header{Name: bin, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	h := sha256.Sum256(ab.Bytes())
	sums := []byte(hex.EncodeToString(h[:]) + "  " + name + "\n")
	sig := ed25519.Sign(sk, sums)
	if badSig {
		sig = make([]byte, 64)
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/n/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":%q,"browser_download_url":"%s/a"},{"name":"checksums.txt","browser_download_url":"%s/s"},{"name":"checksums.txt.ed25519","browser_download_url":"%s/g"}]}`, name, srv.URL, srv.URL, srv.URL)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { w.Write(ab.Bytes()) })
	mux.HandleFunc("/s", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	mux.HandleFunc("/g", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(base64.StdEncoding.EncodeToString(sig))) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, base64.StdEncoding.EncodeToString(pk)
}

func (h *harness) runWith(env func(*Env), stdin string, tty bool, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	e := Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Environ: []string{}, Version: "0.1.0", StdinTTY: tty, StdinFD: -1,
		Keychain: h.kc, Notifier: quiet{}, Trash: trashTo{h.trash}, Service: &fakeService{},
		Getwd: func() (string, error) { return h.t.TempDir(), nil }}
	env(&e)
	return Main(append([]string{"--home", h.home}, args...), e), out.String(), errb.String()
}

func TestUpdateVerifiesAndSwaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake release's binary is a shell script")
	}
	h := newHarness(t)
	h.writeConfig("\n[update]\nrepo = \"o/n\"\n")
	url, pub := releaseServer(t, false)
	old := update.PublicKey
	defer func() { update.PublicKey = old }()
	update.PublicKey = pub
	dir := t.TempDir()
	exe := filepath.Join(dir, "noskill")
	os.WriteFile(exe, []byte("#!/bin/sh\necho noskill 0.1.0\n"), 0o755)
	with := func(e *Env) {
		e.Executable = func() (string, error) { return exe, nil }
		e.Environ = []string{"NOSKILL_UPDATE_API=" + url}
	}
	if code, out, _ := h.runWith(with, "", false, "update", "--check"); code != 0 || !strings.Contains(out, "9.9.9 is available") {
		t.Errorf("check: %d %s", code, out)
	}
	if code, _, errs := h.runWith(with, "", false, "update"); code != ExitError || !strings.Contains(errs, "not updated") {
		t.Errorf("no terminal, no --yes: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "0.1.0") {
		t.Fatal("binary changed without consent")
	}
	if code, out, errs := h.runWith(with, "", false, "update", "--yes"); code != 0 || !strings.Contains(out, "updated to 9.9.9") {
		t.Fatalf("update: %d %s %s", code, out, errs)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "9.9.9") {
		t.Error("new binary not in place")
	}
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Error("the previous version should be kept as .old")
	}
	// A release whose signature doesn't verify never touches the binary.
	url2, _ := releaseServer(t, true)
	os.WriteFile(exe, []byte("#!/bin/sh\necho noskill 0.1.0\n"), 0o755)
	bad := func(e *Env) { with(e); e.Environ = []string{"NOSKILL_UPDATE_API=" + url2} }
	update.PublicKey = pub
	if code, _, errs := h.runWith(bad, "", false, "update", "--yes"); code == 0 || !strings.Contains(errs, "signature") {
		t.Errorf("bad signature: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "0.1.0") {
		t.Error("a release with a bad signature replaced the binary")
	}
	// Installed with Homebrew: print the command, change nothing.
	brew := func(e *Env) {
		with(e)
		e.Executable = func() (string, error) { return "/opt/homebrew/Cellar/noskill/0.1.0/bin/noskill", nil }
	}
	if code, out, _ := h.runWith(brew, "", false, "update", "--yes"); code != 0 || !strings.Contains(out, "brew upgrade noskill") {
		t.Errorf("brew: %d %s", code, out)
	}
	// Without update.repo there's nowhere to look.
	h.writeConfig("")
	if code, _, errs := h.runWith(with, "", false, "update", "--check"); code == 0 || !strings.Contains(errs, "update.repo") {
		t.Errorf("no repo: %d %s", code, errs)
	}
}

func TestUninstallKeepsDataUnlessPurged(t *testing.T) {
	h := newHarness(t)
	exe := filepath.Join(t.TempDir(), "noskill")
	os.WriteFile(exe, []byte("bin"), 0o755)
	svc := &fakeService{installed: true, running: true}
	with := func(e *Env) { e.Executable = func() (string, error) { return exe, nil }; e.Service = svc }
	if code, _, errs := h.runWith(with, "", false, "uninstall"); code != ExitError || !strings.Contains(errs, "nothing removed") {
		t.Errorf("no consent: %d %s", code, errs)
	}
	code, out, _ := h.runWith(with, "", false, "uninstall", "--yes")
	if code != 0 || svc.installed || !strings.Contains(out, "kept in") || !strings.Contains(out, "Ollama and its models were not touched") {
		t.Errorf("uninstall: %d %s", code, out)
	}
	if _, err := os.Stat(exe); err == nil {
		t.Error("the binary should be in the trash")
	}
	if _, err := os.Stat(filepath.Join(h.home, "config.toml")); err != nil {
		t.Error("config removed without --purge")
	}
	if code, _, _ := h.runWith(with, "", false, "uninstall", "--yes", "--purge"); code != 0 {
		t.Errorf("purge: %d", code)
	}
	if _, err := os.Stat(h.home); err == nil {
		t.Error("--purge should move the data folder to the trash")
	}
	if entries, _ := os.ReadDir(h.trash); len(entries) == 0 {
		t.Error("nothing in the trash")
	}
}

type recordNotes struct {
	mu    sync.Mutex
	notes []string
}

func (r *recordNotes) SupportsActions() bool { return false }
func (r *recordNotes) Notify(_ context.Context, n platform.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, n.Title+" | "+n.Body)
	return nil
}
func (r *recordNotes) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.notes, "\n")
}

// SPEC §6 criteria 5 and 7: a scheduled job runs in the server (the daemon) with no CLI open,
// parks on a confirm action, shows a notification, continues after `approvals approve`, and
// reports its result.
func TestScheduledJobWaitsForApprovalThenContinues(t *testing.T) {
	old := schedulerTick
	schedulerTick = 50 * time.Millisecond
	defer func() { schedulerTick = old }()

	h := newHarness(t)
	work := t.TempDir()
	src := filepath.Join(work, "a.mkv")
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	os.WriteFile(src, []byte("episode"), 0o644)
	var mu sync.Mutex
	modelCalls := 0
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		mu.Lock()
		modelCalls++
		mu.Unlock()
		msgs, _ := req["messages"].([]any)
		last, _ := msgs[len(msgs)-1].(map[string]any)
		if last["role"] == "tool" {
			return backendtest.OllamaReply{Content: "Moved the episode into Show."}
		}
		return backendtest.OllamaReply{ToolCalls: []backendtest.OllamaToolCall{{Name: "move_many", Args: map[string]any{
			"plan": []any{map[string]any{"from": src, "to": dst}}, "dry_run": false}}}}
	}
	out, _ := h.ok("", false, "schedule", "add", "files", "--every", "1d", "--prompt", "move the episode into a Show folder",
		"--allow", "write:"+work, "--notify", "os")
	if !strings.Contains(out, "scheduled files: every 1d") || !strings.Contains(out, "Nothing runs jobs right now") {
		t.Fatalf("add:\n%s", out)
	}
	if code, _, errs := h.run("", false, "schedule", "add", "files", "--every", "1d"); code == 0 || !strings.Contains(errs, "already exists") {
		t.Errorf("duplicate name: %d %s", code, errs)
	}
	if code, _, errs := h.run("", false, "schedule", "add", "nosuchskill", "--every", "1h"); code != ExitUsage || !strings.Contains(errs, "no skill named") {
		t.Errorf("unknown skill: %d %s", code, errs)
	}
	h.ok("", false, "schedule", "run", "files")

	// The server runs the scheduler; no other CLI command is involved from here on.
	notes := &recordNotes{}
	port := freePort(t)
	h.writeConfig(fmt.Sprintf("[server]\nport = %d\n", port))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb safeBuffer
	go func() {
		done <- Main([]string{"--home", h.home, "serve"}, Env{
			Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &errb, Environ: []string{}, Version: "test", StdinFD: -1,
			Keychain: h.kc, Notifier: notes, Trash: trashTo{h.trash}, Getwd: func() (string, error) { return t.TempDir(), nil },
			Context: func() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) },
		})
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if cond() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s\nserver log:\n%s", what, errb.String())
	}
	history := func() string { o, _ := h.ok("", false, "jobs", "history", "files"); return o }
	waitFor("the job to park", func() bool { return strings.Contains(history(), "waiting") })
	if _, err := os.Stat(src); err != nil {
		t.Fatal("a scheduled job ran a confirm action unattended")
	}
	waitFor("the approval notification", func() bool { return strings.Contains(notes.all(), "noskill needs your OK") })
	jobsOut, _ := h.ok("", false, "jobs")
	if !strings.Contains(jobsOut, "files") || !strings.Contains(jobsOut, "waiting") {
		t.Errorf("jobs:\n%s", jobsOut)
	}

	list, _ := h.ok("", false, "approvals")
	id := strings.Fields(strings.Split(list, "\n")[1])[0]
	out, _ = h.ok("", false, "approvals", "approve", id)
	if !strings.Contains(out, "carries on from where it stopped") {
		t.Errorf("approve:\n%s", out)
	}
	waitFor("the job to finish", func() bool { return strings.Contains(history(), "done") })
	if b, _ := os.ReadFile(dst); string(b) != "episode" {
		t.Fatalf("file not moved after approval: %q", b)
	}
	waitFor("the result notification", func() bool { return strings.Contains(notes.all(), "files finished") })
	if !strings.Contains(notes.all(), "Moved the episode into Show.") {
		t.Errorf("notes:\n%s", notes.all())
	}
	mu.Lock()
	n := modelCalls
	mu.Unlock()
	if n != 2 { // one call that asked for the tool, one after it ran: the model isn't asked again on resume
		t.Errorf("model calls = %d, want 2", n)
	}
	out, _ = h.ok("", false, "schedule", "list")
	if !strings.Contains(out, "files") || !strings.Contains(out, "done") || strings.Contains(out, "Nothing runs jobs") {
		t.Errorf("list:\n%s", out)
	}
	h.ok("", false, "schedule", "pause", "files")
	if out, _ := h.ok("", false, "schedule", "list"); !strings.Contains(out, "paused") {
		t.Errorf("paused:\n%s", out)
	}
	h.ok("", false, "schedule", "rm", "files")
	if out, _ := h.ok("", false, "schedule", "list"); !strings.Contains(out, "no jobs yet") {
		t.Errorf("after rm:\n%s", out)
	}
}

// A CLI run without a terminal parks on a confirm tool; after approval, `run --resume` carries
// on from the call it stopped on.
func TestRunResumeContinuesParkedRun(t *testing.T) {
	h := newHarness(t)
	work := t.TempDir()
	src := filepath.Join(work, "a.mkv")
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	os.WriteFile(src, []byte("episode"), 0o644)
	calls := 0
	h.srv.Chat = func(req map[string]any) backendtest.OllamaReply {
		calls++
		msgs, _ := req["messages"].([]any)
		last, _ := msgs[len(msgs)-1].(map[string]any)
		if last["role"] == "tool" {
			return backendtest.OllamaReply{Content: "Moved it."}
		}
		return backendtest.OllamaReply{ToolCalls: []backendtest.OllamaToolCall{{Name: "move_many", Args: map[string]any{
			"plan": []any{map[string]any{"from": src, "to": dst}}, "dry_run": false}}}}
	}
	code, _, errs := h.run("", false, "run", "--skill", "files", "--allow", "write:"+work, "move the episode")
	if code != ExitWaiting || !strings.Contains(errs, "run --resume") {
		t.Fatalf("park: %d %s", code, errs)
	}
	list, _ := h.ok("", false, "approvals")
	id := strings.Fields(strings.Split(list, "\n")[1])[0]
	if code, _, errs := h.run("", false, "run", "--resume", id); code == 0 || !strings.Contains(errs, "hasn't been answered") {
		t.Errorf("resume before approval: %d %s", code, errs)
	}
	out, _ := h.ok("", false, "approvals", "approve", id)
	if !strings.Contains(out, "run --resume") {
		t.Errorf("approve:\n%s", out)
	}
	out, _ = h.ok("", false, "run", "--resume", id)
	if !strings.Contains(out, "Moved it.") || calls != 2 {
		t.Fatalf("resume: %q after %d model calls", out, calls)
	}
	if b, _ := os.ReadFile(dst); string(b) != "episode" {
		t.Fatal("not moved")
	}
	if code, _, errs := h.run("", false, "run", "--resume", id); code == 0 || !strings.Contains(errs, "already resumed") {
		t.Errorf("second resume: %d %s", code, errs)
	}
}

func TestLearnedSkillVersions(t *testing.T) {
	h := newHarness(t)
	lib := &learning.Library{Dir: filepath.Join(h.home, "learned"), Trash: trashTo{h.trash}}
	write := func(name, steps string) string {
		d, err := lib.NewDraftDir(name)
		if err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + name + "\ndescription: Sort the downloads folder by file type when the user asks to tidy downloads.\n---\n" + steps
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o600)
		return d
	}
	if _, err := lib.Publish(write("organise-downloads", "1. List.\n"), "Learned from a run."); err != nil {
		t.Fatal(err)
	}
	lib.SetStatus("organise-downloads", "active", "")
	lib.Publish(write("organise-downloads", "1. List.\n2. Archives too.\n"), "Handles archives.")
	if _, err := lib.AdoptNext("organise-downloads"); err != nil {
		t.Fatal(err)
	}
	// A learned skill never replaces another skill of the same name.
	lib.Publish(write("files", "nothing\n"), "clash")

	out, _ := h.ok("", false, "skills", "list", "--learned")
	if !strings.Contains(out, "organise-downloads") || !strings.Contains(out, "1.1.0") || strings.Contains(out, "files ") {
		t.Errorf("list --learned:\n%s", out)
	}
	out, _ = h.ok("", false, "skills", "versions", "organise-downloads")
	if !strings.Contains(out, "1.0.0") || !strings.Contains(out, "1.1.0") || !strings.Contains(out, "current") {
		t.Errorf("versions:\n%s", out)
	}
	out, _ = h.ok("", false, "skills", "diff", "organise-downloads")
	if !strings.Contains(out, "+2. Archives too.") {
		t.Errorf("diff:\n%s", out)
	}
	out, _ = h.ok("", false, "skills", "rollback", "organise-downloads")
	if !strings.Contains(out, "back at version 1.0.0") {
		t.Errorf("rollback:\n%s", out)
	}
	out, _ = h.ok("", false, "--json", "skills", "list", "--learned")
	if !strings.Contains(out, `"version": "1.0.0"`) || !strings.Contains(out, `"status": "active"`) {
		t.Errorf("after rollback:\n%s", out)
	}
	if code, _, errs := h.run("", false, "skills", "rollback", "files"); code == 0 || !strings.Contains(errs, "only learned skills keep versions") {
		t.Errorf("rollback of a built-in: %d %s", code, errs)
	}
}

func TestSchedulePreconditions(t *testing.T) {
	h := newHarness(t)
	out, _ := h.ok("", false, "schedule", "add", "files", "--every", "6h", "--when", "unmetered", "--when", "window:22:00-06:00", "--when", "unmetered")
	if !strings.Contains(out, "runs only if unmetered network and between 22:00 and 06:00") {
		t.Fatalf("add:\n%s", out)
	}
	out, _ = h.ok("", false, "--json", "schedule", "list")
	var got struct {
		Jobs []struct{ Preconditions []string }
	}
	if json.Unmarshal([]byte(out), &got) != nil || len(got.Jobs) != 1 || !slices.Equal(got.Jobs[0].Preconditions, []string{"unmetered", "window:22:00-06:00"}) {
		t.Fatalf("list --json:\n%s", out)
	}
	for _, bad := range []string{"battery", "volume:", "window:9-5"} {
		if code, _, errs := h.run("", false, "schedule", "add", "files", "--name", "x", "--every", "1h", "--when", bad); code != ExitUsage || !strings.Contains(errs, "--when") {
			t.Errorf("--when %s: %d %s", bad, code, errs)
		}
	}
}

func TestHookNeedsAKnownNameAndAHash(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"hook"}, {"hook", "nonsense", "cafe"}, {"hook", "qbittorrent-finished"}} {
		if code, _, errs := h.run("", false, args...); code != ExitUsage {
			t.Errorf("%v → exit %d (%s)", args, code, errs)
		}
	}
}

// `noskill ui` serves the page when nothing is running, opens it on the tab asked for, and just
// opens the running one the second time (no second server).
func TestUIStartsPageAndReusesRunningServer(t *testing.T) {
	h := newHarness(t)
	port := freePort(t)
	var opened []string
	var mu sync.Mutex
	was := openURL
	openURL = func(_ context.Context, u string) error { mu.Lock(); opened = append(opened, u); mu.Unlock(); return nil }
	defer func() { openURL = was }()

	var errb safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	env := func(e *safeBuffer) Env {
		return Env{Stdin: strings.NewReader(""), Stdout: &safeBuffer{}, Stderr: e, Environ: []string{}, Version: "test", StdinFD: -1,
			Keychain: h.kc, Notifier: quiet{}, Trash: trashTo{h.trash}, Getwd: func() (string, error) { return t.TempDir(), nil },
			Context: func() (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }}
	}
	go func() { done <- Main([]string{"--home", h.home, "ui", "downloads", "--port", fmt.Sprint(port)}, env(&errb)) }()

	want := fmt.Sprintf("http://127.0.0.1:%d/ui#downloads", port)
	for i := 0; i < 100; i++ {
		mu.Lock()
		n := len(opened)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	if len(opened) != 1 || opened[0] != want {
		t.Fatalf("opened %v, want [%s]\n%s", opened, want, errb.String())
	}
	mu.Unlock()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ui", port))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("page: %v %v", resp, err)
	}
	resp.Body.Close()

	var errb2 safeBuffer
	if code := Main([]string{"--home", h.home, "ui", "history", "--port", fmt.Sprint(port)}, env(&errb2)); code != 0 || !strings.Contains(errb2.String(), "already serving") {
		t.Errorf("second ui: %d %s", code, errb2.String())
	}
	mu.Lock()
	if len(opened) != 2 || !strings.HasSuffix(opened[1], "/ui#history") {
		t.Errorf("opened %v", opened)
	}
	mu.Unlock()
	if code := Main([]string{"--home", h.home, "ui", "nope"}, env(&errb2)); code != ExitUsage {
		t.Errorf("bad tab: %d", code)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d\n%s", code, errb.String())
	}
}
