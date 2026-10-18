package openai

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

func serve(t *testing.T, h http.HandlerFunc) *Backend {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/v1", "key123")
}

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, l := range lines {
		io.WriteString(w, "data: "+l+"\n\n")
	}
}

func TestChatStreamToolCallsAndUsage(t *testing.T) {
	var got map[string]any
	var auth string
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		sse(w,
			`{"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
			`{"choices":[{"delta":{"content":"Hi"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"fetch","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":9}}`,
			`[DONE]`)
	})
	no := false
	s, err := b.Chat(context.Background(), backends.ChatRequest{
		Model: "qwen", Think: &no, Format: json.RawMessage(`{"type":"object"}`),
		Messages: []backends.Message{
			{Role: backends.RoleUser, Content: "hi"},
			{Role: backends.RoleAssistant, ToolCalls: []backends.ToolCall{{ID: "c0", Name: "search", Args: json.RawMessage(`{"q":"y"}`)}}},
			{Role: backends.RoleTool, Content: "r", ToolCallID: "c0"},
		},
		Tools:   []backends.ToolDef{{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Options: backends.Options{NumPredict: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := backends.Collect(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "Hi" || r.Thinking != "think " || r.Usage.PromptTokens != 30 || r.Usage.CompletionTokens != 9 {
		t.Errorf("response = %+v", r)
	}
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].ID != "call_1" || string(r.ToolCalls[0].Args) != `{"q":"x"}` ||
		r.ToolCalls[1].Name != "fetch" || string(r.ToolCalls[1].Args) != "{}" || r.ToolCalls[1].ID == "" {
		t.Errorf("tool calls = %+v", r.ToolCalls)
	}
	if auth != "Bearer key123" {
		t.Errorf("auth = %q", auth)
	}
	if got["max_tokens"] != float64(100) || got["reasoning_effort"] != "none" || got["stream"] != true {
		t.Errorf("request = %v", got)
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["schema"].(map[string]any)["type"] != "object" {
		t.Errorf("response_format = %v", rf)
	}
	msgs := got["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["content"] != nil || asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"q":"y"}` {
		t.Errorf("arguments must be a JSON string here: %v", asst)
	}
	if msgs[2].(map[string]any)["tool_call_id"] != "c0" {
		t.Errorf("tool message = %v", msgs[2])
	}
}

func TestTruncatedStreamIsAnError(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"content":"par"}}]}`)
	})
	s, _ := b.Chat(context.Background(), backends.ChatRequest{Model: "m"})
	if _, err := backends.Collect(s, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v", err)
	}
}

func TestListModelsPlainAndLMStudio(t *testing.T) {
	plain := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"id":"qwen2.5-7b-instruct"},{"id":"nomic-embed-text-v1.5"}]}`)
	})
	ms, err := plain.ListModels(context.Background())
	if err != nil || len(ms) != 2 || !ms[0].Has(backends.CapChat) || !ms[1].Has(backends.CapEmbed) || ms[1].Has(backends.CapChat) {
		t.Fatalf("%+v %v", ms, err)
	}
	lm := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[
		 {"id":"qwen3-4b","type":"llm","arch":"qwen3","quantization":"Q4_K_M","max_context_length":40960,"capabilities":["tool_use"]},
		 {"id":"gemma-3-4b","type":"vlm","max_context_length":131072},
		 {"id":"text-embedding-nomic","type":"embeddings"}]}`)
	})
	ms, _ = lm.ListModels(context.Background())
	if len(ms) != 3 || !ms[0].Has(backends.CapTools) || ms[0].Context != 40960 || !ms[1].Has(backends.CapVision) || ms[1].Has(backends.CapTools) || !ms[2].Has(backends.CapEmbed) {
		t.Fatalf("%+v", ms)
	}
}

func TestEmbedOrderAndErrors(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			io.WriteString(w, `{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`)
			return
		}
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"model not loaded","type":"invalid_request_error"}}`)
	})
	e, err := b.Embed(context.Background(), backends.EmbedRequest{Model: "e", Input: []string{"a", "b"}})
	if err != nil || e[0][0] != 1 || e[1][0] != 3 {
		t.Errorf("embed = %v %v", e, err)
	}
	_, err = b.Chat(context.Background(), backends.ChatRequest{Model: "m"})
	var be *backends.Error
	if !errors.As(err, &be) || be.Status != 400 || be.Msg != "model not loaded" {
		t.Errorf("err = %v", err)
	}
	if _, err := New("http://127.0.0.1:1/v1", "").ListModels(context.Background()); !errors.As(err, &be) || be.Status != 503 {
		t.Errorf("refused: %v", err)
	}
}

func TestURLForms(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:1234/v1": "http://h:1234/v1", "http://h:1234": "http://h:1234/v1", "http://h:1234/v1/": "http://h:1234/v1",
	} {
		if got := New(in, "").root; got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
}
