// S2 spike: a minimal noskill-style proxy on one port serving the OpenAI (/v1) and Ollama (/api) APIs.
//
//	go run ./cmd/proxy -listen 127.0.0.1:11435 -backend http://127.0.0.1:11436 -model mock-1
//
// Plain model names are passed straight through. "noskill/<anything>" runs a small agent loop with
// one server-side tool (get_time) and streams progress as reasoning (OpenAI: delta.reasoning_content,
// Ollama: message.thinking) before the answer. Checks Host and Origin, optional bearer token (NOSKILL_TOKEN).
// Docs: docs/PROXY.md. Results: docs/spikes/S2-results.md.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	backend      *url.URL
	backendModel string
	keepalive    time.Duration
	client       = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second}}
	token        = os.Getenv("NOSKILL_TOKEN")
	logf         *os.File
	logmu        sync.Mutex
	origins      = map[string]bool{}
	port         string
)

func main() {
	listen := flag.String("listen", "127.0.0.1:11435", "address")
	be := flag.String("backend", "http://127.0.0.1:11436", "backend base URL (Ollama or the mock)")
	flag.StringVar(&backendModel, "model", "mock-1", "backend model used for noskill/* names")
	flag.DurationVar(&keepalive, "keepalive", 10*time.Second, "keepalive interval while a stream is quiet")
	logPath := flag.String("log", "", "write a JSONL request log here")
	allow := flag.String("origins", "", "extra allowed Origin values, comma separated")
	flag.Parse()
	var err error
	if backend, err = url.Parse(*be); err != nil {
		panic(err)
	}
	for _, o := range strings.Split(*allow, ",") {
		if o != "" {
			origins[o] = true
		}
	}
	_, port, _ = net.SplitHostPort(*listen)
	if *logPath != "" {
		if logf, err = os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			panic(err)
		}
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend)
			pr.Out.Host = backend.Host
		},
		FlushInterval: -1,
		Transport:     client.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, r, 502, "backend unreachable: "+err.Error())
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", listModels)
	mux.HandleFunc("GET /api/tags", listModels)
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model, Name string }
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		json.Unmarshal(raw, &b)
		if strings.HasPrefix(b.Model, "noskill/") || strings.HasPrefix(b.Name, "noskill/") {
			json.NewEncoder(w).Encode(map[string]any{"details": map[string]any{"family": "noskill"}, "capabilities": []string{"completion", "tools"}, "model_info": map[string]any{"general.architecture": "noskill"}})
			return
		}
		rp.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(w, r, rp, true) })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { chat(w, r, rp, false) })
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) { chat(w, r, rp, false) })
	// The ollama CLI and many apps probe "/" (HEAD or GET) to see whether the server is up.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "noskill is running") })
	mux.HandleFunc("HEAD /{$}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.Handle("/", rp)
	fmt.Println("proxy on", *listen, "->", backend)
	srv := &http.Server{Addr: *listen, Handler: guard(mux)}
	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}

// ---------- security checks and logging ----------

type rec struct {
	http.ResponseWriter
	status int
}

func (r *rec) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *rec) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (r *rec) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			writeErr(w, r, 403, "Host not allowed")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !origins[o] {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				writeErr(w, r, 403, "Origin not allowed")
				return
			}
		}
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			writeErr(w, r, 401, "missing or wrong token")
			return
		}
		if r.Header.Get("X-Req-Id") == "" {
			r.Header.Set("X-Req-Id", fmt.Sprint(t0.UnixNano()))
		}
		var body []byte
		if logf != nil && r.Method == "POST" {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		rw := &rec{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		if logf != nil {
			size := len(body)
			if len(body) > 700 {
				body = body[:700]
			}
			b, _ := json.Marshal(map[string]any{"t": t0.Format(time.RFC3339Nano), "method": r.Method, "path": r.URL.Path, "status": rw.status, "ms": time.Since(t0).Milliseconds(),
				"ua": r.UserAgent(), "origin": r.Header.Get("Origin"), "accept": r.Header.Get("Accept"), "ctype": r.Header.Get("Content-Type"), "auth": r.Header.Get("Authorization") != "", "bytes": size, "body": string(body)})
			logmu.Lock()
			logf.Write(append(b, '\n'))
			logmu.Unlock()
		}
	})
}

func isOpenAI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1") }

func writeErr(w http.ResponseWriter, r *http.Request, code int, m string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if isOpenAI(r) {
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": m, "type": "noskill_error", "code": code}})
	} else {
		json.NewEncoder(w).Encode(map[string]any{"error": m})
	}
}

// ---------- model lists ----------

