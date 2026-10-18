// Package learning is the learning loop (LEARNING.md, FR-L2–L5): it reads traces, mines runs worth
// turning into skills, drafts them with the skill-author passes, verifies them, and moves learned
// skills through trial, promotion, refinement and retirement.
package learning

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"noskill/internal/agent"
)

// Action is one tool call and its result.
type Action struct {
	Step   int             `json:"step"`
	Tool   string          `json:"tool"`
	Skill  string          `json:"skill,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result string          `json:"result,omitempty"`
	Err    string          `json:"err,omitempty"`
	Denied bool            `json:"denied,omitempty"`
}

type FileEffect struct {
	Op, From, To string
}

type Outcome struct {
	Value      string  // success | partial | fail | unknown
	Confidence float64 // 0–1
	Source     string  // explicit | check | implicit | decision | llm_judge
	Note       string  // why, when the source said (a user's note, a failed check)
}

// Trace is a run as the learning loop sees it.
type Trace struct {
	V        int
	RunID    string
	Path     string
	Start    time.Time // the first event's time
	Request  string
	Source   string
	Parent   string
	Model    string
	TaskType string
	Skills   []string
	Actions  []Action
	Effects  []FileEffect
	Answer   string
	Stop     string
	Steps    int
	Tokens   int
	Ms       int64
	Outcome  Outcome // the last one recorded wins
}

// ReadTrace parses a trace file. v0 traces (no run_start) take the request from the route event,
// which may be cut at 400 characters.
func ReadTrace(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &Trace{Path: path, RunID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Outcome: Outcome{Value: "unknown"}}
	open := map[string]int{} // tool → index of its latest call without a result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var e agent.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if t.Start.IsZero() {
			t.Start = e.Time
		}
		switch e.Kind {
		case agent.EventRunStart:
			t.V, t.Request, t.Model, t.Source, t.Parent, t.Skills, t.TaskType = e.V, e.Content, e.Model, e.Source, e.Parent, e.Skills, e.Tool
		case agent.EventRoute:
			if t.Request == "" {
				t.Request, t.Model, t.TaskType = e.Content, e.Model, e.Tool
			}
		case agent.EventSelect:
			if t.Skills == nil && e.Note != "" {
				t.Skills = strings.Split(e.Note, ", ")
			}
		case agent.EventToolCall:
			t.Actions = append(t.Actions, Action{Step: e.Step, Tool: e.Tool, Skill: e.Skill, Args: e.Args})
			open[e.Tool] = len(t.Actions) - 1
		case agent.EventToolResult:
			if i, ok := open[e.Tool]; ok {
				t.Actions[i].Result, t.Actions[i].Err = e.Content, e.Err
				delete(open, e.Tool)
			}
		case agent.EventToolDenied:
			if i, ok := open[e.Tool]; ok {
				t.Actions[i].Denied, t.Actions[i].Err = true, e.Note
				delete(open, e.Tool)
			}
		case agent.EventFileEffect:
			t.Effects = append(t.Effects, FileEffect{Op: e.Op, From: e.From, To: e.To})
		case agent.EventAnswer:
			t.Answer = e.Content
		case agent.EventEnd:
			if e.Step > 0 { // the loop's own end line
				t.Steps = e.Step
			} else {
				t.Stop, t.Tokens, t.Ms = e.Note, e.Tokens, e.Ms
			}
		case agent.EventOutcome:
			_, why, _ := strings.Cut(e.Note, ": ")
			t.Outcome = Outcome{Value: e.Content, Source: e.Tool, Confidence: parseConfidence(e.Note), Note: why}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

func parseConfidence(note string) float64 {
	rest, ok := strings.CutPrefix(note, "confidence ")
	if !ok {
		return 0
	}
	num, _, _ := strings.Cut(rest, ":")
	f, _ := strconv.ParseFloat(strings.TrimSpace(num), 64)
	return f
}

// Calls are the actions that ran without an error or refusal: what a skill would repeat.
func (t *Trace) Calls() []Action {
	var out []Action
	for _, a := range t.Actions {
		if a.Err == "" && !a.Denied {
			out = append(out, a)
		}
	}
	return out
}

// Pattern is the sequence of tool names, ignoring arguments (LEARNING.md §2).
func (t *Trace) Pattern() string {
	var names []string
	for _, a := range t.Calls() {
		names = append(names, a.Tool)
	}
	return strings.Join(names, ">")
}

// Compress is pass (a): one line per successful call, `tool(args) → result`, short enough for an
// 8k context.
func (t *Trace) Compress(maxResult int) string {
	var b strings.Builder
	for i, a := range t.Calls() {
		fmt.Fprintf(&b, "%d. %s(%s) → %s\n", i+1, a.Tool, compactJSON(a.Args), clip(a.Result, maxResult))
	}
	return b.String()
}

func compactJSON(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(r, &v) != nil {
		return string(r)
	}
	b, _ := json.Marshal(v)
	return strings.TrimSuffix(strings.TrimPrefix(string(b), "{"), "}")
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
