package backendtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// OllamaModel is a model the fake server lists.
type OllamaModel struct {
	Name   string
	Size   int64
	Params string
	Caps   []string // completion, tools, thinking, vision, embedding, decision
}

// OllamaReply is what the fake model says.
type OllamaReply struct {
	Content   string
	Thinking  string
	ToolCalls []OllamaToolCall
	Err       string
}

type OllamaToolCall struct {
	Name string
	Args map[string]any
}

// Ollama is an httptest server that speaks enough of Ollama's HTTP API for the adapter, the CLI
// and the proxy to run against it: version, tags, ps, show, chat (streamed), generate (load and
// unload), embed, pull and delete.
type Ollama struct {
	*httptest.Server
	Version string

	// Chat answers /api/chat. The request is the decoded JSON body.
	Chat func(req map[string]any) OllamaReply

	mu       sync.Mutex
	models   []OllamaModel
	loaded   map[string]int64
	Requests []map[string]any
	Pulled   []string
	Deleted  []string
}

// NewOllama starts a server with the given models. Close it with Close (or t.Cleanup).
func NewOllama(models ...OllamaModel) *Ollama {
	o := &Ollama{Version: "0.40.3", models: models, loaded: map[string]int64{}}
	o.Chat = func(map[string]any) OllamaReply { return OllamaReply{Content: "ok"} }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"version": o.Version})
	})
	mux.HandleFunc("GET /api/tags", o.tags)
	mux.HandleFunc("GET /api/ps", o.ps)
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion"}, "details": map[string]string{}})
	})
	mux.HandleFunc("POST /api/chat", o.chat)
	mux.HandleFunc("POST /api/generate", o.generate)
	mux.HandleFunc("POST /api/embed", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&in)
		out := make([][]float64, len(in.Input))
		for i, s := range in.Input {
			v := make([]float64, 16)
			for _, w := range strings.Fields(strings.ToLower(s)) {
				v[(len(w)*7+int(w[0]))%16]++
			}
			out[i] = v
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	})
	mux.HandleFunc("POST /api/pull", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Model string }
		json.NewDecoder(r.Body).Decode(&in)
		o.mu.Lock()
		o.Pulled = append(o.Pulled, in.Model)
		o.models = append(o.models, OllamaModel{Name: in.Model, Size: 1e9, Params: "1B", Caps: []string{"completion"}})
		o.mu.Unlock()
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		fmt.Fprintln(w, `{"status":"downloading","total":100,"completed":50}`)
		fmt.Fprintln(w, `{"status":"success"}`)
	})
	mux.HandleFunc("DELETE /api/delete", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Model string }
		json.NewDecoder(r.Body).Decode(&in)
		o.mu.Lock()
		defer o.mu.Unlock()
		o.Deleted = append(o.Deleted, in.Model)
		for i, m := range o.models {
			if m.Name == in.Model {
				o.models = append(o.models[:i], o.models[i+1:]...)
				break
			}
		}
	})
	o.Server = httptest.NewServer(mux)
	return o
}

// Loaded returns the names currently loaded.
func (o *Ollama) Loaded() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for n := range o.loaded {
		out = append(out, n)
	}
	return out
}

// ChatRequests returns the decoded /api/chat bodies seen so far.
func (o *Ollama) ChatRequests() []map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]map[string]any(nil), o.Requests...)
}

func (o *Ollama) tags(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	type m struct {
		Name         string         `json:"name"`
		Size         int64          `json:"size"`
		Details      map[string]any `json:"details"`
		Capabilities []string       `json:"capabilities"`
	}
	var out []m
	for _, x := range o.models {
		out = append(out, m{x.Name, x.Size, map[string]any{"family": strings.SplitN(x.Name, ":", 2)[0], "parameter_size": x.Params, "quantization_level": "Q4_K_M", "context_length": 32768}, x.Caps})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": out})
}

func (o *Ollama) ps(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []map[string]any
	for n, sz := range o.loaded {
		out = append(out, map[string]any{"name": n, "size": sz, "size_vram": sz, "context_length": 8192, "expires_at": "2099-01-01T00:00:00Z"})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": out})
}

func (o *Ollama) size(name string) int64 {
	for _, m := range o.models {
		if m.Name == name {
			return m.Size + 600_000_000
		}
	}
	return 1e9
}

func (o *Ollama) generate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model     string `json:"model"`
		KeepAlive any    `json:"keep_alive"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	o.mu.Lock()
	if f, ok := in.KeepAlive.(float64); ok && f == 0 {
		delete(o.loaded, in.Model)
	} else {
		o.loaded[in.Model] = o.size(in.Model)
	}
	o.mu.Unlock()
	fmt.Fprintln(w, `{"done":true}`)
}

func (o *Ollama) chat(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	name, _ := req["model"].(string)
	o.mu.Lock()
	o.Requests = append(o.Requests, req)
	if ka, ok := req["keep_alive"].(float64); !ok || ka != 0 {
		o.loaded[name] = o.size(name)
	}
	o.mu.Unlock()
	rep := o.Chat(req)
	if rep.Err != "" {
		fmt.Fprintf(w, `{"error":%q}`+"\n", rep.Err)
		return
	}
	fl, _ := w.(http.Flusher)
	line := func(v any) {
		b, _ := json.Marshal(v)
		w.Write(append(b, '\n'))
		if fl != nil {
			fl.Flush()
		}
	}
	if rep.Thinking != "" {
		line(map[string]any{"message": map[string]any{"role": "assistant", "thinking": rep.Thinking}, "done": false})
	}
	if rep.Content != "" {
		half := len(rep.Content) / 2
		line(map[string]any{"message": map[string]any{"role": "assistant", "content": rep.Content[:half]}, "done": false})
		line(map[string]any{"message": map[string]any{"role": "assistant", "content": rep.Content[half:]}, "done": false})
	}
	if len(rep.ToolCalls) > 0 {
		var calls []map[string]any
		for _, c := range rep.ToolCalls {
			calls = append(calls, map[string]any{"function": map[string]any{"name": c.Name, "arguments": c.Args}})
		}
		line(map[string]any{"message": map[string]any{"role": "assistant", "content": "", "tool_calls": calls}, "done": false})
	}
	line(map[string]any{"message": map[string]any{"role": "assistant", "content": ""}, "done": true, "done_reason": "stop",
		"prompt_eval_count": 50, "eval_count": 12 + len(rep.Content)/4})
}
