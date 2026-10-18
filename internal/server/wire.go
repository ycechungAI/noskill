package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"noskill/internal/agent"
	"noskill/internal/app"
)

type wireKind int

const (
	wireOpenAI wireKind = iota
	wireOllamaChat
	wireOllamaGenerate
)

// responder writes one chat response in the client's format, streamed or whole (PROXY.md
// "Streaming"). It is safe for concurrent use: the run writes, a ticker sends keepalives.
type responder struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	kind         wireKind
	stream       bool
	model        string // the name the client used
	id           string
	created      time.Time
	progressMode string // thinking | think_tags | content | off
	includeUsage bool
	extra        http.Header

	mu          sync.Mutex
	began       bool
	content     strings.Builder // non-streaming accumulation
	thinking    strings.Builder
	thinkOpen   bool // think_tags: a <think> block is open
	progressed  bool // content mode: progress lines were written
	answerBegan bool
	lastWrite   time.Time
}

func newResponder(w http.ResponseWriter, kind wireKind, stream bool, model, progress string) *responder {
	if progress == "" {
		progress = "thinking"
	}
	return &responder{w: w, rc: http.NewResponseController(w), kind: kind, stream: stream, model: model, progressMode: progress,
		created: time.Now(), extra: http.Header{}, id: "chatcmpl-noskill"}
}

// SetHeader adds a response header; it only takes effect before the first byte is written.
func (r *responder) SetHeader(k, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.began {
		r.extra.Set(k, v)
	}
}

func (r *responder) SetRunID(id string) {
	r.mu.Lock()
	r.id = "chatcmpl-" + id
	r.mu.Unlock()
	r.SetHeader("X-Noskill-Run-Id", id)
}

// Started reports whether any byte has been sent (after that an error can't change the status).
func (r *responder) Started() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.began
}

func (r *responder) begin() {
	if r.began {
		return
	}
	r.began = true
	h := r.w.Header()
	for k, vs := range r.extra {
		h[k] = vs
	}
	if r.stream {
		if r.kind == wireOpenAI {
			h.Set("Content-Type", "text/event-stream")
		} else {
			h.Set("Content-Type", "application/x-ndjson")
		}
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		r.w.WriteHeader(http.StatusOK)
		if r.kind == wireOpenAI {
			r.writeOpenAIChunk(map[string]any{"role": "assistant"}, nil)
		}
	}
}

func (r *responder) flush() {
	r.lastWrite = time.Now()
	r.rc.Flush()
}

// ---- inputs from the run ----

// Progress shows a status line ("Searching…") the way server.progress asks for.
func (r *responder) Progress(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.progressMode {
	case "off":
		return
	case "thinking":
		r.emit("", line+"\n", "")
	case "think_tags":
		if !r.thinkOpen {
			r.thinkOpen = true
			r.emit("<think>\n", "", "")
		}
		r.emit(line+"\n", "", "")
	case "content":
		r.progressed = true
		r.emit(line+"\n", "", "")
	}
}

// Thinking is the model's own reasoning for the final answer.
func (r *responder) Thinking(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.progressMode == "think_tags" || r.progressMode == "content" {
		if !r.thinkOpen {
			r.thinkOpen = true
			r.emit("<think>\n", "", "")
		}
		r.emit(s, "", "")
		return
	}
	r.emit("", s, "")
}

// Content is part of the answer.
func (r *responder) Content(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer(s)
}

func (r *responder) answer(s string) {
	if !r.answerBegan {
		r.answerBegan = true
		if r.thinkOpen {
			r.thinkOpen = false
			r.emit("\n</think>\n", "", "")
		} else if r.progressed {
			r.emit("\n", "", "")
		}
	}
	r.emit(s, "", "")
}

func (r *responder) Keepalive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := r.lastWrite
	if base.IsZero() {
		base = r.created // still waiting for the first byte (queueing, loading a model)
	}
	if !r.stream || time.Since(base) < 9*time.Second {
		return
	}
	r.begin()
	switch r.kind {
	case wireOpenAI:
		fmt.Fprint(r.w, ": keepalive\n\n")
	case wireOllamaChat:
		r.writeLine(map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"message": map[string]any{"role": "assistant", "content": ""}, "done": false})
	case wireOllamaGenerate:
		r.writeLine(map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "response": "", "done": false})
	}
	r.flush()
}

