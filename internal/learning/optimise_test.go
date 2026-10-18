package learning

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const filesBody = `
# Files

## Steps
1. ` + "`list_dir`" + ` to see what's there.
2. Call ` + "`move_many`" + ` with a plan.

## Rules
- Never delete. Use ` + "`trash`" + ` only if asked.
- Never overwrite an existing file.
`

// fakeEval passes a task when the instructions contain its id's keyword ("dry run" for t1, …),
// so a rewrite that adds the keyword fixes that task.
func fakeEval(keywords map[string]string, steps int) func(context.Context, string, []string) ([]TaskResult, error) {
	return func(_ context.Context, body string, ids []string) ([]TaskResult, error) {
		var out []TaskResult
		for _, id := range ids {
			kw := keywords[id]
			r := TaskResult{ID: id, Steps: steps, Prompt: "do " + id, Want: "a file moved"}
			if kw == "" || strings.Contains(body, kw) {
				r.Passed = true
			} else {
				r.Why, r.Calls = "didn't move it", "1. list_dir({}) → 2 entries\n"
			}
			out = append(out, r)
		}
		return out, nil
	}
}

// replying plays the author model: its nth reply is the nth change (the last one again after that).
func replying(cs ...Change) (func(context.Context, string, json.RawMessage, int) (string, error), *[]string) {
	var prompts []string
	i := 0
	return func(_ context.Context, p string, _ json.RawMessage, _ int) (string, error) {
		prompts = append(prompts, p)
		c := cs[min(i, len(cs)-1)]
		i++
		out, _ := json.Marshal(map[string][]Change{"changes": {c}})
		return string(out), nil
	}, &prompts
}

// dryRunStep is a change that makes filesBody's step 2 say "dry run first".
var dryRunStep = Change{Heading: "Steps", Replace: "2. Call `move_many` with a plan.", With: "Call `move_many` with a plan, dry run first."}

func TestOptimiserAcceptsARewriteThatFixesTasks(t *testing.T) {
	train := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
	kw := map[string]string{"t1": "dry run first", "t2": "dry run first", "t3": "dry run first"}
	llm, prompts := replying(dryRunStep)
	o := &Optimiser{Skill: "files", Base: filesBody, Train: train, Held: []string{"h1", "h2"},
		Eval: fakeEval(kw, 3), LLM: llm, Budget: 2}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f, ok := res.Finalist()
	if !ok || !strings.Contains(f.Body, "dry run first") || res.Candidates[0].Score.Passed != 3 || f.Score.Passed != 6 {
		t.Fatalf("%+v", res)
	}
	// The reflection saw the failures, and the second reply, a change already made, was dropped.
	if !strings.Contains((*prompts)[0], `Request 1: "do t1"`) || !strings.Contains((*prompts)[0], "Why it failed: didn't move it") {
		t.Errorf("prompt:\n%s", (*prompts)[0])
	}
	if len(res.Candidates) != 3 || !strings.Contains(res.Candidates[2].Dropped, "none of its changes") {
		t.Errorf("candidates: %+v", res.Candidates)
	}
}

func TestOptimiserAsksAgainWhenARewriteNamesATask(t *testing.T) {
	train := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
	kw := map[string]string{"t1": "dry run first", "t2": "dry run first", "t3": "dry run first"}
	leaky := Change{Heading: "Steps", Replace: dryRunStep.Replace, With: "Call `move_many` to put notes in place, dry run first."}
	better := strings.Replace(filesBody, "2. Call `move_many` with a plan.", "2. Call `move_many` with a plan, dry run first.", 1)
	llm, prompts := replying(leaky, dryRunStep)
	o := &Optimiser{Skill: "files", Base: filesBody, Train: train, Eval: fakeEval(kw, 3), LLM: llm, Budget: 1,
		Values: []string{"notes"}}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := res.Finalist(); !ok || f.Body != better {
		t.Fatalf("%+v", res)
	}
	if len(*prompts) != 2 || !strings.Contains((*prompts)[1], `turned down: it names "notes"`) {
		t.Errorf("prompts: %q", *prompts)
	}
}

