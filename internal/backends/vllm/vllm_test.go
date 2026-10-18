package vllm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"noskill/internal/backends"
)

// FR-B5: vLLM's model list carries the context length, its models count as loaded, the version
// comes from /version, and json_schema output is sent.
func TestVLLM(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"Qwen/Qwen3-8B","owned_by":"vllm","max_model_len":32768}]}`)
		case "/version":
			io.WriteString(w, `{"version":"0.11.0"}`)
		case "/v1/chat/completions":
			json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b, err := backends.New("vllm", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ms, err := b.ListModels(ctx)
	if err != nil || len(ms) != 1 {
		t.Fatalf("models %v %v", ms, err)
	}
	m := ms[0]
	if m.ID != "vllm/Qwen/Qwen3-8B" || m.Context != 32768 || !m.Has(backends.CapSchema) || !m.Has(backends.CapTools) {
		t.Fatalf("model %+v", m)
	}
	if l, _ := b.Loaded(ctx); len(l) != 1 || l[0].Name != "Qwen/Qwen3-8B" {
		t.Fatalf("loaded %v", l)
	}
	if v, _ := b.(interface {
		Version(context.Context) (string, error)
	}).Version(ctx); v != "0.11.0" {
		t.Fatalf("version %q", v)
	}
	s, err := b.Chat(ctx, backends.ChatRequest{Model: m.Name, Messages: []backends.Message{{Role: backends.RoleUser, Content: "hi"}},
		Format: json.RawMessage(`{"type":"object"}`), Options: backends.Options{Logprobs: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backends.Collect(s, nil); err != nil {
		t.Fatal(err)
	}
	if body["response_format"] == nil || body["logprobs"] != true {
		t.Fatalf("request %v", body)
	}
	if b.Name() != "vllm" || !slices.Contains(backends.Kinds(), "vllm") {
		t.Fatal("kind")
	}
}
