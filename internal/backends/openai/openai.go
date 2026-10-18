// Package openai is the adapter for OpenAI-compatible servers: LM Studio, llama.cpp's
// llama-server, vLLM, Jan, LocalAI (MODEL_ADAPTERS.md "Backends", FR-B1).
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"noskill/internal/backends"
	"noskill/internal/ids"
)

func init() {
	backends.Register("openai", func(url, apiKey string) (backends.Backend, error) { return New(url, apiKey), nil })
}

// Backend talks to one OpenAI-compatible server. URL is the API root as configured, usually
// ending in /v1 (http://127.0.0.1:1234/v1 for LM Studio).
type Backend struct {
	root   string // …/v1
	origin string // … without /v1, for vendor endpoints (LM Studio's /api/v0)
	apiKey string
	hc     *http.Client
	o      Dialect
}

// Dialect is what sets one OpenAI-compatible server apart. The mlx and vllm adapters (FR-B5) are
// this client with their own dialect.
type Dialect struct {
	Kind       string // adapter kind and model ID prefix; "" = "openai"
	DefaultURL string // used when the config has no url
	NoSchema   bool   // the server ignores response_format json_schema, so don't claim CapSchema
	NoLogprobs bool   // the server's logprobs field isn't OpenAI's boolean, so don't send it
	PlainList  bool   // skip LM Studio's /api/v0/models
}

func New(url, apiKey string) *Backend { return NewDialect(Dialect{}, url, apiKey) }

func NewDialect(o Dialect, url, apiKey string) *Backend {
	if o.Kind == "" {
		o.Kind = "openai"
	}
	if o.DefaultURL == "" {
		o.DefaultURL = "http://127.0.0.1:1234/v1"
	}
	if url == "" {
		url = o.DefaultURL
	}
	root := strings.TrimRight(url, "/")
	if !strings.HasSuffix(root, "/v1") {
		root += "/v1"
	}
	return &Backend{root: root, origin: strings.TrimSuffix(root, "/v1"), apiKey: apiKey, hc: &http.Client{}, o: o}
}

func (b *Backend) Name() string { return b.o.Kind }

// Root is the API root (…/v1); Origin is it without /v1.
func (b *Backend) Root() string   { return b.root }
func (b *Backend) Origin() string { return b.origin }

// GetJSON fetches url (under Root or Origin) into out, for adapters built on this client.
func (b *Backend) GetJSON(ctx context.Context, url string, out any) error {
	return b.getJSON(ctx, url, out)
}

// ChatCaps are the capabilities of a chat model on this server before the tool probe runs.
func (b *Backend) ChatCaps() []backends.Capability {
	caps := []backends.Capability{backends.CapChat}
	if !b.o.NoSchema {
		caps = append(caps, backends.CapSchema)
	}
	return caps
}

func (b *Backend) do(ctx context.Context, method, url string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := err.Error()
		var op *net.OpError
		if errors.As(err, &op) && op.Err != nil {
			msg = "can't reach " + op.Addr.String() + ": " + op.Err.Error()
		}
		return nil, &backends.Error{Backend: b.o.Kind, Status: http.StatusServiceUnavailable, Msg: msg}
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil {
			switch v := e.Error.(type) {
			case string:
				msg = v
			case map[string]any:
				if m, ok := v["message"].(string); ok {
					msg = m
				}
			}
		}
		status := resp.StatusCode
		if status >= 500 {
			status = http.StatusBadGateway
		}
		return nil, &backends.Error{Backend: b.o.Kind, Status: status, Msg: msg}
	}
	return resp, nil
}

