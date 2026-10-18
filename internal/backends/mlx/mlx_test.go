package mlx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"noskill/internal/backends"
)

// FR-B5: mlx_lm.server lists cached models (vision and embedding ones left out), has no json_schema
// output, and isn't sent OpenAI's boolean logprobs.
func TestMLX(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"mlx-community/Qwen3-4B-4bit"},{"id":"mlx-community/Qwen2.5-VL-3B-Instruct-4bit"},{"id":"mlx-community/nomic-embed-text"}]}`)
		case "/v1/chat/completions":
			json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"```json\\n{\\\"a\\\": 1}\\n```\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b, err := backends.New("mlx", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ms, err := b.ListModels(ctx)
	if err != nil || len(ms) != 1 {
		t.Fatalf("models %v %v", ms, err)
	}
	m := ms[0]
	if m.ID != "mlx/mlx-community/Qwen3-4B-4bit" || m.Family != "qwen3" || m.Quant != "4bit" || m.Has(backends.CapSchema) || !m.Has(backends.CapTools) {
		t.Fatalf("model %+v", m)
	}
	s, err := b.Chat(ctx, backends.ChatRequest{Model: m.Name, Messages: []backends.Message{{Role: backends.RoleUser, Content: "hi"}},
		Format: json.RawMessage(`{"type":"object"}`), Options: backends.Options{Logprobs: true}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := backends.Collect(s, nil); err != nil || r.Content != `{"a": 1}` {
		t.Fatalf("%q %v", r.Content, err)
	}
	msgs, _ := body["messages"].([]any)
	if last, _ := msgs[len(msgs)-1].(map[string]any); !strings.Contains(last["content"].(string), "JSON Schema") {
		t.Fatalf("no schema in the prompt: %v", body)
	}
	if _, ok := body["response_format"]; ok {
		t.Fatalf("sent response_format: %v", body)
	}
	if _, ok := body["logprobs"]; ok {
		t.Fatalf("sent logprobs: %v", body)
	}
	var be *backends.Error
	if _, err := b.Embed(ctx, backends.EmbedRequest{Model: "x", Input: []string{"a"}}); !errors.As(err, &be) || be.Status != http.StatusNotImplemented {
		t.Fatalf("embed err %v", err)
	}
}
