package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasIssue(issues []Issue, level, key, substr string) bool {
	for _, i := range issues {
		if i.Level == level && i.Key == key && strings.Contains(i.Msg, substr) {
			return true
		}
	}
	return false
}

func TestDefaultsPassCheck(t *testing.T) {
	l, err := Load(Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range Check(l, nil) {
		if i.Level == "error" {
			t.Errorf("defaults: %s", i)
		}
	}
	if l.Sources["server.port"] != LayerDefault {
		t.Errorf("source = %s", l.Sources["server.port"])
	}
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[server]
port = 11500
progress = "content"
[orchestrator]
memory_budget_gb = 6
[sandbox]
script_timeout = "90s"
`)
	l, err := Load(Options{
		File:  file,
		Env:   []string{"NOSKILL_SERVER_PORT=11600", "NOSKILL_AGENT_MAX_STEPS=9", "HOME=/x"},
		Flags: map[string]string{"server.port": "11700"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Server.Port != 11700 || l.Sources["server.port"] != LayerFlag {
		t.Errorf("port = %d from %s, want 11700 from flag", c.Server.Port, l.Sources["server.port"])
	}
	if c.Server.Progress != "content" || l.Sources["server.progress"] != LayerFile {
		t.Errorf("progress = %s from %s", c.Server.Progress, l.Sources["server.progress"])
	}
	if c.Agent.MaxSteps != 9 || l.Sources["agent.max_steps"] != LayerEnv {
		t.Errorf("max_steps = %d from %s", c.Agent.MaxSteps, l.Sources["agent.max_steps"])
	}
	if c.Orchestrator.MemoryBudgetGB.Auto || c.Orchestrator.MemoryBudgetGB.Value != 6 {
		t.Errorf("memory_budget_gb = %v", c.Orchestrator.MemoryBudgetGB)
	}
	if c.Sandbox.ScriptTimeout.Duration != 90*time.Second {
		t.Errorf("script_timeout = %v", c.Sandbox.ScriptTimeout)
	}
	if c.Server.Bind != "127.0.0.1" || l.Sources["server.bind"] != LayerDefault {
		t.Errorf("bind = %s from %s", c.Server.Bind, l.Sources["server.bind"])
	}
}

func TestUnknownKeysAreErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[server]
prot = 1
[[backends]]
kind = "ollama"
url = "http://127.0.0.1:11434"
urll = "typo"
[notify]
ntfy = { url = "https://ntfy.sh/x", tokn = "secret:N" }
`)
	l, err := Load(Options{File: file})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"server.prot", "backends.urll", "notify.ntfy.tokn"} {
		if !hasIssue(l.Issues, "error", k, "unknown key") {
			t.Errorf("no unknown-key error for %s: %v", k, l.Issues)
		}
	}
}

func TestTypeErrorHasLine(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, "[server]\nport = \"eleven\"\n")
	_, err := Load(Options{File: file})
	if err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectLayerIsRestricted(t *testing.T) {
	proj := t.TempDir()
	write(t, filepath.Join(proj, ".noskill", "config.toml"), `
[server]
bind = "0.0.0.0"
[sandbox]
mode = "warn"
[skills]
paths = ["skills"]
auto_update = true
[skills.anime-tracker]
default_destination = "/"
`)
	l, err := Load(Options{ProjectDir: proj})
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Server.Bind != "127.0.0.1" || c.Sandbox.Mode != "enforce" || c.Skills.AutoUpdate {
		t.Fatalf("project layer changed protected settings: bind=%s mode=%s auto_update=%v", c.Server.Bind, c.Sandbox.Mode, c.Skills.AutoUpdate)
	}
	for _, k := range []string{"server", "sandbox", "skills.auto_update"} {
		if !hasIssue(l.Issues, "warn", k, "ignored") {
			t.Errorf("no warning for %s", k)
		}
	}
	if len(c.Skills.Paths) != 1 || c.Skills.Paths[0] != filepath.Join(proj, "skills") {
		t.Errorf("paths = %v", c.Skills.Paths)
	}
	if got := l.SkillSources["anime-tracker"]["default_destination"]; got != LayerProject {
		t.Errorf("skill setting source = %s, want project", got)
	}
}

func TestSkillSettingsFromFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[skills]
paths = []
[skills.anime-tracker]
mode = "approve"
quality = { prefer = ["480p"], fallback_after = "12h" }
`)
	l, err := Load(Options{File: file})
	if err != nil {
		t.Fatal(err)
	}
	s := l.Config.Skills.Settings["anime-tracker"]
	if s["mode"] != "approve" {
		t.Errorf("settings = %v", s)
	}
	if len(l.Issues) != 0 {
		t.Errorf("issues = %v", l.Issues)
	}
}

func TestCheckCatchesUnsafeValues(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[server]
bind = "0.0.0.0"
tokens = [{ name = "owui", token = "sk-plain-value" }]
[approvals]
default_on_timeout = "approve"
[traces]
redact = ["emails", { regex = "(" }]
[sandbox]
mode = "warn"
`)
	l, err := Load(Options{File: file})
	if err != nil {
		t.Fatal(err)
	}
	issues := Check(l, nil)
	for _, want := range []struct{ level, key, msg string }{
		{"error", "server.tokens[0].token", "plain credential"},
		{"error", "approvals.default_on_timeout", "skip"},
		{"error", "traces.redact[1]", "bad regex"},
		{"warn", "traces.redact", "secrets"},
		{"warn", "sandbox.mode", "unsandboxed"},
	} {
		if !hasIssue(issues, want.level, want.key, want.msg) {
			t.Errorf("missing %s %s %q in %v", want.level, want.key, want.msg, issues)
		}
	}
	// A LAN bind with a token present is allowed; without tokens it's an error.
	l.Config.Server.Tokens = nil
	if !hasIssue(Check(l, nil), "error", "server.bind", "tokens") {
		t.Error("LAN bind without tokens not flagged")
	}
}

type fakeSecrets map[string]bool

func (f fakeSecrets) Has(n string) (bool, error) { return f[n], nil }

func TestSecretReferences(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[server]
tokens = [{ name = "owui", token = "secret:OWUI" }]
[skills.web-search]
brave = "secret:BRAVE"
`)
	l, err := Load(Options{File: file})
	if err != nil {
		t.Fatal(err)
	}
	issues := Check(l, fakeSecrets{"OWUI": true})
	if !hasIssue(issues, "error", "", "secret:BRAVE is referenced but not stored") {
		t.Errorf("issues = %v", issues)
	}
	if hasIssue(issues, "error", "", "secret:OWUI") {
		t.Error("OWUI is stored but flagged")
	}
}

func TestSetAndGet(t *testing.T) {
	c := Defaults()
	if err := Set(&c, "server.allowed_hosts", "host.docker.internal, example.test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(c, "server.allowed_hosts"); got != `"host.docker.internal,example.test"` {
		t.Errorf("get = %s", got)
	}
	if err := Set(&c, "server.tokens", "x"); err == nil {
		t.Error("arrays of tables should only be settable in the file")
	}
	if err := Set(&c, "nope.key", "1"); err == nil {
		t.Error("unknown key accepted")
	}
	if EnvName("orchestrator.memory_budget_gb") != "NOSKILL_ORCHESTRATOR_MEMORY_BUDGET_GB" {
		t.Error(EnvName("orchestrator.memory_budget_gb"))
	}
}

func TestCheckRulePresets(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	write(t, file, `
[[orchestrator.rules]]
when = { needs = ["tools"], preset = "best" }
use = "qwen3.5:4b"
[[orchestrator.rules]]
when = { preset = "turbo" }
use = "granite4:3b"
[[orchestrator.rules]]
when = {}
use = "granite4:3b"
`)
	l, err := Load(Options{File: file})
	if err != nil {
		t.Fatal(err)
	}
	issues := Check(l, nil)
	if hasIssue(issues, "error", "orchestrator.rules[0]", "") {
		t.Errorf("a valid preset rule was flagged: %v", issues)
	}
	if !hasIssue(issues, "error", "orchestrator.rules[1].when.preset", "fast, balanced or best") || !hasIssue(issues, "error", "orchestrator.rules[2].when", "preset") {
		t.Errorf("bad rules not flagged: %v", issues)
	}
}

// Run from the home folder, ~/.noskill/config.toml is both the user file and the "project" file; its
// per-skill settings must still count (they were ignored, so a run from ~ used the default Nyaa address).
func TestUserFileIsNotAlsoAProjectLayer(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".noskill", "config.toml")
	write(t, file, "[skills.anime-tracker]\nnyaa_url = \"https://nyaa.media\"\n")
	l, err := Load(Options{File: file, ProjectDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.SkillSources["anime-tracker"]["nyaa_url"]; got == LayerProject {
		t.Errorf("the user's own file was read as a project layer")
	}
	// A different project folder still is one.
	proj := t.TempDir()
	write(t, filepath.Join(proj, ".noskill", "config.toml"), "[skills.anime-tracker]\nnyaa_url = \"https://evil.example\"\n")
	l, err = Load(Options{File: file, ProjectDir: proj})
	if err != nil || l.SkillSources["anime-tracker"]["nyaa_url"] != LayerProject {
		t.Errorf("a real project config lost its layer: %v %v", err, l.SkillSources)
	}
}