// emit sends content (and optionally thinking) now (streaming) or keeps it (whole response). The
// first argument is content that belongs inside the content channel as progress text.
func (r *responder) emit(content, thinking, _ string) {
	if content == "" && thinking == "" {
		return
	}
	if !r.stream {
		r.content.WriteString(content)
		r.thinking.WriteString(thinking)
		return
	}
	r.begin()
	switch r.kind {
	case wireOpenAI:
		d := map[string]any{}
		if content != "" {
			d["content"] = content
		}
		if thinking != "" {
			d["reasoning_content"] = thinking
		}
		r.writeOpenAIChunk(d, nil)
	case wireOllamaChat:
		m := map[string]any{"role": "assistant", "content": content}
		if thinking != "" {
			m["thinking"] = thinking
		}
		r.writeLine(map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": m, "done": false})
	case wireOllamaGenerate:
		l := map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "response": content, "done": false}
		if thinking != "" {
			l["thinking"] = thinking
		}
		r.writeLine(l)
	}
	r.flush()
}

func (r *responder) writeLine(v any) {
	b, _ := json.Marshal(v)
	r.w.Write(append(b, '\n'))
}

func (r *responder) writeOpenAIChunk(delta map[string]any, finish *string) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	b, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created.Unix(), "model": r.model,
		"choices": []any{choice}})
	fmt.Fprintf(r.w, "data: %s\n\n", b)
}

// ---- end ----

func finishReason(res *app.RunResult) string {
	if res != nil && (res.Stop == agent.StopMaxSteps || res.Stop == agent.StopMaxTokens) {
		return "length"
	}
	return "stop"
}

// Finish ends the response. res may be nil when the run produced nothing.
func (r *responder) Finish(res *app.RunResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.thinkOpen { // progress only, no answer
		r.thinkOpen = false
		r.emit("\n</think>\n", "", "")
	}
	reason := finishReason(res)
	var usage struct{ prompt, completion int }
	var model string
	if res != nil {
		usage.prompt, usage.completion = res.Usage.PromptTokens, res.Usage.CompletionTokens
		model = res.Model
	}
	_ = model
	r.begin()
	dur := time.Since(r.created).Nanoseconds()
	switch {
	case r.kind == wireOpenAI && r.stream:
		r.writeOpenAIChunk(map[string]any{}, &reason)
		if r.includeUsage {
			b, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created.Unix(), "model": r.model,
				"choices": []any{}, "usage": map[string]int{"prompt_tokens": usage.prompt, "completion_tokens": usage.completion, "total_tokens": usage.prompt + usage.completion}})
			fmt.Fprintf(r.w, "data: %s\n\n", b)
		}
		fmt.Fprint(r.w, "data: [DONE]\n\n")
	case r.kind == wireOpenAI:
		msg := map[string]any{"role": "assistant", "content": r.content.String()}
		if r.thinking.Len() > 0 {
			msg["reasoning_content"] = r.thinking.String()
		}
		r.w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(r.w).Encode(map[string]any{"id": r.id, "object": "chat.completion", "created": r.created.Unix(), "model": r.model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": reason}},
			"usage":   map[string]int{"prompt_tokens": usage.prompt, "completion_tokens": usage.completion, "total_tokens": usage.prompt + usage.completion}})
	case r.kind == wireOllamaChat:
		done := map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "done": true, "done_reason": reason,
			"total_duration": dur, "load_duration": 0, "prompt_eval_count": usage.prompt, "eval_count": usage.completion, "eval_duration": dur}
		if r.stream {
			done["message"] = map[string]any{"role": "assistant", "content": ""}
			r.writeLine(done)
		} else {
			m := map[string]any{"role": "assistant", "content": r.content.String()}
			if r.thinking.Len() > 0 {
				m["thinking"] = r.thinking.String()
			}
			done["message"] = m
			r.w.Header().Set("Content-Type", "application/json")
			r.writeLine(done)
		}
	case r.kind == wireOllamaGenerate:
		done := map[string]any{"model": r.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "done": true, "done_reason": reason,
			"total_duration": dur, "load_duration": 0, "prompt_eval_count": usage.prompt, "eval_count": usage.completion, "eval_duration": dur, "context": []int{}}
		if r.stream {
			done["response"] = ""
			r.writeLine(done)
		} else {
			done["response"] = r.content.String()
			if r.thinking.Len() > 0 {
				done["thinking"] = r.thinking.String()
			}
			r.w.Header().Set("Content-Type", "application/json")
			r.writeLine(done)
		}
	}
	r.flush()
}

// FailMidStream reports an error after the status line is gone, in the client's own format.
func (r *responder) FailMidStream(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kind == wireOpenAI {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "api_error"}})
		fmt.Fprintf(r.w, "data: %s\n\ndata: [DONE]\n\n", b)
	} else {
		r.writeLine(map[string]string{"error": msg})
	}
	r.flush()
}
