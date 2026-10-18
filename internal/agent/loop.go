// Package agent runs the ReAct-style loop (FR-A1): the model reasons, calls one tool at a time,
// sees a trimmed result, and repeats until it answers or a budget ends the run. Every limit is
// enforced here, whatever the model does: step and token budgets, a per-step output cap, a
// wall-clock limit per step, repeat-call detection, and tools only from the selected skills.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"noskill/internal/backends"
)

// Tools are the tools offered in this run: only those of the selected skills (SECURITY.md T1).
// Call enforces permissions; its error is shown to the model as the tool's result.
type Tools interface {
	Defs() []backends.ToolDef
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

type Budget struct {
	MaxSteps          int                      // [agent] max_steps
	MaxTokens         int                      // [agent] max_tokens, prompt + completion over the whole run
	StepOutputTokens  int                      // num_predict per step (MODEL_ADAPTERS.md: 1024)
	StepTimeout       time.Duration            // wall clock per model step and per tool call
	ToolTimeouts      map[string]time.Duration // longer limits for tools that do a whole job (anime_run)
	ObservationTokens int                      // tool results are cut to this before re-entering context
	ContextTokens     int                      // the model's fixed num_ctx
}

// DefaultBudget is [agent] defaults for an 8k-context model.
func DefaultBudget() Budget {
	return Budget{MaxSteps: 25, MaxTokens: 60000, StepOutputTokens: 1024, StepTimeout: 120 * time.Second,
		ObservationTokens: 1500, ContextTokens: 8192,
		// One call of these runs a whole pipeline (feeds, downloads, copies of big files, indexing).
		// Cutting it off at two minutes stopped the first scheduled anime run half way.
		ToolTimeouts: map[string]time.Duration{"anime_run": 45 * time.Minute, "docs_index": 30 * time.Minute}}
}

// toolTimeout is how long a call of tool name may take.
func (b Budget) toolTimeout(name string) time.Duration {
	if d, ok := b.ToolTimeouts[name]; ok && d > b.StepTimeout {
		return d
	}
	return b.StepTimeout
}

type Request struct {
	RunID    string
	Model    string
	Messages []backends.Message
	Strategy backends.ToolStrategy // native (default) or constrained (ADR-0005)
	// CanConstrain: the backend supports JSON-schema output, so a malformed native call is
	// retried once in constrained mode.
	CanConstrain bool
	// Think is the client's choice for the final answer. Tool steps send think=false unless it's
	// true, since a thinking model can spend minutes on a one-line tool question (S2).
	Think *bool
	// AlwaysThinks: the model can't stop thinking (backends.CapThinkAlways). Every step then sends
	// think=true, so the reasoning arrives as thinking and the tool call or answer as content.
	AlwaysThinks bool
	// SmallModel: one tool call per step (MODEL_ADAPTERS.md "Other small-model tactics").
	SmallModel bool
	Budget     Budget
	Final      *backends.Options // client sampling options for the final answer
	// Pending and StartStep resume a parked run (FR-A6): Messages is the conversation as it was
	// when the run parked, ending with the assistant's tool calls; Pending are the calls from that
	// message still to run (the approved one first). They run before the model is asked again.
	Pending   []backends.ToolCall
	StartStep int
	// Handoff, when set, is asked before each later step whether another model should drive from
	// here (swap rule 8, ORCHESTRATOR.md): after a delegation swapped the driver out, the newly
	// loaded model finishes the task instead of swapping back. nil = keep the driver.
	Handoff func(ctx context.Context) *Driver
}

// Driver is a model taking over a run (Request.Handoff).
type Driver struct {
	Backend          backends.Backend
	Model            string
	Strategy         backends.ToolStrategy
	CanConstrain     bool
	AlwaysThinks     bool
	SmallModel       bool
	ContextTokens    int
	StepOutputTokens int
	Note             string // why, for the trace and the client's progress line
}

type StopReason string

const (
	StopAnswered  StopReason = "answered"
	StopMaxSteps  StopReason = "max_steps"
	StopMaxTokens StopReason = "max_tokens"
	StopCancelled StopReason = "cancelled"
	// StopWaiting: a tool needs an approval nobody has given. The run is parked (status
	// "waiting") and resumes when the approval is answered (FR-A6).
	StopWaiting StopReason = "waiting"
)

// Waiter is implemented by tool errors that mean "ask a human first" (tools.ErrWaiting).
type Waiter interface {
	error
	WaitingFor() string // the approval id
}

type Result struct {
	Answer   string
	Thinking string
	Stop     StopReason
	// WaitingFor is the approval id when Stop is StopWaiting, and Pending the calls of the last
	// assistant message that haven't run yet (the one waiting first). Messages + Pending is what a
	// resumed run needs (Request.Pending).
	WaitingFor string
	Pending    []backends.ToolCall
	Steps      int
	Usage      backends.Usage
	Messages   []backends.Message // the conversation as the model last saw it, plus the answer
	Model      string             // the model that drove the last step (differs after a handoff)
}

// Loop drives one model through one run. OnEvent (optional) receives progress for streaming
// and the trace (FR-A3, FR-A4).
type Loop struct {
	Backend backends.Backend
	Tools   Tools
	OnEvent func(Event)
}

const (
	noteActNow   = "Stop thinking and act now: call one tool, or give your answer in a few sentences."
	noteRepeat   = "You already called %s with these arguments in step %d. Use that result. Tools are now off: answer with what you have."
	noteMaxSteps = "Step limit reached. Answer now with what you have, and say what is missing."
	noteNoTool   = "error: tool %q is not available in this run. Available tools: %s."
)

func (l *Loop) emit(e Event) {
	if l.OnEvent != nil {
		e.Time = time.Now()
		l.OnEvent(e)
	}
}

func (l *Loop) Run(ctx context.Context, req Request) (Result, error) {
	b := req.Budget
	if b.MaxSteps <= 0 {
		b = DefaultBudget()
	}
	if req.Strategy == "" {
		req.Strategy = backends.StrategyNative
	}
	var defs []backends.ToolDef
	if l.Tools != nil {
		defs = l.Tools.Defs()
	}
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	res := Result{Model: req.Model}
	msgs := slices.Clone(req.Messages)
	seen := map[string]int{} // call key → step it ran in
	toolsOn := len(defs) > 0
	forceAnswer := ""

	// A resumed run first finishes the calls it parked on, then carries on from the next step.
	if len(req.Pending) > 0 {
		res.Steps = req.StartStep
		for i, c := range req.Pending {
			obs := l.callTool(ctx, req, b, req.StartStep, c, names, seen)
			if obs.waiting != "" {
				res.Stop, res.WaitingFor, res.Pending = StopWaiting, obs.waiting, slices.Clone(req.Pending[i:])
				l.emit(Event{Kind: EventBudget, Step: req.StartStep, Note: "waiting for approval " + obs.waiting})
				return l.finish(res, msgs), nil
			}
			if obs.toolsOff {
				toolsOn = false
			}
			msgs = append(msgs, toolResult(req.Strategy, c, obs.text))
		}
	}

	for step := req.StartStep + 1; ; step++ {
		if err := ctx.Err(); err != nil {
			res.Stop = StopCancelled
			return l.finish(res, msgs), err
		}
		if res.Usage.Total() >= b.MaxTokens {
			res.Stop = StopMaxTokens
			l.emit(Event{Kind: EventBudget, Step: step, Note: fmt.Sprintf("token budget %d used up", b.MaxTokens)})
			return l.finish(res, msgs), nil
		}
		if req.Handoff != nil && step > req.StartStep+1 {
			if d := req.Handoff(ctx); d != nil {
				l.Backend, req.Model, req.Strategy, req.CanConstrain = d.Backend, d.Model, d.Strategy, d.CanConstrain
				req.AlwaysThinks, req.SmallModel, res.Model = d.AlwaysThinks, d.SmallModel, d.Model
				if d.ContextTokens > 0 {
					b.ContextTokens = d.ContextTokens
				}
				if d.StepOutputTokens > 0 {
					b.StepOutputTokens = d.StepOutputTokens
				}
				l.emit(Event{Kind: EventProgress, Step: step, Model: d.Model, Note: d.Note})
			}
		}
		final := !toolsOn || step > b.MaxSteps
		if step > b.MaxSteps && forceAnswer == "" {
			forceAnswer = noteMaxSteps
			l.emit(Event{Kind: EventBudget, Step: step, Note: fmt.Sprintf("step limit %d reached", b.MaxSteps)})
		}
		stepMsgs := msgs
		if forceAnswer != "" {
			stepMsgs = append(slices.Clone(msgs), backends.Message{Role: backends.RoleUser, Content: forceAnswer})
		}
		stepMsgs = l.fit(step, stepMsgs, b.ContextTokens-b.StepOutputTokens)
		res.Steps = step

		strategy := req.Strategy
		resp, calls, err := l.step(ctx, req, b, step, stepMsgs, defs, final, strategy)
		if err == nil && !final && strategy == backends.StrategyNative && malformed(resp.ToolCalls) && req.CanConstrain {
			l.emit(Event{Kind: EventRetry, Step: step, Note: "malformed native tool call; retrying in constrained mode"})
			res.Usage = addUsage(res.Usage, resp.Usage)
			resp, calls, err = l.step(ctx, req, b, step, stepMsgs, defs, final, backends.StrategyConstrained)
		}
		res.Usage = addUsage(res.Usage, resp.Usage)
		if err == nil && len(calls) == 0 && strings.TrimSpace(resp.Content) == "" && resp.Thinking != "" {
			// The step's output ran out while the model was still thinking (a thinking-only 4B model
			// can spend 2k tokens on one step). Ask once more, telling it to act now.
			l.emit(Event{Kind: EventRetry, Step: step, Note: "the model used up the step thinking; asking it to act"})
			nudged := append(slices.Clone(stepMsgs), backends.Message{Role: backends.RoleUser, Content: noteActNow})
			resp, calls, err = l.step(ctx, req, b, step, nudged, defs, final, strategy)
			res.Usage = addUsage(res.Usage, resp.Usage)
		}
		resp.Thinking, resp.Content = splitThinking(resp.Thinking, resp.Content)
		if err != nil {
			if ctx.Err() != nil {
				res.Stop = StopCancelled
				return l.finish(res, msgs), ctx.Err()
			}
			return l.finish(res, msgs), err
		}
		if len(calls) == 0 && strings.TrimSpace(resp.Content) == "" && resp.Thinking != "" {
			// Still only thinking after the nudge. That's a stop, not an empty answer, so the CLI
			// says so and escalation (FR-O5) can try a stronger model.
			res.Stop, res.Thinking = StopMaxTokens, resp.Thinking
			l.emit(Event{Kind: EventBudget, Step: step, Note: "the model used up the step thinking again; no answer"})
			return l.finish(res, msgs), nil
		}
		if len(calls) == 0 || final {
			res.Answer, res.Thinking = resp.Content, resp.Thinking
			res.Stop = StopAnswered
			if step > b.MaxSteps {
				res.Stop = StopMaxSteps
			}
			msgs = append(msgs, backends.Message{Role: backends.RoleAssistant, Content: resp.Content})
			l.emit(Event{Kind: EventAnswer, Step: step, Content: resp.Content})
			return l.finish(res, msgs), nil
		}
		if req.SmallModel && len(calls) > 1 {
			calls = calls[:1]
		}
		for i := range calls {
			if calls[i].ID == "" {
				calls[i].ID = fmt.Sprintf("call_%s_%d_%d", req.RunID, step, i)
			}
		}
		msgs = append(msgs, backends.Message{Role: backends.RoleAssistant, Content: resp.Content, ToolCalls: calls})
		for i, c := range calls {
			obs := l.callTool(ctx, req, b, step, c, names, seen)
			if obs.waiting != "" {
				res.Stop, res.WaitingFor, res.Pending = StopWaiting, obs.waiting, slices.Clone(calls[i:])
				l.emit(Event{Kind: EventBudget, Step: step, Note: "waiting for approval " + obs.waiting})
				return l.finish(res, msgs), nil
			}
			if obs.toolsOff {
				toolsOn = false
			}
			msgs = append(msgs, toolResult(strategy, c, obs.text))
		}
	}
}

type observation struct {
	text     string
	toolsOff bool
	waiting  string // approval id when the tool needs a human
}

func (l *Loop) callTool(ctx context.Context, req Request, b Budget, step int, c backends.ToolCall, names []string, seen map[string]int) observation {
	l.emit(Event{Kind: EventToolCall, Step: step, Tool: c.Name, Args: c.Args})
	if !slices.Contains(names, c.Name) {
		// The model named a tool no selected skill owns: never run it (SECURITY.md T1 walkthrough).
		l.emit(Event{Kind: EventToolDenied, Step: step, Tool: c.Name, Note: "not offered in this run"})
		return observation{text: fmt.Sprintf(noteNoTool, c.Name, strings.Join(names, ", "))}
	}
	key := callKey(c)
	if prev, dup := seen[key]; dup {
		l.emit(Event{Kind: EventRepeat, Step: step, Tool: c.Name, Note: fmt.Sprintf("same call as step %d", prev)})
		return observation{text: fmt.Sprintf(noteRepeat, c.Name, prev), toolsOff: true}
	}
	seen[key] = step
	cctx, cancel := context.WithTimeout(ctx, b.toolTimeout(c.Name))
	defer cancel()
	t0 := time.Now()
	out, err := l.Tools.Call(cctx, c.Name, c.Args)
	var w Waiter
	if errors.As(err, &w) {
		l.emit(Event{Kind: EventToolResult, Step: step, Tool: c.Name, Err: err.Error(), Ms: time.Since(t0).Milliseconds()})
		return observation{waiting: w.WaitingFor()}
	}
	if err != nil {
		out = "error: " + err.Error()
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			out = fmt.Sprintf("error: %s took longer than %s and was stopped", c.Name, b.toolTimeout(c.Name))
		}
	}
	text, cut := truncate(out, b.ObservationTokens)
	l.emit(Event{Kind: EventToolResult, Step: step, Tool: c.Name, Content: text, Err: errString(err),
		Bytes: len(out), Truncated: cut, Ms: time.Since(t0).Milliseconds()})
	return observation{text: text}
}

