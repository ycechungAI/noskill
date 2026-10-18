package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/eval"
	"noskill/internal/learning"
)

const tunedLine = "- Mention the release version in the answer."

// fakeMode steers optimiseFake: broken fails every release question, easy passes them all.
type fakeMode struct{ broken, easy bool }

// optimiseFake answers release questions right only when the files skill's instructions carry
// tunedLine, and plays the author model: its one change adds that line.
func optimiseFake(m *fakeMode) *backendtest.Fake {
	return &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		all := ""
		for _, m := range r.Messages {
			all += m.Content + "\n"
		}
		last := r.Messages[len(r.Messages)-1].Content
		switch {
		case strings.Contains(last, "An assistant follows these instructions"):
			out, _ := json.Marshal(map[string]any{"changes": []map[string]string{{"heading": "", "replace": "", "with": tunedLine}}})
			return backends.Response{Content: string(out)}, nil
		case strings.Contains(last, "latest release"):
			if (strings.Contains(all, tunedLine) || m.easy) && !m.broken {
				return backends.Response{Content: "It's v2.1."}, nil
			}
			return backends.Response{Content: "No idea."}, nil
		}
		return backends.Response{Content: `{"task_type":"chat"}`}, nil
	}}
}

func releaseTasks(prefix string, n int) []eval.Task {
	var out []eval.Task
	for i := 1; i <= n; i++ {
		out = append(out, eval.Task{ID: fmt.Sprintf("%s-%d", prefix, i), Type: "files", Skills: []string{"files"},
			Prompt: fmt.Sprintf("What is the latest release of tool %d?", i), Check: eval.Check{Contains: []string{"v2.1"}}})
	}
	return out
}