func listModels(w http.ResponseWriter, r *http.Request) {
	req, _ := http.NewRequestWithContext(r.Context(), "GET", backend.String()+r.URL.Path, nil)
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, r, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		writeErr(w, r, 502, "bad backend list")
		return
	}
	if isOpenAI(r) {
		d, _ := m["data"].([]any)
		m["data"] = append([]any{map[string]any{"id": "noskill/auto", "object": "model", "created": time.Now().Unix(), "owned_by": "noskill"}}, d...)
	} else {
		d, _ := m["models"].([]any)
		m["models"] = append([]any{map[string]any{"name": "noskill/auto", "model": "noskill/auto", "modified_at": time.Now().UTC().Format(time.RFC3339), "size": 0, "digest": synthDigest, "details": map[string]any{"family": "noskill"}}}, d...)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(m)
}

// ---------- chat ----------

func chat(w http.ResponseWriter, r *http.Request, rp *httputil.ReverseProxy, openai bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, r, 400, "unreadable body")
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		writeErr(w, r, 400, "bad json")
		return
	}
	model, _ := body["model"].(string)
	if !strings.HasPrefix(model, "noskill/") {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		rp.ServeHTTP(w, r)
		return
	}
	stream := !openai // OpenAI defaults to false, Ollama to true
	if s, ok := body["stream"].(bool); ok {
		stream = s
	}
	gen := r.URL.Path == "/api/generate"
	msgs, _ := body["messages"].([]any)
	if gen { // the prompt is one user message; "system" goes first. raw:true would pass through in the real proxy
		if sys, _ := body["system"].(string); sys != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
		if p, _ := body["prompt"].(string); p != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": p})
		}
	}
	if len(msgs) == 0 {
		writeErr(w, r, 400, "messages is required")
		return
	}
	runID := fmt.Sprintf("run_%d", time.Now().UnixNano())
	w.Header().Set("X-Noskill-Run-Id", runID)
	var out sink
	switch {
	case !stream && openai:
		out = &bufSink{w: w, openai: true, model: model}
	case !stream:
		out = &bufSink{w: w, model: model, gen: gen}
	case openai:
		out = newStream(w, true, model)
	default:
		st := newStream(w, false, model)
		st.gen = gen
		out = st
	}
	agent(r.Context(), r.Header.Get("X-Req-Id"), msgs, clientOptions(body), out)
	out.finish()
}

type sink interface {
	reasoning(string)
	content(string)
	ping()
	fail(string)
	finish()
}

// Clients slice the digest (the ollama CLI uses digest[:12]), so it has to look like a real one.
const synthDigest = "6e6f736b696c6c0000000000000000000000000000000000000000000000abcd"