// step runs one model call. Constrained mode asks for the next action as schema-checked JSON
// instead of native tool calls; final steps offer no tools at all.
func (l *Loop) step(ctx context.Context, req Request, b Budget, step int, msgs []backends.Message, defs []backends.ToolDef, final bool, strategy backends.ToolStrategy) (backends.Response, []backends.ToolCall, error) {
	sctx, cancel := context.WithTimeout(ctx, b.StepTimeout)
	defer cancel()
	cr := backends.ChatRequest{Model: req.Model, Messages: msgs, Options: backends.Options{NumCtx: b.ContextTokens, NumPredict: b.StepOutputTokens}}
	think := false
	cr.Think = &think
	if final {
		cr.Think = req.Think
		if req.Final != nil {
			o := *req.Final
			o.NumCtx = b.ContextTokens
			if o.NumPredict == 0 {
				o.NumPredict = b.StepOutputTokens
			}
			cr.Options = o
		}
	} else if strategy == backends.StrategyConstrained {
		schema, err := actionSchema(defs)
		if err != nil {
			return backends.Response{}, nil, err
		}
		cr.Format = schema
		cr.Messages = append(slices.Clone(msgs), backends.Message{Role: backends.RoleSystem, Content: toolMenu(defs)})
	} else {
		cr.Tools = defs
	}
	if req.AlwaysThinks {
		on := true
		cr.Think = &on
	}
	l.emit(Event{Kind: EventStep, Step: step, Model: req.Model, Note: string(strategy), Final: final})
	s, err := l.Backend.Chat(sctx, cr)
	if err != nil {
		return backends.Response{}, nil, err
	}
	resp, err := backends.Collect(s, func(c backends.ChatChunk) {
		if c.Content != "" || c.Thinking != "" {
			l.emit(Event{Kind: EventDelta, Step: step, Content: c.Content, Thinking: c.Thinking, Final: final})
		}
	})
	if err != nil {
		if errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return resp, nil, fmt.Errorf("model step %d took longer than %s", step, b.StepTimeout)
		}
		return resp, nil, err
	}
	if final {
		return resp, nil, nil
	}
	if strategy == backends.StrategyConstrained {
		call, answer, err := parseAction(resp.Content)
		if err != nil {
			l.emit(Event{Kind: EventRetry, Step: step, Note: "constrained output didn't parse: " + err.Error()})
			return resp, nil, nil // treated as an answer: content is shown as is
		}
		if call == nil {
			resp.Content = answer
			return resp, nil, nil
		}
		return resp, []backends.ToolCall{*call}, nil
	}
	return resp, resp.ToolCalls, nil
}

