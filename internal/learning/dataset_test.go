package learning

import (
	"encoding/json"
	"strings"
	"testing"
)

func goodTrace() *Trace {
	return &Trace{V: 1, RunID: "r1", Source: "cli", Request: "Move the PDFs in ~/Downloads into Documents",
		Model: "qwen3:4b", TaskType: "files", Stop: "answered",
		Actions: []Action{
			{Tool: "list_dir", Args: json.RawMessage(`{"path":"/Users/me/Downloads"}`), Result: "a.pdf\nb.txt"},
			{Tool: "move_many", Args: json.RawMessage(`{"moves":[{"from":"/Users/me/Downloads/a.pdf","to":"/Users/me/Documents/a.pdf"}]}`), Result: "moved 1"},
		},
		Answer:  "Moved a.pdf. Token sk-123 used.",
		Outcome: Outcome{Value: "success", Source: "explicit", Confidence: 1}}
}

func TestTrainable(t *testing.T) {
	if ok, why := Trainable(goodTrace()); !ok {
		t.Fatalf("a good run: %s", why)
	}
	for name, edit := range map[string]func(*Trace){
		"eval run":      func(t *Trace) { t.Source = "eval" },
		"old trace":     func(t *Trace) { t.V = 0 },
		"partial":       func(t *Trace) { t.Outcome.Value = "partial" },
		"unsure judge":  func(t *Trace) { t.Outcome = Outcome{Value: "success", Source: "llm_judge", Confidence: 0.6} },
		"no answer":     func(t *Trace) { t.Answer = " " },
		"budget":        func(t *Trace) { t.Stop = "max_steps" },
		"a failed call": func(t *Trace) { t.Actions[1].Err = "no such file" },
		"a refusal":     func(t *Trace) { t.Actions[0].Denied = true },
	} {
		tr := goodTrace()
		edit(tr)
		if ok, _ := Trainable(tr); ok {
			t.Errorf("%s: trainable", name)
		}
	}
	sure := goodTrace()
	sure.Outcome = Outcome{Value: "success", Source: "llm_judge", Confidence: 0.9}
	if ok, why := Trainable(sure); !ok {
		t.Errorf("a sure judge: %s", why)
	}
}

func TestExamplesOnePerTurn(t *testing.T) {
	scrub := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "/Users/me", "~"), "sk-123", "[secret]")
	}
	tools := []ChatTool{{Type: "function", Function: ChatToolSpec{Name: "list_dir", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	ex := Examples(goodTrace(), "You are noskill.", tools, scrub)
	if len(ex) != 3 {
		t.Fatalf("%d examples, want 3 (two calls and the answer)", len(ex))
	}
	// Each ends with an assistant turn; the last is the answer after both calls and their results.
	for i, e := range ex {
		if last := e.Messages[len(e.Messages)-1]; last.Role != "assistant" || len(e.Tools) != 1 {
			t.Errorf("example %d ends with %+v", i, last)
		}
	}
	last := ex[2].Messages
	if len(last) != 7 || last[0].Role != "system" || last[1].Role != "user" || last[3].Role != "tool" || last[3].ToolCallID != "call_1" {
		t.Fatalf("%+v", last)
	}
	b, _ := json.Marshal(ex[2])
	s := string(b)
	if strings.Contains(s, "/Users/me") || strings.Contains(s, "sk-123") || !strings.Contains(s, `"arguments":{"path":"~/Downloads"}`) {
		t.Errorf("not scrubbed, or arguments not an object:\n%s", s)
	}
	if !strings.Contains(ReviewText(goodTrace(), scrub), "`list_dir(\"path\":\"~/Downloads\")` → a.pdf b.txt") {
		t.Errorf("review:\n%s", ReviewText(goodTrace(), scrub))
	}
}

func TestDedupAndSplit(t *testing.T) {
	a, b := goodTrace(), goodTrace()
	b.Request = "Move the PDFs in ~/Desktop into Documents"
	if DedupKey(a) != DedupKey(b) {
		t.Errorf("same request with another folder: %q vs %q", DedupKey(a), DedupKey(b))
	}
	b.Actions = b.Actions[:1]
	if DedupKey(a) == DedupKey(b) {
		t.Error("other calls are another example")
	}
	n := 0
	for i := range 1000 {
		if ValidRun(strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)) {
			n++
		}
	}
	if n < 50 || n > 150 || ValidRun("r1") != ValidRun("r1") {
		t.Errorf("%d of 1000 in validation", n)
	}
}