var getTime = map[string]any{"type": "function", "function": map[string]any{"name": "get_time", "description": "Current local date and time", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}

type call struct{ id, name, args string }

// clientOptions keeps the client's generation options for the backend: OpenAI reasoning_effort,
// max_tokens and temperature, and Ollama's think:false and options.num_predict / temperature.
func clientOptions(body map[string]any) map[string]any {
	o := map[string]any{}
	for _, k := range []string{"reasoning_effort", "max_tokens", "temperature", "top_p", "seed", "stop"} {
		if v, ok := body[k]; ok {
			o[k] = v
		}
	}
	if th, ok := body["think"].(bool); ok && !th {
		o["reasoning_effort"] = "none"
	}
	if op, ok := body["options"].(map[string]any); ok {
		if v, ok := op["num_predict"]; ok {
			o["max_tokens"] = v
		}
		for _, k := range []string{"temperature", "top_p", "seed"} {
			if v, ok := op[k]; ok {
				o[k] = v
			}
		}
	}
	return o
}

func agent(ctx context.Context, reqID string, msgs []any, opts map[string]any, out sink) {
	stop := make(chan struct{})
	defer close(stop)
	go func() { // keepalive while a step is quiet
		t := time.NewTicker(keepalive)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				out.ping()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	for step := 0; step < 4; step++ {
		reqBody := map[string]any{"model": backendModel, "messages": msgs, "stream": true, "tools": []any{getTime}}
		for k, v := range opts {
			reqBody[k] = v
		}
		b, _ := json.Marshal(reqBody)
		req, _ := http.NewRequestWithContext(ctx, "POST", backend.String()+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Req-Id", reqID)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				out.fail("backend error: " + err.Error())
			}
			return
		}
		if resp.StatusCode != 200 {
			bb, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			out.fail(fmt.Sprintf("backend %d: %s", resp.StatusCode, bb))
			return
		}
		calls := map[int]*call{}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			var ch struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						Reasoning string `json:"reasoning_content"`
						Reason2   string `json:"reasoning"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Function struct{ Name, Arguments string }
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(line[6:]), &ch) != nil || len(ch.Choices) == 0 {
				continue
			}
			d := ch.Choices[0].Delta
			if d.Reasoning != "" {
				out.reasoning(d.Reasoning)
			} else if d.Reason2 != "" {
				out.reasoning(d.Reason2)
			}
			if d.Content != "" {
				out.content(d.Content)
			}
			for _, tc := range d.ToolCalls {
				c := calls[tc.Index]
				if c == nil {
					c = &call{}
					calls[tc.Index] = c
				}
				if tc.ID != "" {
					c.id = tc.ID
				}
				c.name += tc.Function.Name
				c.args += tc.Function.Arguments
			}
		}
		resp.Body.Close()
		if len(calls) == 0 || ctx.Err() != nil {
			return
		}
		var tcs []any
		var results []any
		for i := 0; i < len(calls); i++ {
			c := calls[i]
			if c == nil {
				continue
			}
			out.reasoning("Calling " + c.name + "…\n")
			result := "unknown tool"
			if c.name == "get_time" {
				result = time.Now().Format("2006-01-02 15:04:05 MST")
			}
			if c.id == "" {
				c.id = fmt.Sprintf("call_nsk_%d", i)
			}
			args := c.args
			if args == "" {
				args = "{}"
			}
			tcs = append(tcs, map[string]any{"id": c.id, "type": "function", "function": map[string]any{"name": c.name, "arguments": args}})
			results = append(results, map[string]any{"role": "tool", "tool_call_id": c.id, "content": result})
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "", "tool_calls": tcs})
		msgs = append(msgs, results...)
	}
	out.fail("too many tool steps")
}

// ---------- output formats ----------

type stream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	fl      http.Flusher
	openai  bool
	model   string
	id      string
	started bool
	gen     bool // /api/generate shape: "response" instead of "message.content"
}

// genShape turns an /api/chat object into the /api/generate shape.
func genShape(o map[string]any) map[string]any {
	if m, ok := o["message"].(map[string]any); ok {
		o["response"] = m["content"]
		if t, ok := m["thinking"]; ok {
			o["thinking"] = t
		}
		delete(o, "message")
	}
	return o
}

func newStream(w http.ResponseWriter, openai bool, model string) *stream {
	s := &stream{w: w, fl: w.(http.Flusher), openai: openai, model: model, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())}
	if openai {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	w.WriteHeader(200)
	if openai {
		s.sse(map[string]any{"role": "assistant", "content": ""}, nil)
	}
	return s
}

func (s *stream) sse(delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.fl.Flush()
}

func (s *stream) nd(content, thinking string, done bool, extra map[string]any) {
	m := map[string]any{"role": "assistant", "content": content}
	if thinking != "" {
		m["thinking"] = thinking
	}
	o := map[string]any{"model": s.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": m, "done": done}
	for k, v := range extra {
		o[k] = v
	}
	if s.gen {
		o = genShape(o)
	}
	b, _ := json.Marshal(o)
	fmt.Fprintf(s.w, "%s\n", b)
	s.fl.Flush()
}

func (s *stream) reasoning(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openai {
		s.sse(map[string]any{"reasoning_content": t}, nil)
	} else {
		s.nd("", t, false, nil)
	}
}
func (s *stream) content(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openai {
		s.sse(map[string]any{"content": t}, nil)
	} else {
		s.nd(t, "", false, nil)
	}
}
func (s *stream) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openai {
		fmt.Fprint(s.w, ": keepalive\n\n")
		s.fl.Flush()
	} else {
		s.nd("", "", false, nil)
	}
}
func (s *stream) fail(m string) { s.content("\n[noskill error: " + m + "]") }
func (s *stream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.openai {
		s.sse(map[string]any{}, "stop")
		fmt.Fprint(s.w, "data: [DONE]\n\n")
		s.fl.Flush()
	} else {
		s.nd("", "", true, map[string]any{"done_reason": "stop", "total_duration": 1, "eval_count": 1})
	}
}

type bufSink struct {
	w      http.ResponseWriter
	openai bool
	model  string
	text   strings.Builder
	think  strings.Builder
	gen    bool
}

func (b *bufSink) reasoning(t string) { b.think.WriteString(t) }
func (b *bufSink) content(t string)   { b.text.WriteString(t) }
func (b *bufSink) ping()              {}
func (b *bufSink) fail(m string)      { b.text.WriteString("\n[noskill error: " + m + "]") }
func (b *bufSink) finish() {
	b.w.Header().Set("Content-Type", "application/json")
	if b.openai {
		msg := map[string]any{"role": "assistant", "content": b.text.String()}
		if b.think.Len() > 0 {
			msg["reasoning_content"] = b.think.String()
		}
		json.NewEncoder(b.w).Encode(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "created": time.Now().Unix(), "model": b.model,
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
		return
	}
	msg := map[string]any{"role": "assistant", "content": b.text.String()}
	if b.think.Len() > 0 {
		msg["thinking"] = b.think.String()
	}
	o := map[string]any{"model": b.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "message": msg, "done": true, "done_reason": "stop", "total_duration": 1, "eval_count": 1}
	if b.gen {
		o = genShape(o)
	}
	json.NewEncoder(b.w).Encode(o)
}
