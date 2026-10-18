package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"noskill/internal/agent"
	"noskill/internal/skills"
)

// pythonRun stands in for the sandbox in tests: the inputs are linked into in/ and the code runs
// with plain python3.
func pythonRun(t *testing.T) ScriptRun {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	return func(ctx context.Context, code string, stdin []byte, inputs map[string]string) ([]byte, error) {
		work := t.TempDir()
		os.Mkdir(filepath.Join(work, "in"), 0o700)
		for n, p := range inputs {
			if err := os.Symlink(p, filepath.Join(work, "in", n)); err != nil {
				return nil, err
			}
		}
		os.WriteFile(filepath.Join(work, "tool.py"), []byte(code), 0o600)
		cmd := exec.CommandContext(ctx, py, "tool.py")
		cmd.Dir, cmd.Stdin = work, strings.NewReader(string(stdin))
		out, err := cmd.Output()
		if ee, ok := err.(*exec.ExitError); ok {
			return out, &scriptErr{string(ee.Stderr)}
		}
		return out, err
	}
}

type scriptErr struct{ s string }

func (e *scriptErr) Error() string { return e.s }

func archiveTrace(t *testing.T) *Trace {
	const d = "/data/sim-1"
	list := `{"path":"` + d + `","entries":[{"name":"2021-jan.txt","type":"file"},{"name":"2021-feb.txt","type":"file"},{"name":"plan.txt","type":"file"}],"more":false}`
	p := writeTrace(t,
		agent.Event{Kind: agent.EventRunStart, V: 1, Content: "Make a folder " + d + "/archive and move every file whose name starts with 2021- into it.", Model: "qwen3.5:4b", Skills: []string{"files"}},
		agent.Event{Kind: agent.EventToolCall, Step: 1, Tool: "list_dir", Skill: "files", Args: json.RawMessage(`{"path":"` + d + `"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 1, Tool: "list_dir", Content: list},
		agent.Event{Kind: agent.EventToolCall, Step: 2, Tool: "make_dir", Skill: "files", Args: json.RawMessage(`{"path":"` + d + `/archive"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 2, Tool: "make_dir", Content: `{"folder":"` + d + `/archive"}`},
		agent.Event{Kind: agent.EventToolCall, Step: 3, Tool: "move_many", Skill: "files", Args: json.RawMessage(`{"dry_run":false,"plan":[{"from":"` + d + `/2021-jan.txt","to":"` + d + `/archive/2021-jan.txt"},{"from":"` + d + `/2021-feb.txt","to":"` + d + `/archive/2021-feb.txt"}]}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 3, Tool: "move_many", Content: `{"dry_run":false}`},
		agent.Event{Kind: agent.EventAnswer, Step: 4, Content: "Moved 2 files."},
		agent.Event{Kind: agent.EventEnd, Step: 4, Note: "answered"},
		agent.Event{Kind: agent.EventFileEffect, Op: "mkdir", To: d + "/archive"},
		agent.Event{Kind: agent.EventFileEffect, Op: "move", From: d + "/2021-jan.txt", To: d + "/archive/2021-jan.txt"},
		agent.Event{Kind: agent.EventFileEffect, Op: "move", From: d + "/2021-feb.txt", To: d + "/archive/2021-feb.txt"},
		agent.Event{Kind: agent.EventEnd, Note: "answered"},
	)
	tr, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// scriptReply is a pass (d) reply in its plain-text form; args is a JSON list of arguments.
func scriptReply(code string, args string) string {
	var as []ScriptArg
	json.Unmarshal([]byte(args), &as)
	var b strings.Builder
	b.WriteString("tool_name: archive_by_prefix\ndescription: Move files whose names start with a prefix into a folder.\n")
	for _, a := range as {
		fmt.Fprintf(&b, "arg %s: %s | %s\n", a.Name, a.Value, a.Description)
	}
	b.WriteString("```python\n" + code + "```\n")
	return b.String()
}

func TestScriptPass(t *testing.T) {
	tr := archiveTrace(t)
	args := `[{"name":"prefix","description":"the start of the names","value":"2021-"},{"name":"dest","description":"the folder name","value":"archive"}]`
	hardcoded := "import json, os, sys\nargs = json.load(sys.stdin)\nfolder = args['folder']\nops = []\nfor n in sorted(os.listdir(folder)):\n    if n.startswith('2021-'):\n        ops.append({'op': 'move', 'from': os.path.join(folder, n), 'to': os.path.join(folder, args['dest'], n)})\nprint(json.dumps({'ops': ops, 'p': args['prefix']}))\n"
	wrong := strings.Replace(hardcoded, "n.startswith('2021-')", "n.endswith(args['prefix'])", 1)
	good := strings.Replace(hardcoded, "n.startswith('2021-')", "n.startswith(args['prefix'])", 1)
	good = strings.Replace(good, "ops = []\n", "ops = [{'op': 'make_dir', 'path': os.path.join(folder, args['dest'])}]\n", 1)
	// '2021-*' holds the prefix but isn't exactly it, so it can't be lifted into args["prefix"].
	hardcoded = strings.Replace(hardcoded, "n.startswith('2021-')", "fnmatch.fnmatch(n, '2021-*')", 1)
	hardcoded = strings.Replace(hardcoded, "import json, os, sys", "import fnmatch, json, os, sys", 1)
	llm := &scriptedLLM{replies: []string{
		`{"steps": ["Call ` + "`list_dir`" + `.", "Call ` + "`make_dir`" + `.", "Call ` + "`move_many`" + `."]}`,
		scriptReply(hardcoded, args),
		scriptReply(wrong, args),
		scriptReply(good, args),
		`{"name": "archive-by-prefix", "description": "Move files whose names start with a prefix into a folder. Use when archiving.", "triggers": ["move every file whose name starts with"]}`,
		`{"variants": [{"request": "Make a folder /data/x/old and move every file whose name starts with draft- into it.", "values": {"folder": "/data/x", "prefix": "draft-", "dest": "old"}}]}`,
	}}
	au := &Author{LLM: llm.call, Script: pythonRun(t), Confirm: func(tool string) bool { return tool == "move_many" }}
	d, err := au.Draft(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool == nil {
		t.Fatalf("no tool: %v\n%s", d.Notes, strings.Join(llm.prompts, "\n----\n"))
	}
	if !strings.Contains(llm.prompts[2], "2021-") || !strings.Contains(llm.prompts[2], "written in it") {
		t.Errorf("the hard-coded prefix wasn't reported:\n%s", llm.prompts[2])
	}
	if !strings.Contains(llm.prompts[3], "the task needs move 2021-feb.txt → archive/2021-feb.txt") {
		t.Errorf("the wrong ops weren't reported:\n%s", llm.prompts[3])
	}
	if strings.Contains(llm.prompts[1], "/data/sim-1") {
		t.Errorf("the script prompt shows the run's folder:\n%s", llm.prompts[1])
	}
	if !d.Tool.Confirm || len(d.Steps) != 2 || !strings.Contains(d.Steps[0], "`archive_by_prefix` once") {
		t.Errorf("tool %+v steps %q", d.Tool, d.Steps)
	}
	if !strings.Contains(d.Tool.Result, "move sim-1/2021-jan.txt → sim-1/archive/2021-jan.txt") {
		t.Errorf("result %s", d.Tool.Result)
	}
	c := d.Cases[0]
	if strings.Join(c.Expect.Tools, ",") != "archive_by_prefix" || !strings.Contains(strings.Join(c.Expect.ArgsContain, ","), "2021-") {
		t.Errorf("replay case %+v", c)
	}
	if len(d.Cases) != 2 || strings.Join(d.Cases[1].Expect.ArgsContain, ",") != "/data/x,draft-,old" {
		t.Errorf("variation %+v", d.Cases)
	}
	if d.Example != "Make a folder … and move every file whose name starts with … into it." {
		t.Errorf("example %q", d.Example)
	}
	files, err := d.Files()
	if err != nil {
		t.Fatal(err)
	}
	sk, issues := skills.Load(mapFS(files), d.Name, "", skills.SourceLearned)
	if sk == nil || len(issues) > 0 {
		t.Fatalf("doesn't load: %v\n%s", issues, files["SKILL.md"])
	}
	if len(sk.Tools) != 1 || sk.Tools[0].Runtime != "python" || sk.Meta.Confirm[0] != "archive_by_prefix" ||
		!strings.Contains(string(sk.Tools[0].InputSchema), `"format":"path"`) || !strings.Contains(sk.Description, "Example request") {
		t.Errorf("skill %+v %+v", sk.Meta, sk.Tools)
	}
	// A failed replay first makes the steps stricter, then drops the tool.
	if err := au.Repair(context.Background(), d, tr, nil); err != nil || d.Tool == nil || len(d.Steps) != 3 {
		t.Fatalf("first repair: %v %q", err, d.Steps)
	}
	llm.replies = []string{`{"variants": [{"request": "Make a folder /d/old and move every file whose name starts with x- into it."}]}`}
	if err := au.Repair(context.Background(), d, tr, nil); err != nil || d.Tool != nil || d.Cases[0].Expect.Tools[0] != "list_dir" {
		t.Fatalf("second repair: %v %+v", err, d.Cases)
	}
}

func TestExampleRequest(t *testing.T) {
	for in, want := range map[string]string{
		"Tidy up /tmp/x: put every .txt file into a folder named text and every .csv file into a folder named tables.": "Tidy up …: put every … file into a folder named … and every … file into a folder named ….",
		"Read /a/app.log, write every line that contains ERROR to /a/errors.txt.":                                      "Read …, write every line that contains … to ….",
	} {
		if got := ExampleRequest(in, []string{"text", "tables", "txt", "csv", "ERROR"}); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSplitScriptReply(t *testing.T) {
	out := "Here it is.\ntool_name: `combine_files`\ndescription: Join text files.\n- arg ext: .txt | the extension\narg out_name: \"all.txt\" | the result's name\n```python\nimport json\nprint(json.dumps({'ops': []}))\n```\nDone."
	r, why := splitScriptReply(out)
	if why != "" {
		t.Fatal(why)
	}
	if r.ToolName != "combine_files" || r.Description != "Join text files." || !strings.HasPrefix(r.Code, "import json\n") || strings.Contains(r.Code, "```") {
		t.Fatalf("%+v", r)
	}
	if len(r.Arguments) != 2 || r.Arguments[0] != (ScriptArg{"ext", "the extension", ".txt"}) || r.Arguments[1].Value != "all.txt" {
		t.Fatalf("%+v", r.Arguments)
	}
	// A repair reply often runs the keys together on one line.
	r, _ = splitScriptReply("rename_files description: Add a prefix. arg prefix: ui- | the prefix arg ext: .png | the extension ```python\nx\n```")
	if r.ToolName != "rename_files" || r.Description != "Add a prefix." || len(r.Arguments) != 2 || r.Arguments[1] != (ScriptArg{"ext", "the extension", ".png"}) {
		t.Fatalf("%+v", r)
	}
	if _, why := splitScriptReply(`{"code": "x"}`); why == "" {
		t.Fatal("a reply without a code block should fail")
	}
}

// A default in args.get(name, default) isn't a hard-coded value when the reply declares that
// argument with a value: the call always passes one.
func TestArgDefaultsAreNotLiterals(t *testing.T) {
	code := `out = args.get("out_name", "all.txt")` + "\n" + `ext = args.get('ext', '.txt')` + "\n" + `x = args.get("undeclared", "keep")`
	lits := stringLiterals(stripArgDefaults(code, []ScriptArg{{Name: "out_name", Value: "all.txt"}, {Name: "ext", Value: ".txt"}}))
	want := []string{"out_name", "ext", "undeclared", "keep"}
	if !slices.Equal(lits, want) {
		t.Fatalf("got %q, want %q", lits, want)
	}
}

func TestRequestValues(t *testing.T) {
	got := RequestValues(`Combine every .txt file in {folder} into one file named all.txt; add the prefix ui- and a folder called "Old Stuff" for 2024 notes.`)
	for _, v := range []string{".txt", "all.txt", "ui-", "2024"} {
		if !slices.Contains(got, v) {
			t.Errorf("%q missing from %q", v, got)
		}
	}
	for _, v := range []string{"{folder}", "combine", "file"} {
		if slices.Contains(got, v) {
			t.Errorf("%q shouldn't be a value: %q", v, got)
		}
	}
}

// A literal that is exactly a request value becomes an argument; one that only holds it doesn't.
func TestLiftValues(t *testing.T) {
	args := []ScriptArg{{Name: "folder", Value: "/x"}, {Name: "out", Value: "all.txt"}}
	code := `p = os.path.join(folder, "all.txt")` + "\n" + `if n.endswith('.md'):` + "\n" + `g = "*.md"`
	got, as := liftValues(code, args, []string{".md", "all.txt", "ui-"})
	want := `p = os.path.join(folder, args["out"])` + "\n" + `if n.endswith(args["ext"]):` + "\n" + `g = "*.md"`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if len(as) != 3 || as[2].Name != "ext" || as[2].Value != ".md" {
		t.Fatalf("args %+v", as)
	}
	// A declared argument's default is dropped first, so it isn't lifted into a second argument.
	args = []ScriptArg{{Name: "folder", Value: "/x"}, {Name: "extension", Value: "png"}}
	code = `e = args.get("extension", ".png")`
	if got, as := liftValues(code, args, []string{".png"}); got != code || len(as) != 2 {
		t.Fatalf("got %s %+v", got, as)
	}
}

func TestSelfWrite(t *testing.T) {
	for _, c := range []string{`with open(p, 'w', encoding='utf-8') as f:`, `open(out, mode="a")`, `shutil.copy2(a, b)`, `os.makedirs(d)`, `Path(p).write_text(x)`} {
		if selfWriteRe.FindString(c) == "" {
			t.Errorf("%s: not found", c)
		}
	}
	for _, c := range []string{`with open(p, 'r', encoding='utf-8') as f:`, `open(p)`, `os.path.join(a, b)`, `os.listdir(folder)`} {
		if m := selfWriteRe.FindString(c); m != "" {
			t.Errorf("%s: found %q", c, m)
		}
	}
}
