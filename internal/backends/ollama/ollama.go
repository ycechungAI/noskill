// Package ollama is the Ollama adapter (MODEL_ADAPTERS.md "Backends"): native /api/chat with
// tools, think and format, /api/ps for loaded models, keep_alive for residency (FR-B1–B3).
package ollama

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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"noskill/internal/backends"
	"noskill/internal/ids"
)

func init() {
	backends.Register("ollama", func(url, apiKey string) (backends.Backend, error) { return New(url, apiKey), nil })
}

// Backend talks to one Ollama server.
type Backend struct {
	base   string
	apiKey string
	hc     *http.Client

	mu        sync.Mutex
	thinkOnly map[string]bool // digest → template always opens <think>
}

func New(url, apiKey string) *Backend {
	if url == "" {
		url = "http://127.0.0.1:11434"
	}
	return &Backend{base: strings.TrimRight(url, "/"), apiKey: apiKey, hc: &http.Client{}}
}

func (b *Backend) Name() string { return "ollama" }

// Version returns the server version (`doctor` wants ≥ 0.35 for decision models).
func (b *Backend) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := b.getJSON(ctx, "/api/version", &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

func (b *Backend) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, rd)
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
		// Refused, timed out, DNS: the server isn't there. PROXY.md maps this to 503.
		return nil, &backends.Error{Backend: "ollama", Status: http.StatusServiceUnavailable, Msg: unwrap(err)}
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		status := resp.StatusCode
		if status >= 500 {
			status = http.StatusBadGateway
		}
		return nil, &backends.Error{Backend: "ollama", Status: status, Msg: msg}
	}
	return resp, nil
}

func unwrap(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return "can't reach " + op.Addr.String() + ": " + op.Err.Error()
	}
	return err.Error()
}

func (b *Backend) getJSON(ctx context.Context, path string, out any) error {
	resp, err := b.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *Backend) postJSON(ctx context.Context, path string, in, out any) error {
	resp, err := b.do(ctx, http.MethodPost, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- models ----

type tagsResp struct {
	Models []struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Digest  string `json:"digest"`
		Details struct {
			Family        string `json:"family"`
			ParameterSize string `json:"parameter_size"`
			Quant         string `json:"quantization_level"`
			ContextLength int    `json:"context_length"`
		} `json:"details"`
		Capabilities []string `json:"capabilities"`
	} `json:"models"`
}

func (b *Backend) ListModels(ctx context.Context) ([]backends.ModelInfo, error) {
	var t tagsResp
	if err := b.getJSON(ctx, "/api/tags", &t); err != nil {
		return nil, err
	}
	out := make([]backends.ModelInfo, 0, len(t.Models))
	for _, m := range t.Models {
		caps, ctxLen := m.Capabilities, m.Details.ContextLength
		if len(caps) == 0 { // older servers: ask /api/show
			caps, ctxLen = b.show(ctx, m.Name, ctxLen)
		}
		mc := mapCaps(caps)
		if slices.Contains(mc, backends.CapThinking) && b.alwaysThinks(ctx, m.Name, m.Digest) {
			mc = append(mc, backends.CapThinkAlways)
		}
		out = append(out, backends.ModelInfo{
			ID: "ollama/" + m.Name, Backend: "ollama", Name: m.Name, Family: m.Details.Family,
			ParamsB: parseParams(m.Details.ParameterSize), Quant: m.Details.Quant, Context: ctxLen,
			SizeBytes: m.Size, Capabilities: mc,
		})
	}
	return out, nil
}

// alwaysThinks reads a thinking model's template once per digest (one /api/show call).
func (b *Backend) alwaysThinks(ctx context.Context, name, digest string) bool {
	key := digest
	if key == "" {
		key = name
	}
	b.mu.Lock()
	v, ok := b.thinkOnly[key]
	b.mu.Unlock()
	if ok {
		return v
	}
	var s struct {
		Template string `json:"template"`
	}
	if err := b.postJSON(ctx, "/api/show", map[string]string{"model": name}, &s); err != nil {
		return false
	}
	v = templateAlwaysThinks(s.Template)
	b.mu.Lock()
	if b.thinkOnly == nil {
		b.thinkOnly = map[string]bool{}
	}
	b.thinkOnly[key] = v
	b.mu.Unlock()
	return v
}

var reThinkVar = regexp.MustCompile(`\$\.Think\b`)

// templateAlwaysThinks: the generation prompt ends by opening <think>, and nothing in the template
// reads $.Think, so think=false can't close it (Ollama 0.40.3 with qwen3:4b, block 1c review).
// Templates rendered in Go code (empty here) and hybrid templates that check $.Think say false.
func templateAlwaysThinks(t string) bool {
	if t == "" || reThinkVar.MatchString(t) {
		return false
	}
	i := strings.LastIndex(t, "<|im_start|>assistant")
	if i < 0 {
		i = max(0, len(t)-200)
	}
	return strings.Contains(t[i:], "<think>") && !strings.Contains(t[i:], "</think>")
}

func (b *Backend) show(ctx context.Context, name string, ctxLen int) ([]string, int) {
	var s struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := b.postJSON(ctx, "/api/show", map[string]string{"model": name}, &s); err != nil {
		return []string{"completion"}, ctxLen
	}
	for k, v := range s.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok {
				ctxLen = int(f)
			}
		}
	}
	return s.Capabilities, ctxLen
}

