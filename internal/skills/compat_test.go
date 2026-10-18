package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mk(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(p)
}

const claudeSkill = `---
name: pdf-forms
description: Fill in PDF forms. Use when the user has a form to complete.
allowed-tools: Bash(python:*), Read
model: claude-opus-4
context: fork
disable-model-invocation: true
argument-hint: "[file]"
---

# PDF forms

Fill the form named in $ARGUMENTS.

Current branch: !` + "`git branch --show-current`" + `

Run ${CLAUDE_SKILL_DIR}/scripts/fill.py.
`

// FR-S8: a skill written for Claude Code loads here unchanged, and the report says what won't work.
func TestAnalyzeClaudeCodeSkill(t *testing.T) {
	root := t.TempDir()
	dir := mk(t, root, "pdf-forms/SKILL.md", claudeSkill)
	mk(t, root, "pdf-forms/scripts/fill.py", "print('hi')\n")
	c := Analyze(dir)
	if !c.Valid {
		t.Fatalf("%+v", c)
	}
	all := strings.Join(c.Notices, "\n")
	for _, want := range []string{"no noskill: block", "allowed-tools (Bash(python:*), Read)", "model claude-opus-4", "sub-agent", "started by name only",
		"$ARGUMENTS", "!`command`", "CLAUDE_SKILL_DIR", "scripts/fill.py"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing a notice about %q:\n%s", want, all)
		}
	}
	if len(c.Scripts) != 1 || c.Scripts[0] != "scripts/fill.py" {
		t.Errorf("scripts %v", c.Scripts)
	}
	// Nothing was changed.
	if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(b) != claudeSkill {
		t.Error("the analysis rewrote the skill")
	}
}

func TestAnalyzeCleanAndBrokenSkills(t *testing.T) {
	root := t.TempDir()
	plain := mk(t, root, "notes/SKILL.md", "---\nname: notes\ndescription: Keep tidy notes.\n---\n\nWrite short notes.\n")
	if c := Analyze(plain); !c.Valid || len(c.Notices) != 1 || !strings.Contains(c.Notices[0], "no noskill: block") {
		t.Errorf("plain: %+v", c)
	}
	withBlock := mk(t, root, "ok/SKILL.md", "---\nname: ok\ndescription: Fine.\nnoskill:\n  version: 0.1.0\n  permissions: { network: [] }\n---\n\nBody.\n")
	if c := Analyze(withBlock); !c.Valid || len(c.Notices) != 0 {
		t.Errorf("with block: %+v", c)
	}
	broken := mk(t, root, "bad/SKILL.md", "no front matter here\n")
	if c := Analyze(broken); c.Valid || c.Problem == "" {
		t.Errorf("broken: %+v", c)
	}
	noDesc := mk(t, root, "nodesc/SKILL.md", "---\nname: nodesc\n---\n\nBody.\n")
	if c := Analyze(noDesc); c.Valid {
		t.Errorf("a skill without a description loaded: %+v", c)
	}
}

func TestDiscoverFindsSkillsInEachRuntimesFolders(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	mk(t, home, ".claude/skills/pdf-forms/SKILL.md", claudeSkill)
	mk(t, home, ".codex/skills/review/SKILL.md", "---\nname: review\ndescription: Review a change.\n---\n\nLook for bugs.\n")
	mk(t, home, ".codex/skills/.system/hidden/SKILL.md", "---\nname: hidden\ndescription: Built into Codex.\n---\n\nx\n")
	mk(t, home, ".hermes/skills/research/web-notes/SKILL.md", "---\nname: web-notes\ndescription: Take notes from pages.\n---\n\nx\n")
	mk(t, home, ".claude/plugins/marketplaces/acme/plugin/skills/deploy/SKILL.md", "---\nname: deploy\ndescription: Deploy the app.\n---\n\nx\n")
	mk(t, cwd, ".claude/skills/local-only/SKILL.md", "---\nname: local-only\ndescription: Only in this project.\n---\n\nx\n")
	mk(t, home, ".agents/skills/shared/SKILL.md", "---\nname: shared\ndescription: Works in any agent.\n---\n\nx\n")

	got := Discover(home, cwd, func(string) string { return "" })
	by := map[string]string{}
	for _, c := range got {
		by[c.Name] = c.Runtime
		if !c.Compat.Valid {
			t.Errorf("%s invalid: %+v", c.Name, c.Compat)
		}
	}
	want := map[string]string{"pdf-forms": "claude", "review": "codex", "web-notes": "hermes", "deploy": "claude", "local-only": "claude", "shared": "agents"}
	for n, rt := range want {
		if by[n] != rt {
			t.Errorf("%s: found under %q, want %q (all: %v)", n, by[n], rt, by)
		}
	}
	if _, ok := by["hidden"]; ok {
		t.Error("a hidden folder was scanned")
	}
	// CODEX_HOME / HERMES_HOME move those roots.
	other := t.TempDir()
	mk(t, other, "skills/elsewhere/SKILL.md", "---\nname: elsewhere\ndescription: In CODEX_HOME.\n---\n\nx\n")
	got = Discover(t.TempDir(), "", func(k string) string {
		if k == "CODEX_HOME" {
			return other
		}
		return ""
	})
	if len(got) != 1 || got[0].Name != "elsewhere" || got[0].Runtime != "codex" {
		t.Errorf("CODEX_HOME: %+v", got)
	}
}