func TestOptimiserRejectsWhatOnlyHelpsTheTrainingTasks(t *testing.T) {
	train := []string{"t1", "t2", "t3", "t4", "t5"}
	kw := map[string]string{"t1": "dry run first", "t2": "dry run first", "h1": "check twice"}
	llm, _ := replying(Change{Heading: "Rules", With: "Use a dry run first."})
	// The held-out task passes only with the original's (absent) keyword, so both fail it... make
	// the finalist fail one the original passes by keying h2 on text the rewrite removes.
	eval := func(ctx context.Context, body string, ids []string) ([]TaskResult, error) {
		rs, _ := fakeEval(kw, 3)(ctx, body, ids)
		for i := range rs {
			if rs[i].ID == "h2" && strings.Contains(body, "dry run first") {
				rs[i].Passed = false
			}
		}
		return rs, nil
	}
	o := &Optimiser{Skill: "files", Base: filesBody, Train: train, Held: []string{"h1", "h2"}, Eval: eval, LLM: llm, Budget: 1}
	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || !strings.Contains(res.Why, "held-out") {
		t.Fatalf("%+v", res)
	}
}

func TestOptimiserNeedsAClearWin(t *testing.T) {
	train := []string{"t1", "t2", "t3", "t4", "t5"}
	kw := map[string]string{"t1": "dry run first"}
	llm, _ := replying(Change{Heading: "## Rules", With: "Do a dry run first."}) // fixes one task: not enough (needs 2)
	o := &Optimiser{Skill: "files", Base: filesBody, Train: train, Eval: fakeEval(kw, 3), LLM: llm, Budget: 1}
	res, err := o.Run(context.Background())
	if err != nil || res.Accepted || !strings.Contains(res.Why, "isn't enough better") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestOptimiserNeedsEnoughTasks(t *testing.T) {
	o := &Optimiser{Skill: "code-runner", Base: filesBody, Train: []string{"a"}}
	if _, err := o.Run(context.Background()); !errors.Is(err, ErrTooFewTasks) {
		t.Fatal(err)
	}
}

func TestGuardBody(t *testing.T) {
	ok := strings.Replace(filesBody, "to see what's there", "first", 1)
	if err := GuardBody(filesBody, ok, nil, 0); err != nil {
		t.Errorf("a fair rewrite: %v", err)
	}
	for name, cand := range map[string]string{
		"drops a rule":   strings.Replace(filesBody, "- Never delete. Use `trash` only if asked.\n", "", 1),
		"edits a rule":   strings.Replace(filesBody, "Never overwrite", "Avoid overwriting", 1),
		"too long":       filesBody + strings.Repeat("- more advice\n", 9),
		"names task":     filesBody + "Put quarterly.txt in archive.\n",
		"front-matter":   "---\nname: files\n---\n" + filesBody,
		"empty":          "  \n",
		"too long (cap)": filesBody + "- one more line\n",
	} {
		maxLines := 0
		if name == "too long (cap)" {
			maxLines = countLines(filesBody)
		}
		if err := GuardBody(filesBody, cand, []string{"quarterly.txt", "archive"}, maxLines); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestClearlyBetter(t *testing.T) {
	base := Score{Passed: 3, Total: 10, Steps: 40}
	for _, c := range []struct {
		s    Score
		want bool
	}{
		{Score{5, 10, 40}, true},  // two more passed
		{Score{4, 10, 40}, false}, // one more isn't enough
		{Score{3, 10, 34}, true},  // as many, 15% fewer steps
		{Score{3, 10, 35}, false},
		{Score{2, 10, 10}, false}, // fewer steps, fewer passed
	} {
		if got := ClearlyBetter(c.s, base); got != c.want {
			t.Errorf("%s against %s: %v", c.s, base, got)
		}
	}
	if ClearlyBetter(Score{0, 6, 0}, Score{0, 6, 6}) {
		t.Error("a run that failed at once isn't better for taking fewer steps")
	}
}

func TestApplyChanges(t *testing.T) {
	got, n := ApplyChanges(filesBody, []Change{
		dryRunStep,
		// A rule extended: the rule stays, the rest becomes its own line.
		{Heading: "Rules", Replace: "(fixed) - Never delete. Use `trash` only if asked.", With: "(fixed) - Never delete. Use `trash` only if asked. If a target folder doesn't exist, create it first."},
		// A rule rewritten: dropped.
		{Heading: "Rules", Replace: "Never overwrite an existing file.", With: "Skip a file that exists."},
	})
	want := strings.Replace(filesBody, "2. Call `move_many` with a plan.", "2. Call `move_many` with a plan, dry run first.", 1)
	want = strings.Replace(want, "only if asked.\n", "only if asked.\n- If a target folder doesn't exist, create it first.\n", 1)
	if n != 2 || got != want {
		t.Errorf("%d changes:\n%s", n, got)
	}
	// New lines go at the end of their section, numbered like it; an unknown heading adds at the end.
	got, n = ApplyChanges(filesBody, []Change{{Heading: "## Steps", With: "Report what moved."}, {Heading: "Tips", With: "- Be brief."}, {Heading: "Rules", With: ""}})
	if n != 2 || !strings.Contains(got, "2. Call `move_many` with a plan.\n3. Report what moved.\n") || !strings.HasSuffix(got, "file.\n- Be brief.\n") {
		t.Errorf("%d changes:\n%s", n, got)
	}
	if got := markFixed(filesBody); !strings.Contains(got, "(fixed) - Never delete.") || strings.Contains(got, "(fixed) 1.") {
		t.Errorf("marked:\n%s", got)
	}
}

func TestGuardBodyMatchesWholeWords(t *testing.T) {
	cand := filesBody + "- Keep the catalog tidy.\n"
	if err := GuardBody(filesBody, cand, []string{"log", "cat"}, 0); err != nil {
		t.Errorf("a part of a word isn't a task's name: %v", err)
	}
	if err := GuardBody(filesBody, filesBody+"- Sort the Log files.\n", []string{"log"}, 0); err == nil {
		t.Error("a whole word, any case, is")
	}
}

func TestParetoPickSpreadsParents(t *testing.T) {
	r := func(p ...bool) []TaskResult {
		var out []TaskResult
		for _, x := range p {
			out = append(out, TaskResult{Passed: x, Steps: 3})
		}
		return out
	}
	cs := []Candidate{{Results: r(true, false, false)}, {Results: r(false, true, true)}, {Dropped: "x"}}
	for i := range cs {
		cs[i].Score = scoreOf(cs[i].Results)
	}
	used := map[int]int{}
	if p := paretoPick(cs, used); p != 1 {
		t.Errorf("first pick %d, want the one best on most tasks", p)
	}
	used[1] = 3
	if p := paretoPick(cs, used); p != 0 {
		t.Errorf("after rewriting 1 three times, pick %d", p)
	}
}

func TestSplitAndJoinSkillMD(t *testing.T) {
	raw := "---\nname: files\ndescription: d\n---\n" + filesBody
	front, body, err := SplitSkillMD([]byte(raw))
	if err != nil || body != filesBody || !strings.HasSuffix(front, "---\n") {
		t.Fatalf("%q %q %v", front, body, err)
	}
	if string(JoinSkillMD(front, body)) != raw {
		t.Error("join isn't the inverse of split")
	}
}

func TestTunedOverlay(t *testing.T) {
	emb := []byte("---\nname: files\ndescription: d\n---\n" + filesBody)
	base := fstest.MapFS{"files/SKILL.md": {Data: emb}, "files/ref.md": {Data: []byte("ref")}, "web-search/SKILL.md": {Data: []byte("---\nname: web-search\n---\n")}}
	tu := &Tuned{Dir: t.TempDir()}
	if _, in := tu.Overlay(base); len(in) != 0 {
		t.Fatal("nothing tuned yet")
	}
	front, _, _ := SplitSkillMD(emb)
	md := JoinSkillMD(front, "\n# Files\nTuned.\n")
	n, err := tu.Save("files", md, TunedVersion{BaseHash: HashMD(emb), Note: "test"})
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, in := tu.Overlay(base); len(in) != 0 {
		t.Fatal("a candidate isn't used before it's adopted")
	}
	if err := tu.SetStatus("files", TunedActive, "adopted"); err != nil {
		t.Fatal(err)
	}
	fsys, in := tu.Overlay(base)
	got, _ := fsysRead(fsys, "files/SKILL.md")
	other, _ := fsysRead(fsys, "files/ref.md")
	if in["files"] != 1 || !strings.Contains(got, "Tuned.") || other != "ref" {
		t.Fatalf("overlay: %v %q %q", in, got, other)
	}
	// A new noskill with a changed built-in makes the tuned version stale: the embedded one is used.
	base["files/SKILL.md"] = &fstest.MapFile{Data: append(emb, []byte("- New rule.\n")...)}
	if _, in := tu.Overlay(base); len(in) != 0 || !tu.Stale("files", base["files/SKILL.md"].Data) {
		t.Fatal("stale version used")
	}
	// Versions are never overwritten: a second save is version 2, and version 1 is still readable.
	if n, _ := tu.Save("files", md, TunedVersion{BaseHash: "x"}); n != 2 {
		t.Fatal(n)
	}
	if _, err := tu.VersionMD("files", 1); err != nil {
		t.Fatal(err)
	}
}

func fsysRead(fsys fs.FS, name string) (string, error) {
	b, err := fs.ReadFile(fsys, name)
	return string(b), err
}
