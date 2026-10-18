package learning

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"noskill/internal/agent"
)

func writeTrace(t *testing.T, events ...agent.Event) string {
	t.Helper()
	tw, err := agent.NewTraceWriter(t.TempDir(), "01RUN", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		tw.Write(e)
	}
	tw.Close()
	return tw.Path
}

func TestReadTrace(t *testing.T) {
	p := writeTrace(t,
		agent.Event{Kind: agent.EventRunStart, V: 1, Content: "sort my downloads", Model: "qwen3:4b", Source: "cli", Skills: []string{"files"}, Tool: "plan"},
		agent.Event{Kind: agent.EventToolCall, Step: 1, Tool: "list_dir", Skill: "files", Args: json.RawMessage(`{"path":"~/Downloads"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 1, Tool: "list_dir", Content: "a.pdf\nb.png"},
		agent.Event{Kind: agent.EventToolCall, Step: 2, Tool: "read_text", Args: json.RawMessage(`{"path":"/etc/x"}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 2, Tool: "read_text", Err: "permission denied"},
		agent.Event{Kind: agent.EventToolCall, Step: 3, Tool: "move_many", Skill: "files", Args: json.RawMessage(`{"plan":[]}`)},
		agent.Event{Kind: agent.EventToolResult, Step: 3, Tool: "move_many", Content: "moved 2"},
		agent.Event{Kind: agent.EventAnswer, Step: 4, Content: "Done."},
		agent.Event{Kind: agent.EventEnd, Step: 4, Note: "answered"},
		agent.Event{Kind: agent.EventFileEffect, Op: "move", From: "a.pdf", To: "Documents/a.pdf"},
		agent.Event{Kind: agent.EventEnd, Note: "answered", Tokens: 900, Ms: 1200},
	)
	agent.AppendEvent(p, agent.Event{Kind: agent.EventOutcome, Tool: "explicit", Content: "success", Note: "confidence 1.00: good"}, nil)
	tr, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	if tr.V != 1 || tr.Request != "sort my downloads" || tr.TaskType != "plan" || len(tr.Actions) != 3 || tr.Steps != 4 || tr.Stop != "answered" {
		t.Fatalf("%+v", tr)
	}
	if tr.Pattern() != "list_dir>move_many" || len(tr.Effects) != 1 || tr.Answer != "Done." {
		t.Errorf("pattern %q effects %v", tr.Pattern(), tr.Effects)
	}
	if tr.Outcome != (Outcome{Value: "success", Confidence: 1, Source: "explicit", Note: "good"}) {
		t.Errorf("outcome %+v", tr.Outcome)
	}
	if c := tr.Compress(20); !strings.Contains(c, `1. list_dir("path":"~/Downloads") → a.pdf b.png`) || strings.Contains(c, "read_text") {
		t.Errorf("compress:\n%s", c)
	}
}
