package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type EventKind string

const (
	EventStep       EventKind = "step"        // a model call starts
	EventDelta      EventKind = "delta"       // streamed content or thinking
	EventToolCall   EventKind = "tool_call"   // the model asked for a tool
	EventToolResult EventKind = "tool_result" // the tool returned (possibly an error)
	EventToolDenied EventKind = "tool_denied" // a tool no selected skill owns, or a permission refusal
	EventRepeat     EventKind = "repeat"      // same call, same arguments: blocked, tools off
	EventRetry      EventKind = "retry"       // malformed output, retried or shown as is
	EventEvicted    EventKind = "evicted"     // context trimmed
	EventBudget     EventKind = "budget"      // a step or token limit was reached
	EventRoute      EventKind = "route"       // classification and model pick (ORCHESTRATOR.md)
	EventSelect     EventKind = "select"      // skills selected for the run
	EventProgress   EventKind = "progress"    // a status line for clients ("Loading qwen3:4b…")
	EventOutcome    EventKind = "outcome"     // appended later: rating, check or implicit signal
	EventRunStart   EventKind = "run_start"   // first line (trace v1): the full request and the run's setup
	EventFileEffect EventKind = "file_effect" // a file change the run made (from the undo log)
	EventApproval   EventKind = "approval"    // a learned-skill or confirm question and its answer
	EventAnswer     EventKind = "answer"
	EventEnd        EventKind = "end"
)

// Event is one line of a trace (FR-A4) and one progress update for streaming (FR-A3).
type Event struct {
	Time      time.Time       `json:"ts"`
	Kind      EventKind       `json:"kind"`
	Step      int             `json:"step,omitempty"`
	Model     string          `json:"model,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Content   string          `json:"content,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Err       string          `json:"err,omitempty"`
	Note      string          `json:"note,omitempty"`
	Final     bool            `json:"final,omitempty"`
	Bytes     int             `json:"bytes,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Ms        int64           `json:"ms,omitempty"`
	Tokens    int             `json:"tokens,omitempty"`

	// Trace v1 (LEARNING.md "Traces"): run_start carries V, Source, Parent and Skills; tool events
	// carry the Skill that owns the tool; file_effect carries Op, From and To.
	V      int      `json:"v,omitempty"`
	Source string   `json:"source,omitempty"`
	Parent string   `json:"parent,omitempty"`
	Skills []string `json:"skills,omitempty"`
	Skill  string   `json:"skill,omitempty"`
	Op     string   `json:"op,omitempty"`
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
}

// TraceVersion is written in run_start. Traces without it (block 1 and earlier) are read as v0.
const TraceVersion = 1

// TraceWriter appends events to traces/YYYY/MM/DD/<run-id>.jsonl. Streaming deltas are not
// written (the answer event has the full text). Secret values are replaced before anything reaches
// the disk (SECURITY.md T4).
type TraceWriter struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	Path    string
	secrets []string
}

func NewTraceWriter(dir, runID string, start time.Time, secrets []string) (*TraceWriter, error) {
	day := start.UTC()
	p := filepath.Join(dir, day.Format("2006"), day.Format("01"), day.Format("02"), runID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var s []string
	for _, v := range secrets {
		if len(v) >= 4 { // shorter values would redact ordinary text
			s = append(s, v)
		}
	}
	return &TraceWriter{f: f, w: bufio.NewWriter(f), Path: p, secrets: s}, nil
}

// Write records e. It's safe to use as Loop.OnEvent (directly or from a fan-out).
func (t *TraceWriter) Write(e Event) {
	if e.Kind == EventDelta {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	line := t.redact(string(b))
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w.WriteString(line)
	t.w.WriteByte('\n')
	if e.Kind == EventEnd {
		t.w.Flush()
	}
}

func (t *TraceWriter) redact(s string) string {
	for _, v := range t.secrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[secret]")
		}
		// The same value as it appears inside JSON (quotes, backslashes, <>& escaped).
		if enc, err := json.Marshal(v); err == nil {
			if e := string(enc[1 : len(enc)-1]); e != v && strings.Contains(s, e) {
				s = strings.ReplaceAll(s, e, "[secret]")
			}
		}
	}
	return s
}

func (t *TraceWriter) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.w.Flush(); err != nil {
		t.f.Close()
		return err
	}
	return t.f.Close()
}

// AppendEvent adds one line to an existing trace (an outcome that arrives after the run). The
// same redaction applies; the file is opened for append only.
func AppendEvent(path string, e Event, secrets []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	t := &TraceWriter{secrets: secrets}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.WriteString(t.redact(string(b)) + "\n")
	return err
}
