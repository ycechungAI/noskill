// Package backends defines the model-server interface (ARCHITECTURE.md "Backend adapters",
// NFR-8). Adapters (ollama, openai, mlx) live in subpackages and register themselves; the agent
// loop, router and proxy only see these types.
package backends

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Backend is one model server.
type Backend interface {
	Name() string
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Chat starts a generation. The stream must be closed; closing it early cancels the request
	// (the backend stops generating, PROXY.md "Cancel").
	Chat(ctx context.Context, req ChatRequest) (ChatStream, error)
	Embed(ctx context.Context, req EmbedRequest) ([][]float32, error)
	// Load loads a model and keeps it for keepAlive (negative = pinned; 0 = unload).
	Load(ctx context.Context, model string, keepAlive time.Duration) error
	Loaded(ctx context.Context) ([]LoadedModel, error)
}

// CtxLoader is implemented by backends that load a model at a chosen context size (Ollama). The
// router uses it so the model isn't reloaded when the first request arrives with another num_ctx.
type CtxLoader interface {
	LoadCtx(ctx context.Context, model string, numCtx int, keepAlive time.Duration) error
}

// Manager is implemented by backends that can download and remove models (Ollama). The
// model-manager skill asks the user before using either.
type Manager interface {
	Pull(ctx context.Context, name string, progress func(status string)) error
	Delete(ctx context.Context, name string) error
}

// Capability flags in a model profile (FR-B2).
type Capability string

const (
	CapChat     Capability = "chat"
	CapTools    Capability = "tools"
	CapThinking Capability = "thinking"
	CapVision   Capability = "vision"
	CapEmbed    Capability = "embed"
	CapDecide   Capability = "decide"
	CapSchema   Capability = "schema" // JSON-schema / grammar constrained output (FR-B3)
	// CapThinkAlways: thinking can't be turned off (the template always opens <think>, as in
	// Qwen3 "Thinking-2507" builds). think=false then returns the reasoning as content instead.
	CapThinkAlways Capability = "think-always"
)

// ToolStrategy is how a model calls tools (ADR-0005): native by default, constrained as the
// fallback and as the retry after a malformed native call, prompted when nothing else works.
type ToolStrategy string

const (
	StrategyNative      ToolStrategy = "native"
	StrategyConstrained ToolStrategy = "constrained"
	StrategyPrompted    ToolStrategy = "prompted"
)

type ModelInfo struct {
	ID           string // "<backend>/<name>", e.g. "ollama/qwen3:8b"
	Backend      string
	Name         string
	Family       string
	ParamsB      float64
	Quant        string
	Context      int
	SizeBytes    int64
	Capabilities []Capability
}

func (m ModelInfo) Has(c Capability) bool {
	for _, x := range m.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

type LoadedModel struct {
	Name      string
	SizeBytes int64 // memory in use, including KV and prompt cache
	VRAMBytes int64
	Context   int
	ExpiresAt time.Time
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	Images     [][]byte   `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // on RoleTool messages
	ToolName   string     `json:"tool_name,omitempty"`    // on RoleTool messages
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments"`
}

// ToolDef is what the model sees for one tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

type ChatRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolDef       // native tool calling; nil = none offered
	Format   json.RawMessage // JSON Schema that constrains the output (constrained strategy, emulated decide)
	// Think: nil = model default; false sends reasoning_effort "none" / think:false. Internal steps
	// send false unless the client asked for thinking (PROXY.md "Thinking fields").
	Think *bool
	// ThinkLevel: "low" | "medium" | "high", for models whose template reads a graded effort
	// instead of a bool (e.g. granite4.2, PROXY.md "Thinking fields"). Wins over Think when set;
	// ignored by backends/models that don't support it.
	ThinkLevel string
	Options    Options
	KeepAlive *time.Duration
}

type Options struct {
	NumCtx      int // fixed per model (ORCHESTRATOR.md): changing it makes Ollama reload
	NumPredict  int // per-step output cap (loop guard)
	Temperature *float64
	TopP        *float64
	Seed        *int
	Stop        []string
	Logprobs    bool
}

// ChatChunk is one streamed piece. The final chunk has Done set and carries usage.
type ChatChunk struct {
	Content   string
	Thinking  string
	ToolCalls []ToolCall
	Logprobs  []TokenLogprob
	Done      bool
	Usage     Usage
}

type TokenLogprob struct {
	Token   string
	Logprob float64
	Top     map[string]float64
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }

// ChatStream yields chunks until io.EOF.
type ChatStream interface {
	Recv() (ChatChunk, error)
	Close() error
}

type EmbedRequest struct {
	Model string
	Input []string
}

// Response is a whole generation, collected from a stream.
type Response struct {
	Content   string
	Thinking  string
	ToolCalls []ToolCall
	Logprobs  []TokenLogprob
	Usage     Usage
}

// Collect reads a stream to the end. onChunk (may be nil) sees each chunk as it arrives, for
// streaming to the client.
func Collect(s ChatStream, onChunk func(ChatChunk)) (Response, error) {
	defer s.Close()
	var r Response
	for {
		c, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		if onChunk != nil {
			onChunk(c)
		}
		r.Content += c.Content
		r.Thinking += c.Thinking
		r.ToolCalls = append(r.ToolCalls, c.ToolCalls...)
		r.Logprobs = append(r.Logprobs, c.Logprobs...)
		if c.Done {
			r.Usage = c.Usage
		}
	}
}

// Error is a backend failure with the HTTP status it maps to (PROXY.md "Errors": 502, 503).
type Error struct {
	Backend string
	Status  int
	Msg     string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s (status %d)", e.Backend, e.Msg, e.Status) }

// Factory builds a backend from its [[backends]] entry.
type Factory func(url, apiKey string) (Backend, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds an adapter kind ("ollama", "openai", "mlx"). Adapters call it from init().
func Register(kind string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("backends: duplicate kind " + kind)
	}
	registry[kind] = f
}

func New(kind, url, apiKey string) (Backend, error) {
	regMu.RLock()
	f, ok := registry[kind]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend kind %q (have %v)", kind, Kinds())
	}
	return f(url, apiKey)
}

func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
