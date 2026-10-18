// Mock model backend for S2: speaks the Ollama and OpenAI formats and streams fixed tokens,
// so model speed doesn't hide the proxy's overhead.
//
//	go run ./cmd/mock -listen 127.0.0.1:11436
//
// Prompt keywords change the reply: "what time" -> get_time tool call (when tools are sent),
// "long" -> 5000 tokens, "think" -> reasoning tokens first, "slow" -> 20 ms per token, "quiet" -> 3 s of silence first.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type msg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []map[string]any `json:"tool_calls,omitempty"`
}

type chatReq struct {
	Prompt   string           `json:"prompt"`
	Model    string           `json:"model"`
	Messages []msg            `json:"messages"`
	Stream   *bool            `json:"stream"`
	Tools    []map[string]any `json:"tools"`
}

var (
	mu      sync.Mutex
	cancels = map[string]int64{} // X-Req-Id -> unix nanos when the mock saw the client go away
	tokens  = map[string]int{}   // X-Req-Id -> tokens sent before that
)

func lastUser(m []msg) string {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i].Role == "user" {
			return strings.ToLower(m[i].Content)
		}
	}
	return ""
}

func hasToolResult(m []msg) bool {
	for _, x := range m {
		if x.Role == "tool" {
			return true
		}
	}
	return false
}

func main() {
	listen := flag.String("listen", "127.0.0.1:11436", "address")
	flag.Parse()
	mux := http.NewServeMux()
	models := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1") {
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{{"id": "mock-1", "object": "model", "created": 1, "owned_by": "mock"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "mock-1", "model": "mock-1", "size": 1, "digest": "6d6f636b0000000000000000000000000000000000000000000000000000abcd", "details": map[string]any{"family": "mock", "parameter_size": "1B", "quantization_level": "Q4"}}}})
	}
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("GET /api/tags", models)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":"0.40.3"}`) })
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[]}`) })
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"details":{"family":"mock"},"capabilities":["completion","tools"],"model_info":{"general.architecture":"mock"}}`)
	})
	mux.HandleFunc("GET /_cancel/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := r.PathValue("id")
		json.NewEncoder(w).Encode(map[string]any{"cancelled_at": cancels[id], "tokens": tokens[id]})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(w, r, true) })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { chat(w, r, false) })
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) { chat(w, r, false) })
	fmt.Println("mock backend on", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		panic(err)
	}
}

func chat(w http.ResponseWriter, r *http.Request, openai bool) {
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, 400)
		return
	}
	gen := strings.HasSuffix(r.URL.Path, "/generate")
	if gen && len(req.Messages) == 0 {
		req.Messages = []msg{{Role: "user", Content: req.Prompt}}
	}
	enc := func(o map[string]any) []byte { // /api/generate uses "response" where /api/chat uses "message.content"
		if gen {
			if m, ok := o["message"].(map[string]any); ok {
				o["response"] = m["content"]
				if t, ok := m["thinking"]; ok {
					o["thinking"] = t
				}
				delete(o, "message")
			}
		}
		b, _ := json.Marshal(o)
		return b
	}
	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	} else if openai {
		stream = false
	}
	u := lastUser(req.Messages)
	id := r.Header.Get("X-Req-Id")
	n, delay := 40, time.Duration(0)
	if strings.Contains(u, "long") {
		n = 5000
	}
	if strings.Contains(u, "slow") {
		delay = 20 * time.Millisecond
	}
	if strings.Contains(u, "quiet") {
		time.Sleep(3 * time.Second) // a silent gap, to see the proxy's keepalives
	}
	think := strings.Contains(u, "think")
	wantTool := len(req.Tools) > 0 && strings.Contains(u, "what time") && !hasToolResult(req.Messages)
	if hasToolResult(req.Messages) {
		n = 12
	}

	if !stream {
		if wantTool {
			call := []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_time", "arguments": "{}"}}}
			if openai {
				json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": req.Model, "choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "", "tool_calls": call}}}})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "message": map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"function": map[string]any{"name": "get_time", "arguments": map[string]any{}}}}}, "done": true, "done_reason": "stop"})
			}
			return
		}
		text := strings.TrimSpace(strings.Repeat("tok ", n))
		if openai {
			json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": req.Model, "choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}}, "usage": map[string]int{"prompt_tokens": 5, "completion_tokens": n, "total_tokens": n + 5}})
		} else {
			w.Write(enc(map[string]any{"model": req.Model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "content": text}, "done": true, "done_reason": "stop", "eval_count": n}))
		}
		return
	}

	fl := w.(http.Flusher)
	if openai {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.WriteHeader(200)
	send := func(content, reasoning string, tool []map[string]any, finish string) bool {
		if openai {
			d := map[string]any{}
			if content != "" {
				d["content"] = content
			}
			if reasoning != "" {
				d["reasoning_content"] = reasoning
			}
			if tool != nil {
				d["tool_calls"] = tool
			}
			var fr any
			if finish != "" {
				fr = finish
			}
			b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": req.Model, "choices": []map[string]any{{"index": 0, "delta": d, "finish_reason": fr}}})
			_, err := fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
			return err == nil
		}
		m := map[string]any{"role": "assistant", "content": content}
		if reasoning != "" {
			m["thinking"] = reasoning
		}
		b := enc(map[string]any{"model": req.Model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": m, "done": false})
		_, err := fmt.Fprintf(w, "%s\n", b)
		fl.Flush()
		return err == nil
	}
	sent := 0
	gone := func() bool {
		select {
		case <-r.Context().Done():
			fmt.Println("cancelled", id, "after", sent, "tokens at", time.Now().Format("15:04:05.000"))
			if id != "" {
				mu.Lock()
				cancels[id] = time.Now().UnixNano()
				tokens[id] = sent
				mu.Unlock()
			}
			return true
		default:
			return false
		}
	}
	if wantTool {
		call := []map[string]any{{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "get_time", "arguments": "{}"}}}
		send("", "", call, "")
		send("", "", nil, "tool_calls")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		return
	}
	if think {
		for i := 0; i < 8; i++ {
			send("", "hmm ", nil, "")
		}
	}
	for i := 0; i < n; i++ {
		if gone() {
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if !send("tok ", "", nil, "") {
			gone()
			return
		}
		sent++
	}
	if openai {
		send("", "", nil, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
	} else {
		b := enc(map[string]any{"model": req.Model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop", "total_duration": 1, "eval_count": n})
		fmt.Fprintf(w, "%s\n", b)
	}
	fl.Flush()
}