func (b *Backend) getJSON(ctx context.Context, url string, out any) error {
	resp, err := b.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- models ----

func (b *Backend) ListModels(ctx context.Context) ([]backends.ModelInfo, error) {
	// LM Studio describes its models (type, context, tool use) at /api/v0/models; everything else
	// only has the plain list.
	var lm struct {
		Data []struct {
			ID            string   `json:"id"`
			Type          string   `json:"type"` // llm | vlm | embeddings
			Arch          string   `json:"arch"`
			Quant         string   `json:"quantization"`
			MaxContext    int      `json:"max_context_length"`
			Capabilities  []string `json:"capabilities"`
			Compatibility string   `json:"compatibility_type"`
		} `json:"data"`
	}
	if b.o.PlainList {
		// adapters with their own listing don't ask LM Studio's endpoint
	} else if err := b.getJSON(ctx, b.origin+"/api/v0/models", &lm); err == nil && len(lm.Data) > 0 {
		var out []backends.ModelInfo
		for _, m := range lm.Data {
			mi := backends.ModelInfo{ID: b.o.Kind + "/" + m.ID, Backend: b.o.Kind, Name: m.ID, Family: m.Arch, Quant: m.Quant, Context: m.MaxContext}
			switch m.Type {
			case "embeddings":
				mi.Capabilities = []backends.Capability{backends.CapEmbed}
			case "vlm":
				mi.Capabilities = []backends.Capability{backends.CapChat, backends.CapSchema, backends.CapVision}
			default:
				mi.Capabilities = []backends.Capability{backends.CapChat, backends.CapSchema}
			}
			for _, c := range m.Capabilities {
				if c == "tool_use" {
					mi.Capabilities = append(mi.Capabilities, backends.CapTools)
				}
			}
			out = append(out, mi)
		}
		return out, nil
	}
	var l struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.getJSON(ctx, b.root+"/models", &l); err != nil {
		return nil, err
	}
	out := make([]backends.ModelInfo, 0, len(l.Data))
	for _, m := range l.Data {
		caps := b.ChatCaps()
		if strings.Contains(strings.ToLower(m.ID), "embed") {
			caps = []backends.Capability{backends.CapEmbed}
		} else {
			// The plain list doesn't say; native tool calling is probed per model at `init`
			// (ADR-0005), so offer it and let the probe decide.
			caps = append(caps, backends.CapTools)
		}
		out = append(out, backends.ModelInfo{ID: b.o.Kind + "/" + m.ID, Backend: b.o.Kind, Name: m.ID, Capabilities: caps})
	}
	return out, nil
}

// Load does nothing: these servers load models on demand (or the user loads them in the server's
// own UI), and there's no residency control. Loaded returns nothing for the same reason.
func (b *Backend) Load(context.Context, string, time.Duration) error { return nil }

func (b *Backend) Loaded(context.Context) ([]backends.LoadedModel, error) { return nil, nil }

func (b *Backend) Embed(ctx context.Context, req backends.EmbedRequest) ([][]float32, error) {
	resp, err := b.do(ctx, http.MethodPost, b.root+"/embeddings", map[string]any{"model": req.Model, "input": req.Input})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if len(r.Data) != len(req.Input) {
		return nil, &backends.Error{Backend: b.o.Kind, Status: http.StatusBadGateway, Msg: fmt.Sprintf("asked for %d embeddings, got %d", len(req.Input), len(r.Data))}
	}
	out := make([][]float32, len(r.Data))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, &backends.Error{Backend: b.o.Kind, Status: http.StatusBadGateway, Msg: "bad embedding index"}
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// ---- chat ----

func toWire(msgs []backends.Message) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		w := map[string]any{"role": string(m.Role)}
		switch {
		case len(m.Images) > 0:
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
					"url": "data:" + http.DetectContentType(img) + ";base64," + base64.StdEncoding.EncodeToString(img)}})
			}
			w["content"] = parts
		default:
			w["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				args := string(c.Args)
				if args == "" {
					args = "{}"
				}
				calls[j] = map[string]any{"id": c.ID, "type": "function", "function": map[string]string{"name": c.Name, "arguments": args}}
			}
			w["tool_calls"] = calls
			if m.Content == "" {
				w["content"] = nil
			}
		}
		if m.Role == backends.RoleTool {
			w["tool_call_id"] = m.ToolCallID
		}
		out[i] = w
	}
	return out
}

func (b *Backend) chatBody(req backends.ChatRequest) map[string]any {
	body := map[string]any{
		"model": req.Model, "messages": toWire(req.Messages), "stream": true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}}
		}
		body["tools"] = tools
	}
	if len(req.Format) > 0 && !b.o.NoSchema {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "strict": true, "schema": req.Format}}
	}
	if req.Think != nil && !*req.Think {
		body["reasoning_effort"] = "none" // ignored by servers that don't know it
	}
	o := req.Options
	if o.NumPredict > 0 {
		body["max_tokens"] = o.NumPredict
	}
	if o.Temperature != nil {
		body["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		body["top_p"] = *o.TopP
	}
	if o.Seed != nil {
		body["seed"] = *o.Seed
	}
	if len(o.Stop) > 0 {
		body["stop"] = o.Stop
	}
	if o.Logprobs && !b.o.NoLogprobs {
		body["logprobs"] = true
		body["top_logprobs"] = 5
	}
	// NumCtx is a server-side setting here (the user loads the model with a context size); it isn't
	// sent per request.
	return body
}