func mapCaps(in []string) []backends.Capability {
	var out []backends.Capability
	for _, c := range in {
		switch c {
		case "completion":
			// Constrained output (format) works on every completion model (FR-B3).
			out = append(out, backends.CapChat, backends.CapSchema)
		case "tools":
			out = append(out, backends.CapTools)
		case "thinking":
			out = append(out, backends.CapThinking)
		case "vision":
			out = append(out, backends.CapVision)
		case "embedding":
			out = append(out, backends.CapEmbed)
		case "decision":
			out = append(out, backends.CapDecide)
		}
	}
	return out
}

func parseParams(s string) float64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "M"):
		mult = 0.001
	case strings.HasSuffix(s, "B"):
	case strings.HasSuffix(s, "K"):
		mult = 0.000001
	default:
		return 0
	}
	f, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0
	}
	return f * mult
}

// ---- loaded models ----

func (b *Backend) Loaded(ctx context.Context) ([]backends.LoadedModel, error) {
	var p struct {
		Models []struct {
			Name          string    `json:"name"`
			Size          int64     `json:"size"`
			SizeVRAM      int64     `json:"size_vram"`
			ContextLength int       `json:"context_length"`
			ExpiresAt     time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := b.getJSON(ctx, "/api/ps", &p); err != nil {
		return nil, err
	}
	out := make([]backends.LoadedModel, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, backends.LoadedModel{Name: m.Name, SizeBytes: m.Size, VRAMBytes: m.SizeVRAM, Context: m.ContextLength, ExpiresAt: m.ExpiresAt})
	}
	return out, nil
}

// keepAlive: Ollama takes seconds; a negative number pins the model.
func keepAlive(d time.Duration) any {
	if d < 0 {
		return -1
	}
	return int(d.Seconds())
}

func (b *Backend) Load(ctx context.Context, model string, ka time.Duration) error {
	return b.LoadCtx(ctx, model, 0, ka)
}

// LoadCtx loads a model at the context size noskill will send with its requests: Ollama reloads a
// model when num_ctx changes (ORCHESTRATOR.md "Fixed context per model").
func (b *Backend) LoadCtx(ctx context.Context, model string, numCtx int, ka time.Duration) error {
	body := map[string]any{"model": model, "keep_alive": keepAlive(ka)}
	if numCtx > 0 {
		body["options"] = map[string]any{"num_ctx": numCtx}
	}
	return b.postJSON(ctx, "/api/generate", body, nil)
}

func (b *Backend) Embed(ctx context.Context, req backends.EmbedRequest) ([][]float32, error) {
	var r struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := b.postJSON(ctx, "/api/embed", map[string]any{"model": req.Model, "input": req.Input}, &r); err != nil {
		return nil, err
	}
	if len(r.Embeddings) != len(req.Input) {
		return nil, &backends.Error{Backend: "ollama", Status: http.StatusBadGateway, Msg: fmt.Sprintf("asked for %d embeddings, got %d", len(req.Input), len(r.Embeddings))}
	}
	return r.Embeddings, nil
}

// ---- chat ----

type wireMsg struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Thinking  string         `json:"thinking,omitempty"`
	Images    []string       `json:"images,omitempty"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func toWire(msgs []backends.Message) []wireMsg {
	out := make([]wireMsg, len(msgs))
	for i, m := range msgs {
		w := wireMsg{Role: string(m.Role), Content: m.Content, Thinking: m.Thinking, ToolName: m.ToolName}
		for _, img := range m.Images {
			w.Images = append(w.Images, base64.StdEncoding.EncodeToString(img))
		}
		for _, c := range m.ToolCalls {
			var wc wireToolCall
			wc.ID = c.ID
			wc.Function.Name = c.Name
			wc.Function.Arguments = c.Args
			if len(wc.Function.Arguments) == 0 {
				wc.Function.Arguments = json.RawMessage("{}")
			}
			w.ToolCalls = append(w.ToolCalls, wc)
		}
		out[i] = w
	}
	return out
}

func (b *Backend) chatBody(req backends.ChatRequest) map[string]any {
	body := map[string]any{"model": req.Model, "messages": toWire(req.Messages), "stream": true}
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
	if len(req.Format) > 0 {
		body["format"] = req.Format
	}
	if req.ThinkLevel != "" {
		body["think"] = req.ThinkLevel
	} else if req.Think != nil {
		body["think"] = *req.Think
	}
	if req.KeepAlive != nil {
		body["keep_alive"] = keepAlive(*req.KeepAlive)
	}
	opts := map[string]any{}
	o := req.Options
	if o.NumCtx > 0 {
		opts["num_ctx"] = o.NumCtx
	}
	if o.NumPredict > 0 {
		opts["num_predict"] = o.NumPredict
	}
	if o.Temperature != nil {
		opts["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		opts["top_p"] = *o.TopP
	}
	if o.Seed != nil {
		opts["seed"] = *o.Seed
	}
	if len(o.Stop) > 0 {
		opts["stop"] = o.Stop
	}
	if len(opts) > 0 {
		body["options"] = opts
	}
	if o.Logprobs {
		body["logprobs"] = true
		body["top_logprobs"] = 5
	}
	return body
}

type chunkWire struct {
	Message struct {
		Content   string         `json:"content"`
		Thinking  string         `json:"thinking"`
		ToolCalls []wireToolCall `json:"tool_calls"`
	} `json:"message"`
	Logprobs []struct {
		Token       string  `json:"token"`
		Logprob     float64 `json:"logprob"`
		TopLogprobs []struct {
			Token   string  `json:"token"`
			Logprob float64 `json:"logprob"`
		} `json:"top_logprobs"`
	} `json:"logprobs"`
	Done            bool   `json:"done"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

type stream struct {
	body io.ReadCloser
	dec  *json.Decoder
	done bool
}

func (b *Backend) Chat(ctx context.Context, req backends.ChatRequest) (backends.ChatStream, error) {
	resp, err := b.do(ctx, http.MethodPost, "/api/chat", b.chatBody(req))
	if err != nil {
		return nil, err
	}
	return &stream{body: resp.Body, dec: json.NewDecoder(bufio.NewReader(resp.Body))}, nil
}

func (s *stream) Recv() (backends.ChatChunk, error) {
	if s.done {
		return backends.ChatChunk{}, io.EOF
	}
	var w chunkWire
	if err := s.dec.Decode(&w); err != nil {
		if errors.Is(err, io.EOF) {
			return backends.ChatChunk{}, io.ErrUnexpectedEOF // the server hung up before "done"
		}
		return backends.ChatChunk{}, err
	}
	if w.Error != "" {
		s.done = true
		return backends.ChatChunk{}, &backends.Error{Backend: "ollama", Status: http.StatusBadGateway, Msg: w.Error}
	}
	c := backends.ChatChunk{Content: w.Message.Content, Thinking: w.Message.Thinking, Done: w.Done}
	for _, tc := range w.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "call_" + ids.New()
		}
		args := tc.Function.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		c.ToolCalls = append(c.ToolCalls, backends.ToolCall{ID: id, Name: tc.Function.Name, Args: args})
	}
	for _, lp := range w.Logprobs {
		t := backends.TokenLogprob{Token: lp.Token, Logprob: lp.Logprob, Top: map[string]float64{}}
		for _, tl := range lp.TopLogprobs {
			t.Top[tl.Token] = tl.Logprob
		}
		c.Logprobs = append(c.Logprobs, t)
	}
	if w.Done {
		c.Usage = backends.Usage{PromptTokens: w.PromptEvalCount, CompletionTokens: w.EvalCount}
		s.done = true
	}
	return c, nil
}

// Close ends the request; closing before "done" makes Ollama stop generating at once (PROXY.md
// "Cancel", measured in S2).
func (s *stream) Close() error { return s.body.Close() }

// Pull downloads a model, reporting each distinct status line ("pulling manifest", "downloading
// 45%", …). It is only called after the user approved it.
func (b *Backend) Pull(ctx context.Context, name string, progress func(string)) error {
	resp, err := b.do(ctx, http.MethodPost, "/api/pull", map[string]any{"model": name, "stream": true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var lastStatus, lastLine string
	for {
		var m struct {
			Status    string `json:"status"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				if lastStatus != "success" {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
		if m.Error != "" {
			return &backends.Error{Backend: "ollama", Status: http.StatusBadGateway, Msg: m.Error}
		}
		line := m.Status
		if m.Total > 0 {
			line = fmt.Sprintf("%s %d%%", m.Status, 100*m.Completed/m.Total)
		}
		if line != lastLine && progress != nil {
			progress(line)
		}
		lastLine, lastStatus = line, m.Status
	}
}

// Delete removes an installed model from Ollama's store.
func (b *Backend) Delete(ctx context.Context, name string) error {
	resp, err := b.do(ctx, http.MethodDelete, "/api/delete", map[string]string{"model": name})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