// splitThinking moves reasoning that leaked into the content out of it. With think=false, qwen3
// models still think on the step after a tool result, and Ollama then returns the reasoning as
// content ending in a bare "</think>" (seen live in the block 1c review). Everything up to the last
// "</think>" is thinking; if nothing follows it, the content is kept as is.
func splitThinking(thinking, content string) (string, string) {
	i := strings.LastIndex(content, "</think>")
	if i < 0 {
		return thinking, content
	}
	answer := strings.TrimSpace(content[i+len("</think>"):])
	if answer == "" {
		return thinking, content
	}
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(content[:i]), "<think>"))
	if thinking != "" && t != "" {
		t = thinking + "\n" + t
	} else if thinking != "" {
		t = thinking
	}
	return t, answer
}

func (l *Loop) finish(res Result, msgs []backends.Message) Result {
	res.Messages = msgs
	l.emit(Event{Kind: EventEnd, Step: res.Steps, Note: string(res.Stop), Tokens: res.Usage.Total()})
	return res
}

// toolResult formats a tool's output for the next step. Constrained mode has no native tool
// role in many chat templates, so results go back as a user message.
func toolResult(strategy backends.ToolStrategy, c backends.ToolCall, text string) backends.Message {
	if strategy == backends.StrategyConstrained {
		return backends.Message{Role: backends.RoleUser, Content: fmt.Sprintf("Result of %s:\n%s", c.Name, text)}
	}
	return backends.Message{Role: backends.RoleTool, Content: text, ToolCallID: c.ID, ToolName: c.Name}
}

// callKey identifies a call by tool name and canonical arguments (key order and spacing ignored).
func callKey(c backends.ToolCall) string {
	var v any
	if err := json.Unmarshal(c.Args, &v); err != nil {
		return c.Name + "\x00" + string(c.Args)
	}
	b, _ := json.Marshal(v) // maps marshal with sorted keys
	return c.Name + "\x00" + string(b)
}

func malformed(calls []backends.ToolCall) bool {
	for _, c := range calls {
		if c.Name == "" || (len(c.Args) > 0 && !json.Valid(c.Args)) {
			return true
		}
	}
	return false
}

func addUsage(a, b backends.Usage) backends.Usage {
	return backends.Usage{PromptTokens: a.PromptTokens + b.PromptTokens, CompletionTokens: a.CompletionTokens + b.CompletionTokens}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
