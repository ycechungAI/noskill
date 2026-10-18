package config

import (
	"strings"
	"testing"
)

const sample = `# my config
[server]
port = 11435          # the proxy port
bind = "127.0.0.1"

[orchestrator]
preset = "balanced"
# memory_budget_gb = 8

[[orchestrator.rules]]
when = { task_type = "code" }
use = "qwen2.5-coder:7b"

[skills.files]
allow = "write:~/Downloads"
`

func TestEditTextKeepsEverythingElse(t *testing.T) {
	got := EditText(sample, "server", "port", "11500")
	want := strings.Replace(sample, "port = 11435          # the proxy port", "port = 11500  # the proxy port", 1)
	if got != want {
		t.Errorf("replace:\n%s", got)
	}
	// New key in an existing section lands at the end of that section, before the next header.
	got = EditText(sample, "orchestrator", "hold_seconds", "60")
	i, j, k := strings.Index(got, "hold_seconds = 60"), strings.Index(got, `preset = "balanced"`), strings.Index(got, "[[orchestrator.rules]]")
	if !(j < i && i < k) || !strings.Contains(got, "# memory_budget_gb = 8") {
		t.Errorf("insert:\n%s", got)
	}
	// A commented-out key is not the key.
	got = EditText(sample, "orchestrator", "memory_budget_gb", "10")
	if !strings.Contains(got, "# memory_budget_gb = 8") || !strings.Contains(got, "memory_budget_gb = 10\n") {
		t.Errorf("commented key:\n%s", got)
	}
	// New section at the end.
	got = EditText(sample, "memory", "max_items", "100")
	if !strings.HasSuffix(got, "[memory]\nmax_items = 100\n") {
		t.Errorf("new section:\n%s", got)
	}
	// Dotted per-skill section.
	got = EditText(sample, "skills.files", "allow", `"write:~/Documents"`)
	if !strings.Contains(got, `allow = "write:~/Documents"`) || strings.Contains(got, "Downloads") {
		t.Errorf("skill table:\n%s", got)
	}
	// A # inside a string is not a comment.
	got = EditText("[a]\nk = \"x # y\"  # real\n", "a", "k", `"z"`)
	if got != "[a]\nk = \"z\"  # real\n" {
		t.Errorf("hash in string: %q", got)
	}
	if EditText("", "a", "b", "1") != "[a]\nb = 1\n" {
		t.Error("empty file")
	}
}

func TestLiteralAndSplitKey(t *testing.T) {
	c := Defaults()
	for key, raw := range map[string]string{
		"orchestrator.preset": "best", "server.port": "11500", "memory.auto_extract": "false", "orchestrator.min_swap_gain": "0.2",
		"sandbox.script_timeout": "90s", "orchestrator.memory_budget_gb": "10", "server.expose_skills": "research, files",
	} {
		if err := Set(&c, key, raw); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	for key, want := range map[string]string{
		"orchestrator.preset": `"best"`, "server.port": "11500", "memory.auto_extract": "false", "orchestrator.min_swap_gain": "0.2",
		"sandbox.script_timeout": `"1m30s"`, "orchestrator.memory_budget_gb": "10", "server.expose_skills": `["research", "files"]`,
	} {
		if got, ok := Literal(c, key); !ok || got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := Literal(c, "nope.nope"); ok {
		t.Error("unknown key")
	}
	if s, k, err := SplitKey("skills.files.allow"); err != nil || s != "skills.files" || k != "allow" {
		t.Errorf("%s %s %v", s, k, err)
	}
	if _, _, err := SplitKey("port"); err == nil {
		t.Error("bare key accepted")
	}
	auto := Defaults()
	if got, _ := Literal(auto, "orchestrator.memory_budget_gb"); got != `"auto"` {
		t.Errorf("auto = %s", got)
	}
}
