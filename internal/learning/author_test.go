package learning

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"noskill/internal/agent"
	"noskill/internal/decide"
	"noskill/internal/skills"
)

type scriptedLLM struct {
	replies []string
	prompts []string
}

func (s *scriptedLLM) call(_ context.Context, prompt string, schema json.RawMessage, _ int) (string, error) {
	if strings.Contains(string(schema), `"required":null`) || strings.Contains(string(schema), `"properties":{}`) {
		panic("Ollama refuses a null required list, and small models pad an empty object until they run out: " + string(schema))
	}
	s.prompts = append(s.prompts, prompt)
	if len(s.replies) == 0 {
		return "", errors.New("no reply")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

type noulDecider map[string]float64

func (n noulDecider) Decide(_ context.Context, _ map[string]any, qs map[string]decide.Question) (decide.Answers, error) {
	out := decide.Answers{Answers: map[string]decide.Answer{}}
	for k, q := range qs {
		p := 0.1
		for v, x := range n {
			if strings.Contains(q.Instructions, v) {
				p = x
			}
		}
		out.Answers[k] = decide.Answer{Noul: p}
	}
	return out, nil
}

func downloadsTrace(t *testing.T) *Trace {
	p := writeTrace(t,
		agent.Event{Kind: agent.EventRunStart, V: 1, Content: "Move the PDFs in my Downloads to Documents/PDFs", Model: "qwen3:4b", Tool: "files", Skills: []string{"files"}},
		agent.Event{Kind: agent.EventToolCall, Step: 1, Tool: "list_dir", Skill: "files", Args: json.RawMessage(`{"path":"/home/u/Downloads"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 1, Tool: "list_dir", Content: "a.pdf b.pdf c.png"},
		agent.Event{Kind: agent.EventToolCall, Step: 2, Tool: "move_many", Skill: "files", Args: json.RawMessage(`{"dry_run":false,"plan":[{"from":"/home/u/Downloads/a.pdf","to":"/home/u/Documents/PDFs/a.pdf"}],"mode":"keep-both"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 2, Tool: "move_many", Content: "moved 1"},
		agent.Event{Kind: agent.EventAnswer, Step: 3, Content: "Moved a.pdf."},
		agent.Event{Kind: agent.EventEnd, Step: 3, Note: "answered"},
		agent.Event{Kind: agent.EventFileEffect, Op: "move", From: "/home/u/Downloads/a.pdf", To: "/home/u/Documents/PDFs/a.pdf"},
		agent.Event{Kind: agent.EventEnd, Note: "answered"},
	)
	tr, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestAuthorDraft(t *testing.T) {
	tr := downloadsTrace(t)
	llm := &scriptedLLM{replies: []string{
		`{"steps": ["1. Call ` + "`list_dir`" + ` on {path}.", "Call ` + "`move_many`" + ` with dry_run true, then false, moving each PDF to {to}."]}`,
		`{"name": "files", "description": "Move PDF files from a folder into another folder. Use when the user asks to file away PDFs.", "triggers": ["file my pdfs"]}`,
		`{"variants": [{"request": "Move the PDFs in my Desktop to Documents/PDFs", "values": {"path": "Desktop"}}]}`,
	}}
	au := &Author{LLM: llm.call, Decide: noulDecider{"keep-both": 0.2}, Home: "/home/u",
		Taken: func(n string) bool { return n == "files" }}
	d, err := au.Draft(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "files-2" || !strings.HasPrefix(d.Description, "Move PDF files") || len(d.Steps) != 2 || d.Steps[0] != "Call `list_dir` on {path}." {
		t.Fatalf("%+v", d)
	}
	var names []string
	for _, in := range d.Inputs {
		names = append(names, in.Name+"="+in.Default)
	}
	// Request values are inputs; "keep-both" is fixed (decide said so); the file paths are unclear → inputs.
	if got := strings.Join(names, " "); !strings.Contains(got, "path=~/Downloads") || strings.Contains(got, "keep-both") {
		t.Errorf("inputs %s", got)
	}
	if strings.Join(d.DependsOn, ",") != "files" || strings.Join(d.Filesystem, ",") != "write:~/Downloads,write:~/Documents/PDFs" {
		t.Errorf("deps %v fs %v", d.DependsOn, d.Filesystem)
	}
	if len(d.Cases) != 2 || strings.Join(d.Cases[0].Expect.Tools, ">") != "list_dir>move_many" || d.Cases[1].Expect.ArgsContain[0] != "Desktop" {
		t.Errorf("cases %+v", d.Cases)
	}
	if !strings.Contains(llm.prompts[0], "{path}") {
		t.Errorf("the steps prompt doesn't show the inputs:\n%s", llm.prompts[0])
	}
	files, err := d.Files()
	if err != nil {
		t.Fatal(err)
	}
	sk, issues := skills.Load(mapFS(files), "files-2", "", skills.SourceLearned)
	if sk == nil || len(issues) > 0 {
		t.Fatalf("draft doesn't load: %v\n%s", issues, files["SKILL.md"])
	}
	if sk.Status() != "draft" || sk.Origin() != "learned" || sk.Meta.DependsOn[0] != "files" {
		t.Errorf("meta %+v", sk.Meta)
	}
	cases, fx, err := ReadCases(func(n string) ([]byte, error) { return files[n], nil })
	if err != nil || len(cases) != 2 || len(fx["fixtures/replay.json"]) != 2 {
		t.Fatalf("cases %v %v %v", cases, fx, err)
	}
}

// A run whose request has no changing values still gets a schema Ollama accepts for the variations
// pass (a nil required list once went out as null and Ollama refused it).
func TestAuthorDraftWithoutRequestValues(t *testing.T) {
	p := writeTrace(t,
		agent.Event{Kind: agent.EventRunStart, V: 1, Content: "Print a random UUID with Python", Model: "qwen3.5:4b", Tool: "code-runner", Skills: []string{"code-runner"}},
		agent.Event{Kind: agent.EventToolCall, Step: 1, Tool: "run_python", Skill: "code-runner", Args: json.RawMessage(`{"code":"import uuid\nprint(uuid.uuid4())"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 1, Tool: "run_python", Content: `{"stdout":"6f1c…"}`},
		agent.Event{Kind: agent.EventAnswer, Step: 2, Content: "6f1c…"},
		agent.Event{Kind: agent.EventEnd, Note: "answered"},
	)
	tr, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	llm := &scriptedLLM{replies: []string{
		`{"steps": ["Call ` + "`run_python`" + ` with a script that prints uuid.uuid4()."]}`,
		`{"name": "random-uuid", "description": "Make a random UUID. Use when the user needs a new unique id.", "triggers": ["random uuid"]}`,
		`{"variants": [{"request": "Give me a fresh UUID using Python"}]}`,
	}}
	d, err := (&Author{LLM: llm.call, Home: "/home/u"}).Draft(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.prompts) != 3 || len(d.Cases) != 2 {
		t.Errorf("%d prompts, cases %+v, notes %v", len(llm.prompts), d.Cases, d.Notes)
	}

	// Steps written from the trace describe the program instead of quoting it.
	d, err = (&Author{LLM: (&scriptedLLM{}).call, Home: "/home/u"}).Draft(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(d.Steps, "\n"); strings.Contains(s, "uuid4") || !strings.Contains(s, "`run_python`") {
		t.Errorf("steps %q", s)
	}
}

func TestAuthorFallsBackWhenTheModelFails(t *testing.T) {
	tr := downloadsTrace(t)
	au := &Author{LLM: (&scriptedLLM{}).call, Home: "/home/u"}
	d, err := au.Draft(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if !nameRe.MatchString(d.Name) || !strings.Contains(d.Steps[0], "`list_dir`") || len(d.Notes) < 2 || len(d.Cases) != 1 {
		t.Fatalf("%+v", d)
	}
}

// Block 2c: learned skills named the source run's files ("read_text {path}/x.txt") or wrote
// steps as code, and the next request with other files failed.
func TestAuthorStepsDontKeepThisRunsFiles(t *testing.T) {
	p := writeTrace(t,
		agent.Event{Kind: agent.EventRunStart, V: 1, Content: "Read every .txt file in /w and write a new file /w/all.txt with all their lines.", Model: "qwen3.5:4b", Tool: "files", Skills: []string{"files"}},
		agent.Event{Kind: agent.EventToolCall, Step: 1, Tool: "list_dir", Skill: "files", Args: json.RawMessage(`{"path":"/w"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 1, Tool: "list_dir", Content: "x.txt y.txt"},
		agent.Event{Kind: agent.EventToolCall, Step: 2, Tool: "read_text", Skill: "files", Args: json.RawMessage(`{"path":"/w/xray.txt"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 2, Tool: "read_text", Content: "x"},
		agent.Event{Kind: agent.EventToolCall, Step: 3, Tool: "read_text", Skill: "files", Args: json.RawMessage(`{"path":"/w/yankee.txt"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 3, Tool: "read_text", Content: "y"},
		agent.Event{Kind: agent.EventToolCall, Step: 4, Tool: "write_text", Skill: "files", Args: json.RawMessage(`{"path":"/w/all.txt","content":"x\ny"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 4, Tool: "write_text", Content: "ok"},
		agent.Event{Kind: agent.EventAnswer, Step: 5, Content: "Done."},
		agent.Event{Kind: agent.EventEnd, Note: "answered"},
	)
	tr, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"steps": ["Call ` + "`list_dir`" + ` on {path}.", "Call ` + "`read_text`" + ` on {path}/xray.txt.", "Call ` + "`write_text`" + ` to {path}/all.txt."]}`,
		`{"steps": ["Call ` + "`list_dir`" + ` on {path}.", "Call ` + "`read_text`" + ` for name in names.", "Call ` + "`write_text`" + ` to {path}/all.txt."]}`,
	} {
		llm := &scriptedLLM{replies: []string{bad}}
		d, err := (&Author{LLM: llm.call, Decide: noulDecider{}}).Draft(context.Background(), tr)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Join(d.Steps, "\n")
		if strings.Contains(s, "xray") || strings.Contains(s, "yankee") || strings.Contains(s, " in names") || !strings.Contains(s, "once for each item") {
			t.Errorf("steps %q (notes %v)", s, d.Notes)
		}
		if strings.Contains(llm.prompts[0], "yankee") || !strings.Contains(llm.prompts[0], "<each item>") {
			t.Errorf("the steps prompt shows each file:\n%s", llm.prompts[0])
		}
	}
}
