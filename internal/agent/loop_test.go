package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
)

type fakeTools struct {
	calls []string
	out   func(name string, args json.RawMessage) (string, error)
}

func (f *fakeTools) Defs() []backends.ToolDef {
	p := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	return []backends.ToolDef{{Name: "search", Description: "Web search.", Parameters: p}, {Name: "fetch", Description: "Fetch a page.", Parameters: p}}
}

func (f *fakeTools) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	f.calls = append(f.calls, name+" "+string(args))
	if f.out != nil {
		return f.out(name, args)
	}
	return "result for " + string(args), nil
}

func call(name, args string) backends.ToolCall {
	return backends.ToolCall{Name: name, Args: json.RawMessage(args)}
}

func req() Request {
	b := DefaultBudget()
	b.MaxSteps = 4
	return Request{RunID: "r1", Model: "qwen3:4b", Budget: b, SmallModel: true,
		Messages: []backends.Message{{Role: backends.RoleSystem, Content: "sys"}, {Role: backends.RoleUser, Content: "q"}}}
}

func kinds(evs []Event) string {
	var k []string
	for _, e := range evs {
		if e.Kind != EventDelta && e.Kind != EventStep {
			k = append(k, string(e.Kind))
		}
	}
	return strings.Join(k, ",")
}

func run(t *testing.T, fake *backendtest.Fake, tools Tools, r Request) (Result, []Event) {
	t.Helper()
	var evs []Event
	l := &Loop{Backend: fake, Tools: tools, OnEvent: func(e Event) { evs = append(evs, e) }}
	res, err := l.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return res, evs
}

func TestToolThenAnswer(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("search", `{"q":"ollama"}`)}},
		{Content: "Ollama 0.35 added decisions [1]."},
	}}
	tools := &fakeTools{}
	res, evs := run(t, fake, tools, req())
	if res.Stop != StopAnswered || res.Answer != "Ollama 0.35 added decisions [1]." || res.Steps != 2 {
		t.Fatalf("res = %+v", res)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("calls = %v", tools.calls)
	}
	first, second := fake.Requests[0], fake.Requests[1]
	if len(first.Tools) != 2 || first.Think == nil || *first.Think {
		t.Errorf("tool step: tools=%d think=%v", len(first.Tools), first.Think)
	}
	last := second.Messages[len(second.Messages)-1]
	if last.Role != backends.RoleTool || last.ToolName != "search" || last.ToolCallID == "" {
		t.Errorf("tool result message = %+v", last)
	}
	if kinds(evs) != "tool_call,tool_result,answer,end" {
		t.Errorf("events = %s", kinds(evs))
	}
}

func TestRepeatCallTurnsToolsOff(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("search", `{"q":"x"}`)}},
		{ToolCalls: []backends.ToolCall{call("search", `{ "q" : "x" }`)}}, // same call, different spacing
		{Content: "answer"},
	}}
	tools := &fakeTools{}
	res, evs := run(t, fake, tools, req())
	if len(tools.calls) != 1 {
		t.Fatalf("repeat ran: %v", tools.calls)
	}
	if fake.Requests[2].Tools != nil {
		t.Error("tools still offered after a repeat")
	}
	if res.Answer != "answer" || !strings.Contains(kinds(evs), "repeat") {
		t.Errorf("res = %+v, events = %s", res, kinds(evs))
	}
}

func TestStepLimitForcesAnswer(t *testing.T) {
	n := 0
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		if r.Tools == nil {
			return backends.Response{Content: "partial answer"}, nil
		}
		n++
		return backends.Response{ToolCalls: []backends.ToolCall{call("search", fmt.Sprintf(`{"q":"%d"}`, n))}}, nil
	}}
	res, _ := run(t, fake, &fakeTools{}, req())
	if res.Stop != StopMaxSteps || res.Answer != "partial answer" || fake.Calls() != 5 {
		t.Fatalf("res = %+v after %d calls", res, fake.Calls())
	}
	last := fake.Requests[4].Messages
	if !strings.Contains(last[len(last)-1].Content, "Step limit") {
		t.Errorf("no step-limit note: %+v", last[len(last)-1])
	}
}

