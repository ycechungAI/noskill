package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func startTable(t *testing.T, file, open, close string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", file))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("%s has no item table", file)
	}
	s = s[i+len(open):]
	j := strings.Index(s, close)
	if j < 0 {
		t.Fatalf("%s's item table doesn't end", file)
	}
	var rows []string
	for _, l := range strings.Split(strings.ReplaceAll(s[:j], "\r", ""), "\n") {
		if strings.TrimSpace(l) != "" {
			rows = append(rows, l)
		}
	}
	return rows
}

// start.sh and start.ps1 list the same items, and the guide has a section for each (docs/START.md).
func TestStartScriptsListTheSameItems(t *testing.T) {
	sh := startTable(t, "start.sh", "<<'TABLE'\n", "\nTABLE\n")
	ps := startTable(t, "start.ps1", "$table = @'\n", "\n'@")
	if strings.Join(sh, "\n") != strings.Join(ps, "\n") {
		t.Fatalf("start.sh and start.ps1 have different item tables:\n%s\n--- vs ---\n%s", strings.Join(sh, "\n"), strings.Join(ps, "\n"))
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "START.md"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range sh {
		f := strings.Split(row, "|")
		if len(f) != 6 {
			t.Errorf("%q: want 6 fields (key|group|title|command|ask|default), got %d", row, len(f))
			continue
		}
		if seen[f[0]] {
			t.Errorf("item %s twice", f[0])
		}
		seen[f[0]] = true
		if !regexp.MustCompile(`(?m)^### ` + regexp.QuoteMeta(f[0]) + `$`).Match(guide) {
			t.Errorf("docs/START.md has no section for %s", f[0])
		}
		if strings.Contains(f[3], "{}") && f[4] == "" {
			t.Errorf("%s takes input but says nothing to ask for", f[0])
		}
	}
}

func TestStartShListsAndExplains(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("start.sh is for macOS and Linux; start.cmd is the Windows menu")
	}
	run := func(args ...string) string {
		cmd := exec.Command("sh", append([]string{filepath.Join("..", "..", "start.sh")}, args...)...)
		cmd.Env = append(os.Environ(), "NOSKILL_BIN=/nonexistent/noskill")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("start.sh %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := run("--list"); !strings.Contains(out, "research") || !strings.Contains(out, "anime-add") {
		t.Errorf("--list:\n%s", out)
	}
	if out := run("--help"); !strings.Contains(out, "./start.sh <name>") || strings.Contains(out, "set -eu") {
		t.Errorf("--help:\n%s", out)
	}
	// An installed noskill that doesn't know the command falls back to a source build; -n shows it without building.
	if out := run("-n", "research", "heat pumps"); !strings.Contains(out, "'run' '--skill' 'storm' 'heat pumps'") {
		t.Errorf("research:\n%s", out)
	}
}
