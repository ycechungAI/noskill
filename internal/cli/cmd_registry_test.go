package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Make a key, publish a folder, add it as a registry, search it and install from it (FR-S10).
func TestRegistryCommands(t *testing.T) {
	h := newHarness(t)
	if code, _, errs := h.run("", false, "registry", "key", "show"); code == 0 || !strings.Contains(errs, "registry key new") {
		t.Errorf("no key: %d %s", code, errs)
	}
	out, _ := h.ok("", false, "registry", "key", "new")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	key := strings.TrimSpace(lines[len(lines)-1])
	if len(key) != 44 {
		t.Fatalf("key new:\n%s", out)
	}
	if out, _ := h.ok("", false, "registry", "key", "show"); !strings.Contains(out, key) {
		t.Errorf("show:\n%s", out)
	}

	reg := t.TempDir()
	src := filepath.Join(reg, "pdf-tools")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: pdf-tools\ndescription: Split and merge PDF files.\nnoskill:\n  version: 1.0.0\n  permissions: { network: ['example.com'] }\n---\nSteps.\n"), 0o644)
	if out, _ := h.ok("", false, "registry", "publish", reg, "--name", "team"); !strings.Contains(out, "Signed 1 skills") {
		t.Errorf("publish:\n%s", out)
	}
	if code, _, _ := h.run("", false, "registry", "add", "team", reg); code != ExitUsage {
		t.Error("add without --key")
	}
	h.ok("", false, "registry", "add", "team", reg, "--key", key)
	if out, _ := h.ok("", false, "registry", "list"); !strings.Contains(out, "team") || !strings.Contains(out, reg) {
		t.Errorf("list:\n%s", out)
	}
	if out, _ := h.ok("", false, "registry", "search", "pdf"); !strings.Contains(out, "team/pdf-tools") || !strings.Contains(out, "network: example.com") {
		t.Errorf("search:\n%s", out)
	}
	if out, _ := h.ok("", false, "registry", "update"); !strings.Contains(out, "✓ team: 1 skills") {
		t.Errorf("update:\n%s", out)
	}

	_, errs := h.ok("y\n", true, "skills", "install", "team/pdf-tools")
	if !strings.Contains(errs, "signed by registry team") {
		t.Errorf("install:\n%s", errs)
	}
	if out, _ := h.ok("", false, "skills", "verify", "pdf-tools"); !strings.Contains(out, "matches its signature (signed by you") {
		t.Errorf("verify:\n%s", out)
	}

	// A plain install of a folder that changed after signing is refused (before the already-installed check).
	os.WriteFile(filepath.Join(src, "notes.txt"), []byte("x"), 0o644)
	if code, _, errs := h.run("y\n", true, "skills", "install", src); code == 0 || !strings.Contains(errs, "notes.txt") {
		t.Errorf("tampered: %d %s", code, errs)
	}
	h.ok("", false, "registry", "remove", "team")
}

// `doctor --files` does a real move, copy, trash and undo (here in the harness's own trash folder)
// and `anime audit` needs the job it checks.
func TestDoctorFilesAndAnimeAudit(t *testing.T) {
	h := newHarness(t)
	out, _ := h.ok("", false, "doctor", "--files")
	for _, want := range []string{"files: move", "files: copy", "files: trash", "files: undo", "files: after undo"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor --files lacks %q:\n%s", want, out)
		}
	}
	if code, _, errs := h.run("", false, "anime", "audit"); code == 0 || !strings.Contains(errs, "anime-nightly") {
		t.Errorf("audit without the job: %d %s", code, errs)
	}
	if code, _, _ := h.run("", false, "anime", "audit", "--since", "soon"); code != ExitUsage {
		t.Errorf("a bad --since: %d", code)
	}
}