func TestUnofferedToolIsNeverRun(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("files.trash", `{"path":"~/Documents"}`)}},
		{Content: "ok"},
	}}
	tools := &fakeTools{}
	_, evs := run(t, fake, tools, req())
	if len(tools.calls) != 0 {
		t.Fatalf("ran %v", tools.calls)
	}
	if !strings.Contains(kinds(evs), "tool_denied") {
		t.Errorf("events = %s", kinds(evs))
	}
	m := fake.Requests[1].Messages
	if !strings.Contains(m[len(m)-1].Content, "not available") {
		t.Errorf("model not told: %q", m[len(m)-1].Content)
	}
}

func TestLongToolOutputIsCut(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("fetch", `{"q":"u"}`)}},
		{Content: "done"},
	}}
	tools := &fakeTools{out: func(string, json.RawMessage) (string, error) { return strings.Repeat("é", 20000), nil }}
	r := req()
	r.Budget.ObservationTokens = 100
	run(t, fake, tools, r)
	m := fake.Requests[1].Messages
	got := m[len(m)-1].Content
	if len(got) > 500 || !strings.Contains(got, "[truncated:") || !strings.HasPrefix(got, "é") {
		t.Errorf("observation = %d bytes: %.60q", len(got), got)
	}
}

func TestMalformedNativeCallRetriesConstrained(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{{Name: "search", Args: json.RawMessage(`{"q": "unterminated`)}}},
		{Content: `{"thought":"search it","action":{"tool":"search","args":{"q":"ollama"}}}`},
		{Content: `{"final":"the answer"}`},
	}}
	r := req()
	r.CanConstrain = true
	tools := &fakeTools{}
	res, _ := run(t, fake, tools, r)
	if len(tools.calls) != 1 || tools.calls[0] != `search {"q":"ollama"}` {
		t.Fatalf("calls = %v", tools.calls)
	}
	if fake.Requests[1].Format == nil || fake.Requests[1].Tools != nil {
		t.Error("retry wasn't constrained")
	}
	// The run continues in native mode after the retried step.
	if fake.Requests[2].Tools == nil {
		t.Error("native tools not offered after the retry")
	}
	if res.Answer != `{"final":"the answer"}` {
		t.Errorf("answer = %q", res.Answer)
	}
}

func TestConstrainedStrategy(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{Content: `{"thought":"t","action":{"tool":"search","args":{"q":"a"}}}`},
		{Content: `{"final":"done"}`},
	}}
	r := req()
	r.Strategy = backends.StrategyConstrained
	res, _ := run(t, fake, &fakeTools{}, r)
	if res.Answer != "done" {
		t.Fatalf("answer = %q", res.Answer)
	}
	m := fake.Requests[1].Messages
	if m[len(m)-2].Role != backends.RoleUser || !strings.HasPrefix(m[len(m)-2].Content, "Result of search") {
		t.Errorf("constrained result message = %+v", m[len(m)-2])
	}
}

func TestTokenBudget(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		return backends.Response{ToolCalls: []backends.ToolCall{call("search", fmt.Sprintf(`{"q":"%d"}`, len(r.Messages)))},
			Usage: backends.Usage{PromptTokens: 3000, CompletionTokens: 100}}, nil
	}}
	r := req()
	r.Budget.MaxTokens = 5000
	res, _ := run(t, fake, &fakeTools{}, r)
	if res.Stop != StopMaxTokens || fake.Calls() != 2 {
		t.Fatalf("stop = %s after %d calls", res.Stop, fake.Calls())
	}
}

