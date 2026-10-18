package learning

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
)

// Training datasets for weight learning (FR-L8, ADR-0015): good runs from real use, as mlx-lm chat
// examples with tools. One example per assistant turn, so that with --mask-prompt (which trains an
// example's last message only) every tool call and the answer are trained.

// TrainSources are the run sources a dataset may use: real use only. Eval, learning, optimise and
// pinned runs are what the benchmark grades a trained model on.
var TrainSources = []string{"cli", "proxy", "tray", "job", "delegate"}

// MinTrainConfidence is how sure a judged or implicit success must be.
const MinTrainConfidence = 0.8

// ChatMessage is one message in mlx-lm's chat format (OpenAI's shape).
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type ChatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

type ChatFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // an object, not a string: chat templates render it
}

// ChatTool is one tool definition.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatToolSpec `json:"function"`
}

type ChatToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Example is one line of train.jsonl or valid.jsonl.
type Example struct {
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
}

// Trainable says whether a run may train a model, and why not.
func Trainable(t *Trace) (bool, string) {
	switch {
	case !slices.Contains(TrainSources, t.Source):
		return false, "a run from " + orNone(t.Source)
	case t.V == 0:
		return false, "an old trace (its request may be cut short)"
	case t.Outcome.Value != "success":
		return false, "outcome " + t.Outcome.Value
	case t.Outcome.Source != "explicit" && t.Outcome.Source != "check" && t.Outcome.Confidence < MinTrainConfidence:
		return false, fmt.Sprintf("judged a success with only %.2f confidence", t.Outcome.Confidence)
	case strings.TrimSpace(t.Request) == "" || strings.TrimSpace(t.Answer) == "":
		return false, "no request or no answer"
	case t.Stop != "" && t.Stop != "answered":
		return false, "it stopped: " + t.Stop
	}
	for _, a := range t.Actions {
		if a.Denied || a.Err != "" {
			return false, "a tool call failed or was refused"
		}
	}
	return true, ""
}

// DedupKey is the same for two runs of one request with different values and the same calls.
func DedupKey(t *Trace) string {
	return strings.Join(TaskWords(t.Request), " ") + "|" + t.Pattern()
}

// ValidRun puts about 1 run in 10 in the validation set, the same ones every export.
func ValidRun(runID string) bool {
	h := fnv.New32a()
	h.Write([]byte(runID))
	return h.Sum32()%10 == 0
}

// Examples turns a run into its examples: the conversation up to each assistant turn. scrub is
// applied to every text and argument (secrets, the home folder); arguments that stop being JSON
// after it are kept as they were.
func Examples(t *Trace, system string, tools []ChatTool, scrub func(string) string) []Example {
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	msgs := []ChatMessage{{Role: "system", Content: scrub(system)}, {Role: "user", Content: scrub(t.Request)}}
	var out []Example
	turn := func(m ChatMessage) {
		msgs = append(msgs, m)
		out = append(out, Example{Messages: slices.Clone(msgs), Tools: tools})
	}
	for i, a := range t.Actions {
		args := a.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if s := json.RawMessage(scrub(string(args))); json.Valid(s) {
			args = s
		}
		id := fmt.Sprintf("call_%d", i+1)
		turn(ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{{ID: id, Type: "function", Function: ChatFunction{Name: a.Tool, Arguments: args}}}})
		msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: id, Name: a.Tool, Content: scrub(a.Result)})
	}
	turn(ChatMessage{Role: "assistant", Content: scrub(t.Answer)})
	return out
}

// ReviewText is a run as REVIEW.md shows it: what was asked, each call, the answer.
func ReviewText(t *Trace, scrub func(string) string) string {
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s, %s, %s)\n\n", t.RunID, t.Source, orNone(t.TaskType), t.Model)
	fmt.Fprintf(&b, "**Request:** %s\n\n", scrub(t.Request))
	for i, a := range t.Actions {
		fmt.Fprintf(&b, "%d. `%s(%s)` → %s\n", i+1, a.Tool, clip(compactJSON(json.RawMessage(scrub(string(a.Args)))), 300), clip(oneLine(scrub(a.Result)), 200))
	}
	fmt.Fprintf(&b, "\n**Answer:** %s\n\n", clip(scrub(t.Answer), 1200))
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
