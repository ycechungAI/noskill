package eval

import (
	"fmt"
	"noskill/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltinTasksLoad(t *testing.T) {
	ts, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) < 60 {
		t.Errorf("%d tasks, want about 60", len(ts))
	}
	types := map[string]int{}
	bench := 0
	for _, x := range ts {
		types[x.Type]++
		if x.Bench {
			bench++
			if x.Agent() {
				t.Errorf("%s: bench tasks are plain", x.ID)
			}
		}
		if _, err := x.Check.compile(); err != nil {
			t.Errorf("%s: %v", x.ID, err)
		}
	}
	if bench < 10 || bench > 16 {
		t.Errorf("%d bench tasks", bench)
	}
	for _, ty := range []string{"chat", "reasoning", "math", "code", "summarise", "extract", "translate", "creative", "plan", "files"} {
		if types[ty] == 0 {
			t.Errorf("no %s tasks", ty)
		}
	}
}

func TestChecks(t *testing.T) {
	n := 391.0
	for _, c := range []struct {
		name   string
		check  Check
		answer string
		want   bool
	}{
		{"number last", Check{Number: &n}, "17 * 23 = 391", true},
		{"number commas", Check{Number: &n}, "It's 391.", true},
		{"number wrong", Check{Number: &n}, "The answer is 392", false},
		{"contains", Check{Contains: []string{"paris"}}, "Paris.", true},
		{"not", Check{Contains: []string{"carol"}, Not: []string{"alice"}}, "Carol, not Alice", false},
		{"json fenced", Check{JSON: map[string]any{"name": "Maria", "age": 34}}, "```json\n{\"name\": \"Maria Lopez\", \"age\": 34}\n```", true},
		{"json age string", Check{JSON: map[string]any{"age": 34}}, `{"age": "34"}`, true},
		{"json array hash", Check{JSON: map[string]any{"tags": []any{"beach", "summer2027"}}}, `{"tags": ["#beach", "#summer2027"]}`, true},
		{"json missing", Check{JSON: map[string]any{"city": "Berlin"}}, `{"town": "Berlin"}`, false},
		{"words", Check{MaxWords: 3}, "one two three four", false},
		{"lines", Check{Lines: 3}, "a\n\nb\nc\n", true},
		{"regex", Check{Regex: []string{`(?i)write code\W+test\W+deploy`}}, "Write code, test, deploy", true},
		{"empty", Check{Answered: true}, "  ", false},
	} {
		if ok, why := c.check.Verdict(c.answer); ok != c.want {
			t.Errorf("%s: %v (%s)", c.name, ok, why)
		}
	}
}

func TestEndState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("Hello noskill"), 0o644)
	if ok, why := (End{Exists: []string{"a.txt"}, Absent: []string{"b.txt"}, FileHas: map[string]string{"a.txt": "hello"}, FileLacks: map[string]string{"a.txt": "bye"}}).Verdict(dir); !ok {
		t.Error(why)
	}
	if ok, _ := (End{Exists: []string{"b.txt"}}).Verdict(dir); ok {
		t.Error("b.txt doesn't exist")
	}
}

func TestPinnedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SavePinned(dir, Task{ID: "mine-1", Type: "chat", Prompt: "hi", Check: Check{Answered: true}}); err != nil {
		t.Fatal(err)
	}
	if err := SavePinned(dir, Task{ID: "mine-1", Type: "chat", Prompt: "hi"}); err == nil {
		t.Error("duplicate id accepted")
	}
	ts, err := Pinned(dir)
	if err != nil || len(ts) != 1 || ts[0].Source != "pinned" || !ts[0].Check.Answered {
		t.Fatalf("%+v %v", ts, err)
	}
}

func TestCompareFindsRegressions(t *testing.T) {
	mk := func(pass ...bool) []store.EvalResult {
		var out []store.EvalResult
		for i, p := range pass {
			out = append(out, store.EvalResult{TaskID: fmt.Sprintf("math-%02d", i), Type: "math", Passed: p})
		}
		out = append(out, store.EvalResult{TaskID: "chat-01", Type: "chat", Passed: true})
		return out
	}
	if r := Compare(mk(true, true, true, true), mk(true, true, true, false)); len(r) != 0 {
		t.Errorf("one task is noise: %+v", r)
	}
	r := Compare(mk(true, true, true, true), mk(true, true, false, false))
	if len(r) != 1 || r[0].Type != "math" || len(r[0].Flipped) != 2 {
		t.Errorf("%+v", r)
	}
	sc := Scores(mk(true, true))
	if sc["math"] != 2.5/3 || sc["chat"] != 0.75 {
		t.Errorf("%v", sc)
	}
}

// The simulation's training requests and the held-out multi-step tasks cover the same families
// and never share a prompt or a file set, so a learned skill isn't graded on its own source.
func TestWorkloadIsHeldOutFromTheSuite(t *testing.T) {
	work, err := Workload()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := Builtin()
	held := map[string]int{}
	prompts := map[string]bool{}
	files := map[string]bool{}
	for _, x := range all {
		if x.Type != "multi" {
			continue
		}
		if x.Family == "" || !x.Agent() {
			t.Errorf("%s: multi tasks need a family and skills", x.ID)
		}
		held[x.Family]++
		prompts[x.Prompt] = true
		files[fmt.Sprint(x.Files)] = true
	}
	fams := map[string]int{}
	for _, w := range work {
		fams[w.Family]++
		if held[w.Family] == 0 {
			t.Errorf("%s: family %q has no held-out task", w.ID, w.Family)
		}
		if files[fmt.Sprint(w.Files)] {
			t.Errorf("%s has the same files as a held-out task", w.ID)
		}
		if prompts[w.Prompt] && len(w.Files) == 0 {
			t.Errorf("%s repeats a held-out prompt with no files of its own", w.ID)
		}
	}
	for f := range held {
		if fams[f] < 3 {
			t.Errorf("family %s has %d training requests, want 3+", f, fams[f])
		}
	}
}

// A learned script tool's ops count for the built-in tool that would make the same change.
func TestToolsMet(t *testing.T) {
	task := Task{Tools: []string{"copy_verify"}}
	for _, c := range []struct {
		called, learned, effects []string
		want                     bool
	}{
		{[]string{"list_dir", "copy_verify"}, nil, []string{"copy"}, true},
		{[]string{"list_dir", "csv_backup_tool"}, []string{"csv_backup_tool"}, []string{"mkdir", "copy"}, true},
		{[]string{"list_dir", "csv_backup_tool"}, []string{"csv_backup_tool"}, []string{"mkdir"}, false},
		{[]string{"list_dir", "make_dir"}, nil, []string{"mkdir", "copy"}, false}, // built-ins only: copy_verify itself
		{[]string{"list_dir"}, nil, nil, false},
	} {
		if got := task.ToolsMet(c.called, c.learned, c.effects); got != c.want {
			t.Errorf("%v %v: got %v", c.called, c.effects, got)
		}
	}
	if !(Task{}).ToolsMet(nil, nil, nil) {
		t.Error("no tools asked for")
	}
}
