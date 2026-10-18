package cli

import (
	"strings"
	"testing"
	"time"
)

func TestTrainCommands(t *testing.T) {
	h := newHarness(t)
	if out, _ := h.ok("", false, "train", "list"); !strings.Contains(out, "train export") {
		t.Errorf("list:\n%s", out)
	}
	if out, _ := h.ok("", false, "train", "export", "--since", "7d"); !strings.Contains(out, "No run to train on") {
		t.Errorf("export:\n%s", out)
	}
	if code, _, errs := h.run("", false, "train", "lora", "--data", "../x", "--base", "qwen3:4b"); code == 0 || !strings.Contains(errs, `no dataset "../x"`) {
		t.Errorf("a path as a dataset: %d %s", code, errs)
	}
	if code, _, _ := h.run("", false, "train", "adopt", "x"); code == 0 {
		t.Error("adopt x")
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "72h": 72 * time.Hour} {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "d", "-3d", "soon"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
