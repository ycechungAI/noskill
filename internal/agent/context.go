package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"noskill/internal/backends"
)

// Context management for small windows (FR-A2): tool results are cut before they re-enter the
// context, and old tool results are dropped first when the conversation no longer fits. The system
// prompt, the first user message and the latest turns stay verbatim.

const evictedNote = "[earlier tool result removed to fit the context window]"

// estimateTokens is a cheap upper-leaning estimate (~4 bytes per token for English and JSON).
func estimateTokens(m backends.Message) int {
	n := len(m.Content)/4 + 4
	for _, c := range m.ToolCalls {
		n += (len(c.Name)+len(c.Args))/4 + 4
	}
	return n + len(m.Images)*800
}

func totalTokens(msgs []backends.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateTokens(m)
	}
	return n
}

// fit returns msgs trimmed to about limit tokens. It never edits the caller's slice.
func (l *Loop) fit(step int, msgs []backends.Message, limit int) []backends.Message {
	if limit <= 0 || totalTokens(msgs) <= limit {
		return msgs
	}
	out := append([]backends.Message{}, msgs...)
	firstUser := -1
	for i, m := range out {
		if m.Role == backends.RoleUser {
			firstUser = i
			break
		}
	}
	keepTail := 2 // the latest call and its result
	evicted := 0
	// 1. Replace old tool results, oldest first.
	for i := firstUser + 1; i < len(out)-keepTail && totalTokens(out) > limit; i++ {
		if isToolResult(out[i]) && out[i].Content != evictedNote {
			out[i].Content = evictedNote
			evicted++
		}
	}
	// 2. Drop whole old turns between the first user message and the tail.
	for totalTokens(out) > limit && firstUser >= 0 && firstUser+1 < len(out)-keepTail {
		out = append(out[:firstUser+1], out[firstUser+2:]...)
		evicted++
	}
	if evicted > 0 {
		l.emit(Event{Kind: EventEvicted, Step: step, Note: fmt.Sprintf("%d old messages trimmed to fit %d tokens", evicted, limit)})
	}
	return dropOrphanResults(out)
}

func isToolResult(m backends.Message) bool {
	return m.Role == backends.RoleTool || (m.Role == backends.RoleUser && strings.HasPrefix(m.Content, "Result of "))
}

// dropOrphanResults removes native tool results whose assistant call was dropped: most chat
// templates reject a tool message without its call.
func dropOrphanResults(msgs []backends.Message) []backends.Message {
	ids := map[string]bool{}
	out := msgs[:0:0]
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			ids[c.ID] = true
		}
		if m.Role == backends.RoleTool && m.ToolCallID != "" && !ids[m.ToolCallID] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// truncate cuts s to about maxTokens, keeping the head (where results usually put what matters)
// and saying how much was cut.
func truncate(s string, maxTokens int) (string, bool) {
	maxBytes := maxTokens * 4
	if maxTokens <= 0 || len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 { // don't split a UTF-8 sequence
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[truncated: %d of %d bytes shown]", cut, len(s)), true
}

// actionSchema is the constrained-mode output (MODEL_ADAPTERS.md "Tool calling strategies"):
// {"thought", "action": {"tool": enum, "args": {...}}} or {"final": "..."}.
func actionSchema(defs []backends.ToolDef) (json.RawMessage, error) {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return json.Marshal(map[string]any{
		"anyOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thought": map[string]any{"type": "string"},
					"action": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tool": map[string]any{"type": "string", "enum": names},
							"args": map[string]any{"type": "object"},
						},
						"required": []string{"tool", "args"},
					},
				},
				"required": []string{"thought", "action"},
			},
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"final": map[string]any{"type": "string"}},
				"required":   []string{"final"},
			},
		},
	})
}

// toolMenu describes the tools for constrained mode, where they aren't passed natively.
func toolMenu(defs []backends.ToolDef) string {
	var b strings.Builder
	b.WriteString("You can use these tools. Reply with JSON: {\"thought\": \"...\", \"action\": {\"tool\": \"<name>\", \"args\": {...}}} to call one, or {\"final\": \"<answer>\"} when you can answer.\n")
	for _, d := range defs {
		fmt.Fprintf(&b, "- %s: %s Arguments (JSON Schema): %s\n", d.Name, d.Description, string(d.Parameters))
	}
	return b.String()
}

func parseAction(content string) (*backends.ToolCall, string, error) {
	var v struct {
		Thought string  `json:"thought"`
		Final   *string `json:"final"`
		Action  *struct {
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		} `json:"action"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &v); err != nil {
		return nil, "", err
	}
	if v.Final != nil {
		return nil, *v.Final, nil
	}
	if v.Action == nil || v.Action.Tool == "" {
		return nil, "", errors.New("neither action nor final")
	}
	args := v.Action.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return &backends.ToolCall{Name: v.Action.Tool, Args: args}, "", nil
}

// ConstrainedFormat is the JSON schema that forces the constrained strategy's output (ADR-0005).
func ConstrainedFormat(defs []backends.ToolDef) (json.RawMessage, error) { return actionSchema(defs) }

// ToolMenu describes the tools in the system prompt for the constrained strategy.
func ToolMenu(defs []backends.ToolDef) string { return toolMenu(defs) }

// ParseAction reads a constrained-mode reply: a tool call, or the final answer.
func ParseAction(content string) (call *backends.ToolCall, final string, err error) {
	return parseAction(content)
}