type chunkWire struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Logprobs *struct {
			Content []struct {
				Token       string  `json:"token"`
				Logprob     float64 `json:"logprob"`
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type pendingCall struct {
	id, name string
	args     strings.Builder
}

type stream struct {
	kind    string
	body    io.ReadCloser
	sc      *bufio.Scanner
	calls   map[int]*pendingCall
	order   []int
	finish  bool
	usage   backends.Usage
	sawDone bool
	done    bool
}

func (b *Backend) Chat(ctx context.Context, req backends.ChatRequest) (backends.ChatStream, error) {
	if len(req.Format) > 0 && b.o.NoSchema {
		return b.promptedJSON(ctx, req)
	}
	resp, err := b.do(ctx, http.MethodPost, b.root+"/chat/completions", b.chatBody(req))
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &stream{kind: b.o.Kind, body: resp.Body, sc: sc, calls: map[int]*pendingCall{}}, nil
}

// Recv returns content as it streams. Tool-call fragments are held back and returned whole in the
// final chunk, because arguments arrive as pieces of a JSON string.
func (s *stream) Recv() (backends.ChatChunk, error) {
	if s.done {
		return backends.ChatChunk{}, io.EOF
	}
	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // comments, blank lines, "event:" lines
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			s.sawDone = true
			break
		}
		var w chunkWire
		if err := json.Unmarshal([]byte(data), &w); err != nil {
			return backends.ChatChunk{}, &backends.Error{Backend: s.kind, Status: http.StatusBadGateway, Msg: "bad stream data: " + err.Error()}
		}
		if w.Error != nil {
			return backends.ChatChunk{}, &backends.Error{Backend: s.kind, Status: http.StatusBadGateway, Msg: w.Error.Message}
		}
		if w.Usage != nil {
			s.usage = backends.Usage{PromptTokens: w.Usage.PromptTokens, CompletionTokens: w.Usage.CompletionTokens}
		}
		var c backends.ChatChunk
		for _, ch := range w.Choices {
			c.Content += ch.Delta.Content
			c.Thinking += ch.Delta.ReasoningContent + ch.Delta.Reasoning
			for _, tc := range ch.Delta.ToolCalls {
				p := s.calls[tc.Index]
				if p == nil {
					p = &pendingCall{}
					s.calls[tc.Index] = p
					s.order = append(s.order, tc.Index)
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if ch.Logprobs != nil {
				for _, lp := range ch.Logprobs.Content {
					t := backends.TokenLogprob{Token: lp.Token, Logprob: lp.Logprob, Top: map[string]float64{}}
					for _, tl := range lp.TopLogprobs {
						t.Top[tl.Token] = tl.Logprob
					}
					c.Logprobs = append(c.Logprobs, t)
				}
			}
			if ch.FinishReason != "" {
				s.finish = true
			}
		}
		if c.Content != "" || c.Thinking != "" || len(c.Logprobs) > 0 {
			return c, nil
		}
	}
	if err := s.sc.Err(); err != nil {
		return backends.ChatChunk{}, err
	}
	if !s.sawDone && !s.finish {
		return backends.ChatChunk{}, io.ErrUnexpectedEOF
	}
	s.done = true
	last := backends.ChatChunk{Done: true, Usage: s.usage}
	for _, i := range s.order {
		p := s.calls[i]
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		id := p.id
		if id == "" {
			id = "call_" + ids.New()
		}
		last.ToolCalls = append(last.ToolCalls, backends.ToolCall{ID: id, Name: p.name, Args: json.RawMessage(args)})
	}
	return last, nil
}

func (s *stream) Close() error { return s.body.Close() }

// promptedJSON stands in for json_schema output on a server without it (mlx): the schema goes into
// the prompt, and the reply comes back whole with any ``` fence taken off, so callers that parse
// the content as JSON work the same. It isn't a guarantee; callers already handle a bad reply.
func (b *Backend) promptedJSON(ctx context.Context, req backends.ChatRequest) (backends.ChatStream, error) {
	hint := "Reply with only a JSON object that matches this JSON Schema, with no other text:\n" + string(req.Format)
	msgs := append([]backends.Message{}, req.Messages...)
	if n := len(msgs); n > 0 && msgs[n-1].Role == backends.RoleUser {
		m := msgs[n-1]
		m.Content += "\n\n" + hint
		msgs[n-1] = m
	} else {
		msgs = append(msgs, backends.Message{Role: backends.RoleUser, Content: hint})
	}
	req.Messages, req.Format = msgs, nil
	st, err := b.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	r, err := backends.Collect(st, nil)
	if err != nil {
		return nil, err
	}
	return &whole{c: backends.ChatChunk{Content: unfence(r.Content), Thinking: r.Thinking, ToolCalls: r.ToolCalls, Done: true, Usage: r.Usage}}, nil
}

// unfence returns the JSON inside a ```json … ``` block, or s trimmed.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return s
}

// whole is a stream of one finished chunk.
type whole struct {
	c    backends.ChatChunk
	sent bool
}

func (w *whole) Recv() (backends.ChatChunk, error) {
	if w.sent {
		return backends.ChatChunk{}, io.EOF
	}
	w.sent = true
	return w.c, nil
}

func (w *whole) Close() error { return nil }