// A built-in skill's rewrite that's clearly better waits as a tuned candidate, is used once
// adopted, and the weekly gate puts the built-in back when a task naming it falls (FR-L9, ADR-0014).
func TestOptimiseTunesABuiltinSkill(t *testing.T) {
	m := &fakeMode{}
	e := newApp(t, optimiseFake(m), noRules, "")
	e.app.powerOverride = func() (bool, bool) { return true, true }
	ctx := context.Background()
	train, held := releaseTasks("tr", 6), releaseTasks("ho", 2)
	var lines []string
	out, err := e.app.OptimiseSkill(ctx, OptimiseOptions{Skill: "files", Budget: 1, Model: "qwen3:4b",
		tasks: func() ([]eval.Task, []eval.Task) { return train, held }, Progress: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Result
	if !r.Accepted || r.Candidates[0].Score.Passed != 0 || r.Candidates[1].Score.Passed != 6 || r.BestHeld.Passed != 2 {
		t.Fatalf("%+v\n%s", r, strings.Join(lines, "\n"))
	}
	if !strings.Contains(out.Saved, "tuned version 1") {
		t.Errorf("saved: %q", out.Saved)
	}
	// Not used before it's adopted.
	if sk, _ := e.app.Skills.Get("files"); strings.Contains(sk.Body, tunedLine) || e.app.TunedVersion("files") != 0 {
		t.Fatal("a candidate is in use")
	}
	if n, err := e.app.OptimiseAdopt(ctx, "files"); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	sk, _ := e.app.Skills.Get("files")
	if !strings.Contains(sk.Body, tunedLine) || e.app.TunedVersion("files") != 1 || sk.Source != "builtin" {
		t.Fatalf("adopted version not loaded: %v %q", e.app.TunedVersion("files"), sk.Body)
	}
	// Its permissions and tools are the built-in's: only the instructions changed.
	emb, _ := embeddedSkillMD("files")
	front, _, _ := learning.SplitSkillMD(emb)
	cur, _ := e.app.skillMD(sk)
	if !strings.HasPrefix(string(cur), front) {
		t.Error("the front-matter changed")
	}
	st, err := e.app.OptimiseStatus("files")
	if err != nil || st.InUse != 1 || !strings.Contains(st.Diff, "+"+tunedLine) {
		t.Fatalf("%+v %v", st, err)
	}

	// The gate: a good run with the tuned version, then the tasks fall → back to the built-in.
	opts := EvalOptions{Model: "qwen3:4b", Tasks: train, Suite: "rel"}
	// The good run used the embedded instructions; the tasks fall once the tuned version is in
	// use, so the gate sends it back.
	if err := e.app.OptimiseRevert(ctx, "files", 0, "test"); err != nil {
		t.Fatal(err)
	}
	m.easy = true
	if _, err := e.app.Eval(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.OptimiseAdopt(ctx, "files"); err != nil {
		t.Fatal(err)
	}
	m.easy, m.broken = false, true
	g, err := e.app.EvalGate(ctx, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if g.RolledBack["files"] != "built-in instructions" || e.app.TunedVersion("files") != 0 {
		t.Fatalf("rolled back %v, in use %d; regressions %v; prev %q now %q", g.RolledBack, e.app.TunedVersion("files"), g.Outcome.Regressions, g.Outcome.Prev.SkillState, g.Outcome.Run.SkillState)
	}
}

func TestOptimiseRefusesUserSkillsAndTooFewTasks(t *testing.T) {
	m := &fakeMode{}
	e := newApp(t, optimiseFake(m), noRules, "")
	e.app.powerOverride = func() (bool, bool) { return true, true }
	ctx := context.Background()
	_, err := e.app.OptimiseSkill(ctx, OptimiseOptions{Skill: "files", Model: "qwen3:4b",
		tasks: func() ([]eval.Task, []eval.Task) { return releaseTasks("tr", 2), nil }})
	if err == nil || !strings.Contains(err.Error(), "needs 5") {
		t.Errorf("too few tasks: %v", err)
	}
	if _, err := e.app.OptimiseSkill(ctx, OptimiseOptions{Skill: "nope"}); err == nil {
		t.Error("unknown skill")
	}
	e.app.powerOverride = func() (bool, bool) { return false, true }
	if _, err := e.app.OptimiseSkill(ctx, OptimiseOptions{Skill: "files", Model: "qwen3:4b",
		tasks: func() ([]eval.Task, []eval.Task) { return releaseTasks("tr", 6), nil }}); err == nil || !strings.Contains(err.Error(), "battery") {
		t.Errorf("on battery: %v", err)
	}
	// The real split: files has training tasks from the agent and workload sets, and the
	// multi-step tasks are held out.
	train, held, err := e.app.OptimiseTasks("files")
	if err != nil || len(train) < 20 || len(held) < 10 {
		t.Fatalf("split: %d train, %d held, %v", len(train), len(held), err)
	}
	for _, h := range held {
		if !strings.HasPrefix(h.ID, "multi-") {
			t.Errorf("held out: %s", h.ID)
		}
	}
	for _, tr := range train {
		if strings.HasPrefix(tr.ID, "multi-") {
			t.Errorf("trained on %s", tr.ID)
		}
	}
}

func TestSampleTasks(t *testing.T) {
	var ts []eval.Task
	for i := 0; i < 28; i++ {
		ts = append(ts, eval.Task{ID: fmt.Sprintf("t%02d", i)})
	}
	got := sampleTasks(ts, 7)
	if len(got) != 7 || got[0].ID != "t00" || got[1].ID != "t04" || got[6].ID != "t24" {
		t.Errorf("%v", got)
	}
	if again := sampleTasks(ts, 7); again[3].ID != got[3].ID {
		t.Error("not the same tasks every time")
	}
	if len(sampleTasks(ts, 40)) != 28 || len(sampleTasks(nil, 5)) != 0 {
		t.Error("a larger n must keep all")
	}
}

func TestOptimiseTasksLimit(t *testing.T) {
	e := newApp(t, trainFake(new(bool)), nil, "")
	if _, err := e.app.OptimiseSkill(context.Background(), OptimiseOptions{Skill: "files", MaxTasks: 3, DryRun: true}); err == nil || !strings.Contains(err.Error(), "at least 5") {
		t.Errorf("--tasks 3: %v", err)
	}
}

// Every search leaves a report with its rewrites and scores, a dry run included; --adopt puts a
// built-in's accepted rewrite straight into use; --tasks cuts the task lists (FR-L9).
func TestOptimiseReportAdoptAndTaskLimit(t *testing.T) {
	ctx := context.Background()
	opts := func(o OptimiseOptions, train, held int) OptimiseOptions {
		o.Skill, o.Budget, o.Model = "files", 1, "qwen3:4b"
		o.tasks = func() ([]eval.Task, []eval.Task) { return releaseTasks("tr", train), releaseTasks("ho", held) }
		return o
	}

	// A dry run saves no version, but its winner is in the report.
	e := newApp(t, optimiseFake(&fakeMode{}), noRules, "")
	e.app.powerOverride = func() (bool, bool) { return true, true }
	out, err := e.app.OptimiseSkill(ctx, opts(OptimiseOptions{DryRun: true}, 6, 2))
	if err != nil {
		t.Fatal(err)
	}
	if e.app.TunedVersion("files") != 0 || out.Saved != "" {
		t.Errorf("a dry run saved %q", out.Saved)
	}
	var rep OptimiseOutcome
	b, err := os.ReadFile(out.Report)
	if err != nil || out.Report == "" || json.Unmarshal(b, &rep) != nil {
		t.Fatalf("report %q: %v", out.Report, err)
	}
	if f, ok := rep.Result.Finalist(); !ok || !strings.Contains(f.Body, tunedLine) || f.Score.Passed != 6 || rep.Result.Candidates[0].Score.Passed != 0 {
		t.Errorf("the winning rewrite isn't in the report: %s", b)
	}

	// --adopt needs a real run.
	if _, err := e.app.OptimiseSkill(ctx, opts(OptimiseOptions{DryRun: true, Adopt: true}, 6, 2)); err == nil || !strings.Contains(err.Error(), "--adopt") {
		t.Errorf("--adopt with --dry-run: %v", err)
	}

	// A partial task list can't adopt.
	if _, err := e.app.OptimiseSkill(ctx, opts(OptimiseOptions{Adopt: true, MaxTasks: 6}, 12, 6)); err == nil || !strings.Contains(err.Error(), "too noisy") {
		t.Errorf("--adopt with --tasks: %v", err)
	}

	// A real run with --adopt: in use at once, and `revert` goes back.
	out, err = e.app.OptimiseSkill(ctx, opts(OptimiseOptions{Adopt: true}, 6, 2))
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := e.app.Skills.Get("files")
	if e.app.TunedVersion("files") != 1 || !strings.Contains(sk.Body, tunedLine) || !strings.Contains(out.Saved, "now in use") {
		t.Fatalf("not adopted: v%d %q", e.app.TunedVersion("files"), out.Saved)
	}
	if err := e.app.OptimiseRevert(ctx, "files", 0, "test"); err != nil || e.app.TunedVersion("files") != 0 {
		t.Fatalf("revert: %v", err)
	}

	// A rewrite that isn't better saves nothing, even with --adopt; the report is still written.
	e2 := newApp(t, optimiseFake(&fakeMode{broken: true}), noRules, "")
	e2.app.powerOverride = func() (bool, bool) { return true, true }
	out, err = e2.app.OptimiseSkill(ctx, opts(OptimiseOptions{Adopt: true}, 6, 2))
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Accepted || e2.app.TunedVersion("files") != 0 || out.Saved != "" || out.Report == "" {
		t.Errorf("accepted %v, saved %q, report %q", out.Result.Accepted, out.Saved, out.Report)
	}

	// --tasks cuts the lists: 12 training tasks to 6, and 6 held-out to 2.
	out, err = e2.app.OptimiseSkill(ctx, opts(OptimiseOptions{DryRun: true, MaxTasks: 6}, 12, 6))
	if err != nil || len(out.Train) != 6 || len(out.Held) != 2 {
		t.Errorf("%v: %d train, %d held", err, len(out.Train), len(out.Held))
	}
}

// A skill that declares a budget gets a longer run than the default 25 steps; --budget-steps from
// the user still wins, and no skill can ask for more than the cap.
func TestSkillBudgetLengthensTheRun(t *testing.T) {
	calls := 0
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		if r.Format != nil {
			return backends.Response{Content: "{}"}, nil
		}
		n := 0
		for _, m := range r.Messages {
			if m.Role == backends.RoleTool {
				n++
			}
		}
		if n < 30 {
			calls = max(calls, n+1) // the most tool calls the model got to make
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: "notes", Args: json.RawMessage(fmt.Sprintf(`{"action":"append","text":"fact %d"}`, n))}}}, nil
		}
		return backends.Response{Content: "all done"}, nil
	}}
	run := func(steps int, budget string) *RunResult {
		e := newApp(t, fake, noRules, "")
		dir := filepath.Join(e.home, "skills", "longrun")
		os.MkdirAll(dir, 0o700)
		md := "---\nname: longrun\ndescription: A very long chore with many small steps, for the budget test only.\nnoskill:\n  version: 1.0.0\n  triggers: [many small steps]\n" +
			budget + "  tools:\n    - builtin: notes\n---\n## Steps\n1. Call `notes` for every fact.\n"
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600)
		if err := e.app.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
		calls = 0
		res, err := e.app.Run(context.Background(), RunRequest{Messages: user("do the long chore with many small steps"), Skills: []string{"longrun"}, MaxSteps: steps})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(0, ""); res.Answer == "all done" || calls != 26 { // the 26th ask is the one the budget refuses
		t.Errorf("without a budget: answer %q after %d asks (want the default 25-step stop)", res.Answer, calls)
	}
	if res := run(0, "  budget: { steps: 60 }\n"); res.Answer != "all done" || calls != 30 {
		t.Errorf("with budget.steps 60: answer %q after %d calls", res.Answer, calls)
	}
	if res := run(10, "  budget: { steps: 60 }\n"); res.Answer == "all done" || calls != 11 {
		t.Errorf("--budget-steps must win: answer %q after %d asks", res.Answer, calls)
	}
	if res := run(0, "  budget: { steps: 5000 }\n"); res.Answer != "all done" {
		t.Errorf("an over-large ask should be capped at %d, not ignored: %q", 200, res.Answer)
	}
}
