// Package probe picks a model's tool-calling strategy (FR-B3, ADR-0005): it runs the same 10 short
// tasks natively and constrained, and keeps native unless constrained is at least 10 points better.
// Models without native tool support get constrained.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"noskill/internal/agent"
	"noskill/internal/backends"
)

// Margin is how much better (in percentage points) constrained must be to replace native.
const Margin = 10.0

type task struct {
	prompt string
	tool   string // expected tool; "" = answer without any tool
	arg    string // argument that must contain want
	want   string
}

// Six tasks need one tool call; four are answered without tools. S1 found small models call tools
// on tasks that need none, so those four matter as much as the first six.
var tasks = []task{
	{"What time is it in Tokyo right now?", "get_time", "timezone", "tokyo"},
	{"Use the calculator: what is 1234 * 5678?", "calculate", "expression", "1234"},
	{"Search the web for the latest Ollama release.", "web_search", "query", "ollama"},
	{"Read the file notes.txt and tell me what it says.", "read_file", "path", "notes.txt"},
	{"What's the current time in London?", "get_time", "timezone", "london"},
	{"Look up who won the 2022 football World Cup.", "web_search", "query", "world cup"},
	{"Say hello in French.", "", "", ""},
	{"What is the capital of France?", "", "", ""},
	{"Write a haiku about rain.", "", "", ""},
	{"What does the word 'ubiquitous' mean?", "", "", ""},
}

func defs() []backends.ToolDef {
	p := func(prop string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"}},"required":[%q]}`, prop, prop))
	}
	return []backends.ToolDef{
		{Name: "get_time", Description: "Current time in a time zone or city.", Parameters: p("timezone")},
		{Name: "calculate", Description: "Evaluate an arithmetic expression exactly.", Parameters: p("expression")},
		{Name: "web_search", Description: "Search the web for current information.", Parameters: p("query")},
		{Name: "read_file", Description: "Read a text file from the user's folder.", Parameters: p("path")},
	}
}

// Score is one mode's result.
type Score struct {
	Passed int           `json:"passed"`
	Total  int           `json:"total"`
	Errors int           `json:"errors"` // malformed output or a request failure
	Mean   time.Duration `json:"mean_ms"`
}

func (s Score) Percent() float64 {
	if s.Total == 0 {
		return 0
	}
	return 100 * float64(s.Passed) / float64(s.Total)
}

// Result is the probe's verdict, stored with the model profile.
type Result struct {
	Model       string                `json:"model"`
	Strategy    backends.ToolStrategy `json:"strategy"`
	Native      *Score                `json:"native,omitempty"`
	Constrained *Score                `json:"constrained,omitempty"`
	Reason      string                `json:"reason"`
}

// Run probes model on b. info says whether the model advertises native tools.
func Run(ctx context.Context, b backends.Backend, info backends.ModelInfo, numCtx int) (Result, error) {
	res := Result{Model: info.Name}
	if numCtx <= 0 {
		numCtx = 4096
	}
	if !info.Has(backends.CapSchema) {
		// No constrained decoding to fall back on: native if it has it, else the few-shot text format.
		if info.Has(backends.CapTools) {
			res.Strategy, res.Reason = backends.StrategyNative, "no constrained decoding on this backend"
		} else {
			res.Strategy, res.Reason = backends.StrategyPrompted, "no native tools and no constrained decoding"
		}
		return res, nil
	}
	if info.Has(backends.CapTools) {
		s, err := score(ctx, b, info.Name, numCtx, true)
		if err != nil {
			return res, err
		}
		res.Native = &s
	}
	c, err := score(ctx, b, info.Name, numCtx, false)
	if err != nil {
		return res, err
	}
	res.Constrained = &c
	switch {
	case res.Native == nil:
		res.Strategy, res.Reason = backends.StrategyConstrained, "no native tool support"
	case c.Percent() >= res.Native.Percent()+Margin:
		res.Strategy = backends.StrategyConstrained
		res.Reason = fmt.Sprintf("constrained %.0f%% vs native %.0f%% (≥ %.0f points better)", c.Percent(), res.Native.Percent(), Margin)
	default:
		res.Strategy = backends.StrategyNative
		res.Reason = fmt.Sprintf("native %.0f%% vs constrained %.0f%% (constrained isn't %.0f points better)", res.Native.Percent(), c.Percent(), Margin)
	}
	return res, nil
}

func score(ctx context.Context, b backends.Backend, model string, numCtx int, native bool) (Score, error) {
	var s Score
	no := false
	temp, seed := 0.0, 1
	ds := defs()
	format, err := agent.ConstrainedFormat(ds)
	if err != nil {
		return s, err
	}
	var total time.Duration
	for _, t := range tasks {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		req := backends.ChatRequest{Model: model, Think: &no,
			Options: backends.Options{NumCtx: numCtx, NumPredict: 256, Temperature: &temp, Seed: &seed}}
		if native {
			req.Messages = []backends.Message{
				{Role: backends.RoleSystem, Content: "You are a helpful assistant. Use a tool only when the question needs one."},
				{Role: backends.RoleUser, Content: t.prompt}}
			req.Tools = ds
		} else {
			req.Messages = []backends.Message{
				{Role: backends.RoleSystem, Content: "You are a helpful assistant. Use a tool only when the question needs one.\n" + agent.ToolMenu(ds)},
				{Role: backends.RoleUser, Content: t.prompt}}
			req.Format = format
		}
		t0 := time.Now()
		stream, err := b.Chat(ctx, req)
		var r backends.Response
		if err == nil {
			r, err = backends.Collect(stream, nil)
		}
		total += time.Since(t0)
		s.Total++
		if err != nil {
			if ctx.Err() != nil {
				return s, ctx.Err()
			}
			var be *backends.Error
			if errors.As(err, &be) && (be.Status == 404 || be.Status == 503) {
				return s, err // the model or server is gone; the other failures just score as a miss
			}
			s.Errors++
			continue
		}
		var call *backends.ToolCall
		ok := true
		if native {
			if len(r.ToolCalls) > 0 {
				call = &r.ToolCalls[0]
			}
		} else {
			c, _, perr := agent.ParseAction(r.Content)
			if perr != nil {
				ok = false
			}
			call = c
		}
		if !ok {
			s.Errors++
			continue
		}
		if check(t, call) {
			s.Passed++
		}
	}
	s.Mean = total / time.Duration(s.Total)
	return s, nil
}

func check(t task, call *backends.ToolCall) bool {
	if t.tool == "" {
		return call == nil
	}
	if call == nil || call.Name != t.tool {
		return false
	}
	var args map[string]any
	if json.Unmarshal(call.Args, &args) != nil {
		return false
	}
	v, _ := args[t.arg].(string)
	return strings.Contains(strings.ToLower(v), t.want)
}