func TestStepTimeout(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("search", `{"q":"slow"}`)}},
		{Content: "gave up"},
	}}
	tools := &fakeTools{out: func(string, json.RawMessage) (string, error) {
		time.Sleep(200 * time.Millisecond)
		return "", context.DeadlineExceeded
	}}
	r := req()
	r.Budget.StepTimeout = 50 * time.Millisecond
	l := &Loop{Backend: fake, Tools: tools}
	// The tool ignores ctx here; the loop still reports the timeout as the result.
	res, err := l.Run(context.Background(), r)
	if err != nil || res.Answer != "gave up" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	m := fake.Requests[1].Messages
	if !strings.Contains(m[len(m)-1].Content, "longer than") {
		t.Errorf("observation = %q", m[len(m)-1].Content)
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) {
		cancel()
		return backends.Response{ToolCalls: []backends.ToolCall{call("search", `{"q":"x"}`)}}, nil
	}}
	res, err := (&Loop{Backend: fake, Tools: &fakeTools{}}).Run(ctx, req())
	if !errors.Is(err, context.Canceled) || res.Stop != StopCancelled {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestFitEvictsOldToolResultsFirst(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens
	msgs := []backends.Message{
		{Role: backends.RoleSystem, Content: "sys"},
		{Role: backends.RoleUser, Content: "question"},
		{Role: backends.RoleAssistant, ToolCalls: []backends.ToolCall{{ID: "a", Name: "fetch"}}},
		{Role: backends.RoleTool, Content: big, ToolCallID: "a"},
		{Role: backends.RoleAssistant, ToolCalls: []backends.ToolCall{{ID: "b", Name: "fetch"}}},
		{Role: backends.RoleTool, Content: big, ToolCallID: "b"},
	}
	var evs []Event
	l := &Loop{OnEvent: func(e Event) { evs = append(evs, e) }}
	out := l.fit(3, msgs, 1500)
	if out[3].Content != evictedNote || out[5].Content != big {
		t.Fatalf("old result kept or latest dropped: %q / %d bytes", out[3].Content, len(out[5].Content))
	}
	if msgs[3].Content != big {
		t.Fatal("fit edited the caller's slice")
	}
	if out[0].Content != "sys" || out[1].Content != "question" || len(evs) != 1 {
		t.Errorf("head changed or no event: %+v", evs)
	}
}

func TestTraceRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tw, err := NewTraceWriter(dir, "01RUN", start, []string{"canary-s3cr3t-value", "ab"})
	if err != nil {
		t.Fatal(err)
	}
	tw.Write(Event{Kind: EventToolResult, Content: `token is canary-s3cr3t-value; ab stays`})
	tw.Write(Event{Kind: EventDelta, Content: "not written"})
	tw.Write(Event{Kind: EventEnd})
	tw.Close()
	b, err := os.ReadFile(filepath.Join(dir, "2026", "10", "15", "01RUN.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "canary") || !strings.Contains(s, "[secret]") || !strings.Contains(s, "ab stays") || strings.Contains(s, "not written") {
		t.Fatalf("trace = %s", s)
	}
}

type waitErr string

func (w waitErr) Error() string      { return "needs approval " + string(w) }
func (w waitErr) WaitingFor() string { return string(w) }

// A parked run resumes from where it stopped: the approved call runs first, without asking the
// model again, then the loop carries on (FR-A6).
func TestParkAndResume(t *testing.T) {
	approved := false
	tools := &fakeTools{out: func(name string, args json.RawMessage) (string, error) {
		if name == "fetch" && !approved {
			return "", waitErr("01APPR")
		}
		return "ok " + name, nil
	}}
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("search", `{"q":"a"}`)}},
		{ToolCalls: []backends.ToolCall{call("fetch", `{"q":"b"}`)}},
	}}
	res, _ := run(t, fake, tools, req())
	if res.Stop != StopWaiting || res.WaitingFor != "01APPR" || len(res.Pending) != 1 || res.Pending[0].Name != "fetch" || res.Steps != 2 {
		t.Fatalf("park: %+v", res)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != backends.RoleAssistant || len(last.ToolCalls) != 1 {
		t.Fatalf("parked conversation must end with the pending call: %+v", last)
	}

	approved = true
	fake2 := &backendtest.Fake{Script: []backends.Response{{Content: "done"}}}
	r := req()
	r.Messages, r.Pending, r.StartStep = res.Messages, res.Pending, res.Steps
	res2, _ := run(t, fake2, tools, r)
	if res2.Stop != StopAnswered || res2.Answer != "done" || res2.Steps != 3 || fake2.Calls() != 1 {
		t.Fatalf("resume: %+v after %d model calls", res2, fake2.Calls())
	}
	sent := fake2.Requests[0].Messages
	if m := sent[len(sent)-1]; m.Role != backends.RoleTool || m.Content != "ok fetch" {
		t.Errorf("the approved call's result must reach the model: %+v", m)
	}
	if got := strings.Join(tools.calls, "|"); got != `search {"q":"a"}|fetch {"q":"b"}|fetch {"q":"b"}` {
		t.Errorf("calls = %s", got)
	}
}

