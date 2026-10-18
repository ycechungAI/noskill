package learning

import (
	"strings"
	"testing"

	"noskill/internal/skills"
)

func TestStepsRoundTrip(t *testing.T) {
	md := "---\nname: x\n---\n# X\n\n## Steps\n1. Call `list`.\n2. Answer.\n\n<!-- Learned from run 1. -->\n"
	if got := StepsOf(md); len(got) != 2 || got[0] != "Call `list`." {
		t.Fatalf("steps %q", got)
	}
	out, err := WithSteps(md, []string{"Call `list` on {folder}.", "Call `move_many`.", "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: x\n---\n# X\n\n## Steps\n1. Call `list` on {folder}.\n2. Call `move_many`.\n3. Answer.\n\n<!-- Learned from run 1. -->\n"
	if out != want {
		t.Fatalf("got\n%s", out)
	}
	if _, err := WithSteps("# no steps\n", nil); err == nil {
		t.Fatal("a file without steps was accepted")
	}
}

func TestFailureCaseAndGate(t *testing.T) {
	tr := downloadsTrace(t)
	tr.RunID = "01JABCDEFGHJKMNPQRST"
	cases, fx, data, err := AddFailureCase([]byte("- name: replay\n  input: x\n  fixtures: fixtures/replay.json\n"), tr)
	if err != nil || fx != "fixtures/failed-hjkmnpqrst.json" {
		t.Fatalf("%q %v", fx, err)
	}
	if !strings.Contains(string(cases), "name: failed-") || !strings.Contains(string(cases), "answered: true") || len(data) == 0 {
		t.Fatalf("cases:\n%s", cases)
	}
	again, fx2, _, _ := AddFailureCase(cases, tr)
	if fx2 != "" || string(again) != string(cases) {
		t.Fatal("the same run was added twice")
	}
	res := []CaseResult{{Case: "replay", Passed: true}, {Case: "variant-1", Passed: false}, {Case: "failed-1", Required: true, Passed: true}}
	if Passed(res) {
		t.Fatal("half of one variant isn't enough: 0 of 1 passed")
	}
	res[1].Passed = true
	res[2].Passed = false
	if Passed(res) {
		t.Fatal("a failed-run case must pass")
	}
	res[2].Passed = true
	if !Passed(res) {
		t.Fatal("all passed")
	}
}

func TestAccessGrowth(t *testing.T) {
	old := &skills.Skill{Meta: skills.Meta{DependsOn: []string{"files"}, Permissions: skills.Permissions{Filesystem: []string{"read:~/Downloads"}}}}
	cur := &skills.Skill{Meta: skills.Meta{DependsOn: []string{"files", "web-search"}, Permissions: skills.Permissions{Filesystem: []string{"read:~/Downloads"}, Exec: true}}}
	if g := AccessGrowth(old, old); len(g) != 0 {
		t.Fatalf("%v", g)
	}
	if g := AccessGrowth(old, cur); strings.Join(g, ", ") != "skill web-search, running programs" {
		t.Fatalf("%v", g)
	}
}

// A revision that writes in a failed run's folder or prefix is refused (block 2d: a refined
// step named another run's folder).
func TestRunValue(t *testing.T) {
	fails := []Failure{{Request: "Make a folder /tmp/sim-730451906/archive and move every file whose name starts with 2021- into it."}}
	cur := []string{"Call `plan_file_organization` with {folder}, {prefix} and {target_folder}."}
	if v := runValue([]string{"Call `plan_file_organization`(folder='/tmp/sim-730451906/archive', prefix='2021-')."}, cur, fails); v == "" {
		t.Fatal("a failed run's folder and prefix weren't caught")
	}
	if v := runValue([]string{"Call `list_dir` on {folder}.", "Call `plan_file_organization` with {folder}, {prefix} and {target_folder}."}, cur, fails); v != "" {
		t.Fatalf("clean steps flagged: %q", v)
	}
	if v := runValue([]string{"Move the .txt files with `move_many`."}, []string{"List the .txt files."}, []Failure{{Request: "Sort every .txt file in /tmp/x."}}); v != "" {
		t.Fatalf("a value the current steps already have was flagged: %q", v)
	}
}
