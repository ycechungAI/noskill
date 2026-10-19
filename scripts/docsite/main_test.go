package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRel(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"index.html", "docs/CLI.html", "docs/CLI.html"},
		{"docs/CLI.html", "index.html", "../index.html"},
		{"docs/CLI.html", "docs/CONFIG.html", "CONFIG.html"},
		{"docs/adr/0001.html", "docs/CLI.html", "../CLI.html"},
		{"docs/adr/0001.html", "skills/files/SKILL.html", "../../skills/files/SKILL.html"},
		{"skills/files/SKILL.html", "style.css", "../../style.css"},
	} {
		if got := rel(c.from, c.to); got != c.want {
			t.Errorf("rel(%s, %s) = %s, want %s", c.from, c.to, got, c.want)
		}
	}
}

// A small tree: page links become page links, source links are marked, a missing target is a problem.
func TestBuildLinks(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		p = filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"docs/START.md", "docs/VISION.md", "docs/CLI.md", "docs/CONFIG.md", "docs/SKILL_AUTHORING.md", "docs/SKILL_FORMAT.md", "docs/LEARNING.md",
		"skills/README.md", "docs/ARCHITECTURE.md", "docs/ORCHESTRATOR.md", "docs/MODEL_ADAPTERS.md", "docs/PROXY.md", "docs/SECURITY.md", "docs/PLATFORMS.md",
		"docs/DATA_MODEL.md", "SPEC.md", "docs/ROADMAP.md", "docs/PACKAGING.md", "docs/OPEN_QUESTIONS.md", "docs/PRIOR_ART.md", "docs/PHASE0_TESTS.md"} {
		write(p, "# "+p+"\n")
	}
	for _, d := range []string{"web-search", "research", "storm", "files", "code-runner", "local-docs", "watcher", "anime-tracker", "model-manager", "orchestrator", "skill-author"} {
		write("skills/"+d+"/SKILL.md", "# "+d+"\n")
	}
	write("docs/adr/0001-x.md", "# ADR 1\n")
	write("docs/spikes/S1.md", "# S1\n")
	write("internal/app/app.go", "package app\n")
	write("README.md", "# Home\n[cli](docs/CLI.md#flags) [skills](skills/) [code](internal/app/app.go:12) [web](https://example.com) [here](#top)\n")
	write("docs/CLI.md", "# CLI\n[home](../README.md) [adr](adr/0001-x.md) [gone](nope.md)\n")

	out := t.TempDir()
	n, problems, err := build(root, out, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 21+11+3 {
		t.Errorf("%d pages", n)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "docs/CLI.md") || !strings.Contains(problems[0], "nope.md") {
		t.Errorf("problems: %v", problems)
	}
	home, _ := os.ReadFile(filepath.Join(out, "index.html"))
	for _, want := range []string{`href="docs/CLI.html#flags"`, `href="skills/README.html"`, `class="src"`, `href="https://example.com"`, `href="#top"`, `<title>noskill</title>`} {
		if !strings.Contains(string(home), want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	cli, _ := os.ReadFile(filepath.Join(out, "docs", "CLI.html"))
	if !strings.Contains(string(cli), `href="../index.html"`) || !strings.Contains(string(cli), `href="adr/0001-x.html"`) || !strings.Contains(string(cli), `aria-current="page"`) {
		t.Errorf("CLI.html:\n%s", cli)
	}
	// With a repository URL, source links go there.
	if _, _, err := build(root, out, "https://example.com/r/blob/main/"); err != nil {
		t.Fatal(err)
	}
	home, _ = os.ReadFile(filepath.Join(out, "index.html"))
	if !strings.Contains(string(home), `href="https://example.com/r/blob/main/internal/app/app.go"`) {
		t.Errorf("repo link:\n%s", home)
	}
}

// The real docs have no broken links and every page the site lists exists.
func TestRealDocsBuild(t *testing.T) {
	root := filepath.Join("..", "..")
	n, problems, err := build(root, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if n < 40 {
		t.Errorf("only %d pages", n)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