func TestSplitThinking(t *testing.T) {
	for _, c := range []struct{ in, think, answer string }{
		{"Paris.", "", "Paris."},
		{"Okay, the user asks.\n</think>\n\nParis.", "Okay, the user asks.", "Paris."},
		{"<think>hmm</think>Paris.", "hmm", "Paris."},
		{"all thinking</think>", "", "all thinking</think>"},
	} {
		th, a := splitThinking("", c.in)
		if th != c.think || a != c.answer {
			t.Errorf("%q → (%q, %q)", c.in, th, a)
		}
	}
}

func TestToolStepsThinkOnlyWhenTheModelAlwaysThinks(t *testing.T) {
	for _, always := range []bool{false, true} {
		fake := &backendtest.Fake{Script: []backends.Response{
			{ToolCalls: []backends.ToolCall{call("search", `{"q":"a"}`)}},
			{Content: "done"},
		}}
		r := req()
		r.AlwaysThinks = always
		run(t, fake, &fakeTools{out: func(string, json.RawMessage) (string, error) { return "ok", nil }}, r)
		for i, cr := range fake.Requests {
			if cr.Think == nil || *cr.Think != always {
				t.Errorf("always=%v: step %d sent think=%v", always, i+1, cr.Think)
			}
		}
	}
}

func TestToolStepsUseGradedThinkingWhenAsked(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{call("search", `{"q":"a"}`)}},
		{Content: "done"},
	}}
	r := req()
	r.ToolThinkLevel = "low"
	run(t, fake, &fakeTools{out: func(string, json.RawMessage) (string, error) { return "ok", nil }}, r)
	if len(fake.Requests) == 0 {
		t.Fatal("no requests sent")
	}
	first := fake.Requests[0]
	if first.Think != nil {
		t.Errorf("tool step: Think should be nil when ToolThinkLevel is set, got %v", *first.Think)
	}
	if first.ThinkLevel != "low" {
		t.Errorf("tool step: ThinkLevel = %q, want low", first.ThinkLevel)
	}
}

func TestStepCutOffWhileThinkingIsAskedAgain(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{
		{Thinking: "Okay, the user wants… (cut off)"},
		{Content: "Paris."},
	}}
	res, evs := run(t, fake, nil, req())
	if res.Answer != "Paris." || fake.Calls() != 2 {
		t.Fatalf("%+v after %d calls", res, fake.Calls())
	}
	sent := fake.Requests[1].Messages
	if !strings.Contains(sent[len(sent)-1].Content, "act now") || !slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == EventRetry }) {
		t.Error("the retry must nudge the model and be traced")
	}

	// Thinking through the retry too is a max_tokens stop, not an empty answer.
	fake = &backendtest.Fake{Script: []backends.Response{
		{Thinking: "Okay, the user wants… (cut off)"},
		{Thinking: "Let me think again… (cut off)"},
	}}
	res, _ = run(t, fake, nil, req())
	if res.Stop != StopMaxTokens || res.Answer != "" || fake.Calls() != 2 {
		t.Errorf("%+v after %d calls", res, fake.Calls())
	}
}

func TestLongToolsGetTheirOwnTimeout(t *testing.T) {
	b := DefaultBudget()
	if b.toolTimeout("anime_run") != 45*time.Minute || b.toolTimeout("web_search") != b.StepTimeout {
		t.Errorf("anime_run %v, web_search %v", b.toolTimeout("anime_run"), b.toolTimeout("web_search"))
	}
	b.StepTimeout = time.Hour // a longer step limit is never cut down by the table
	if b.toolTimeout("anime_run") != time.Hour {
		t.Errorf("got %v", b.toolTimeout("anime_run"))
	}
}
