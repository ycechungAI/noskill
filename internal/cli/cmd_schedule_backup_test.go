package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// FR-J6 from the command line: back up, restore on a fresh home, restore again from a file path.
func TestScheduleBackupRestore(t *testing.T) {
	h := newHarness(t)
	h.ok("", false, "schedule", "add", "brief", "--cron", "0 7 * * *")
	h.ok("", false, "schedule", "add", "diskwatch", "--cron", "30 6 * * *", "--paused")
	out, _ := h.ok("", false, "schedule", "backup")
	if !strings.Contains(out, "backed up 2 jobs") {
		t.Fatalf("%s", out)
	}
	if out, _ = h.ok("", false, "schedule", "backup"); !strings.Contains(out, "unchanged") {
		t.Errorf("%s", out)
	}
	file := filepath.Join(t.TempDir(), "jobs.json")
	h.ok("", false, "schedule", "backup", "--to", file)
	out, _ = h.ok("", false, "schedule", "backups")
	if !strings.Contains(out, "jobs-") {
		t.Errorf("%s", out)
	}

	h2 := newHarness(t)
	out, _ = h2.ok("", false, "schedule", "restore", file)
	if !strings.Contains(out, "restored brief") || !strings.Contains(out, "restored diskwatch") {
		t.Fatalf("%s", out)
	}
	if out, _ = h2.ok("", false, "schedule", "list"); !strings.Contains(out, "paused") {
		t.Errorf("diskwatch should stay paused: %s", out)
	}
	if out, _ = h2.ok("", false, "schedule", "restore", file); !strings.Contains(out, "skipped brief: a job with this name already exists") {
		t.Errorf("%s", out)
	}
	if code, _, errs := h2.run("", false, "schedule", "restore", "no-such-backup.json"); code == 0 || !strings.Contains(errs, "no backup named") {
		t.Errorf("%d %s", code, errs)
	}
}
