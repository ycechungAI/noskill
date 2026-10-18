package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"noskill/internal/backends"
)

func serve(t *testing.T, h http.HandlerFunc) *Backend {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

func TestListModelsAndCapabilities(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"models":[
		 {"name":"qwen3:4b","size":2497293931,"details":{"family":"qwen3","parameter_size":"4.0B","quantization_level":"Q4_K_M","context_length":262144},"capabilities":["completion","tools","thinking"]},
		 {"name":"tev1:0.8b","size":1524372929,"details":{"parameter_size":"752M"},"capabilities":["decision"]},
		 {"name":"qwen3.5:4b","details":{"parameter_size":"4B"},"capabilities":["completion","vision","thinking","tools"]},
		 {"name":"nomic-embed-text","details":{},"capabilities":["embedding"]}]}`)
	})
	ms, err := b.ListModels(context.Background())
	if err != nil || len(ms) != 4 {
		t.Fatalf("%v %v", ms, err)
	}
	q := ms[0]
	if q.ID != "ollama/qwen3:4b" || q.ParamsB != 4 || q.Context != 262144 || !q.Has(backends.CapTools) || !q.Has(backends.CapThinking) || !q.Has(backends.CapSchema) || q.Has(backends.CapVision) {
		t.Errorf("qwen3:4b = %+v", q)
	}
	d := ms[1]
	if !d.Has(backends.CapDecide) || d.Has(backends.CapChat) || d.ParamsB != 0.752 {
		t.Errorf("tev1 must be a decision model only, not a chat candidate: %+v", d)
	}
	if !ms[2].Has(backends.CapVision) || !ms[3].Has(backends.CapEmbed) || ms[3].Has(backends.CapChat) {
		t.Errorf("vision/embed = %+v %+v", ms[2], ms[3])
	}
}

func TestOldServerFallsBackToShow(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			io.WriteString(w, `{"models":[{"name":"llama3.2:3b","details":{"parameter_size":"3.2B"}}]}`)
		case "/api/show":
			io.WriteString(w, `{"capabilities":["completion","tools"],"model_info":{"llama.context_length":131072}}`)
		}
	})
	ms, _ := b.ListModels(context.Background())
	if len(ms) != 1 || ms[0].Context != 131072 || !ms[0].Has(backends.CapTools) {
		t.Fatalf("%+v", ms)
	}
}

func TestChatRequestAndStream(t *testing.T) {
	var got map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"message":{"role":"assistant","thinking":"hm"},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"role":"assistant","content":"Hel"},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"role":"assistant","content":"lo","tool_calls":[{"function":{"name":"search","arguments":{"q":"x"}}}]},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":42,"eval_count":7}`+"\n")
	})
	no := false
	temp := 0.2
	ka := -time.Second
	s, err := b.Chat(context.Background(), backends.ChatRequest{
		Model: "qwen3:4b", Think: &no, KeepAlive: &ka, Format: json.RawMessage(`{"type":"object"}`),
		Messages: []backends.Message{
			{Role: backends.RoleUser, Content: "hi", Images: [][]byte{{1, 2, 3}}},
			{Role: backends.RoleAssistant, ToolCalls: []backends.ToolCall{{ID: "a", Name: "search", Args: json.RawMessage(`{"q":"y"}`)}}},
			{Role: backends.RoleTool, Content: "result", ToolName: "search", ToolCallID: "a"},
		},
		Tools:   []backends.ToolDef{{Name: "search", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Options: backends.Options{NumCtx: 8192, NumPredict: 1024, Temperature: &temp, Logprobs: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := backends.Collect(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "Hello" || r.Thinking != "hm" || r.Usage.PromptTokens != 42 || r.Usage.CompletionTokens != 7 {
		t.Errorf("response = %+v", r)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "search" || r.ToolCalls[0].ID == "" || string(r.ToolCalls[0].Args) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", r.ToolCalls)
	}
	if got["think"] != false || got["keep_alive"] != float64(-1) || got["stream"] != true || got["logprobs"] != true {
		t.Errorf("request = %v", got)
	}
	opts := got["options"].(map[string]any)
	if opts["num_ctx"] != float64(8192) || opts["num_predict"] != float64(1024) || opts["temperature"] != 0.2 {
		t.Errorf("options = %v", opts)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["images"].([]any)[0] != "AQID" {
		t.Errorf("image not base64: %v", msgs[0])
	}
	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if call["arguments"].(map[string]any)["q"] != "y" {
		t.Errorf("arguments must be an object, not a string: %v", call)
	}
	if msgs[2].(map[string]any)["tool_name"] != "search" {
		t.Errorf("tool message = %v", msgs[2])
	}
	if _, ok := got["tools"]; !ok {
		t.Error("tools missing")
	}
}

func TestErrors(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "chat"):
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"model 'nope' not found"}`)
		default:
			w.WriteHeader(500)
			io.WriteString(w, `{"error":"boom"}`)
		}
	})
	_, err := b.Chat(context.Background(), backends.ChatRequest{Model: "nope"})
	var be *backends.Error
	if !errors.As(err, &be) || be.Status != 404 || !strings.Contains(be.Msg, "not found") {
		t.Errorf("404: %v", err)
	}
	if _, err := b.ListModels(context.Background()); !errors.As(err, &be) || be.Status != 502 {
		t.Errorf("5xx must map to 502: %v", err)
	}
	// Nothing listening: 503 so the proxy can say the backend is down.
	dead := New("http://127.0.0.1:1", "")
	if _, err := dead.ListModels(context.Background()); !errors.As(err, &be) || be.Status != 503 {
		t.Errorf("refused: %v", err)
	}
}

func TestMidStreamErrorAndEarlyHangup(t *testing.T) {
	n := 0
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		io.WriteString(w, `{"message":{"content":"par"},"done":false}`+"\n")
		if n == 1 {
			io.WriteString(w, `{"error":"model runner crashed"}`+"\n")
		}
	})
	s, _ := b.Chat(context.Background(), backends.ChatRequest{Model: "m"})
	if _, err := backends.Collect(s, nil); err == nil || !strings.Contains(err.Error(), "runner crashed") {
		t.Errorf("mid-stream error: %v", err)
	}
	s, _ = b.Chat(context.Background(), backends.ChatRequest{Model: "m"})
	if _, err := backends.Collect(s, nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("hang-up before done must not look like a finished answer: %v", err)
	}
}

func TestCancelClosesRequest(t *testing.T) {
	closed := make(chan struct{})
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		io.WriteString(w, `{"message":{"content":"a"},"done":false}`+"\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	})
	s, err := b.Chat(context.Background(), backends.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the stream didn't reach the server")
	}
}

func TestLoadPsEmbed(t *testing.T) {
	var gen map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			json.NewDecoder(r.Body).Decode(&gen)
			io.WriteString(w, `{"done":true}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen3:4b","size":3900000000,"size_vram":3900000000,"context_length":8192,"expires_at":"2026-10-15T12:05:00Z"}]}`)
		case "/api/embed":
			io.WriteString(w, `{"embeddings":[[0.1,0.2],[0.3,0.4]]}`)
		}
	})
	ctx := context.Background()
	if err := b.LoadCtx(ctx, "qwen3:4b", 8192, -1); err != nil {
		t.Fatal(err)
	}
	if gen["keep_alive"] != float64(-1) || gen["options"].(map[string]any)["num_ctx"] != float64(8192) {
		t.Errorf("load = %v", gen)
	}
	b.Load(ctx, "qwen3:4b", 0)
	if gen["keep_alive"] != float64(0) {
		t.Errorf("unload = %v", gen)
	}
	l, err := b.Loaded(ctx)
	if err != nil || len(l) != 1 || l[0].SizeBytes != 3900000000 || l[0].Context != 8192 || l[0].ExpiresAt.IsZero() {
		t.Errorf("loaded = %+v %v", l, err)
	}
	e, err := b.Embed(ctx, backends.EmbedRequest{Model: "nomic-embed-text", Input: []string{"a", "b"}})
	if err != nil || len(e) != 2 || e[1][1] != 0.4 {
		t.Errorf("embed = %v %v", e, err)
	}
	var _ backends.CtxLoader = b
}

// TestLive talks to the real Ollama on this machine with the smallest installed chat model. It
// loads and unloads that model; it never pulls or deletes anything.
func TestLive(t *testing.T) {
	url := os.Getenv("NOSKILL_LIVE_OLLAMA")
	if url == "" {
		t.Skip("set NOSKILL_LIVE_OLLAMA=http://127.0.0.1:11434 to run against a real Ollama")
	}
	b := New(url, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := b.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := b.ListModels(ctx)
	if err != nil || len(ms) == 0 {
		t.Fatalf("%v %v", ms, err)
	}
	model := os.Getenv("NOSKILL_LIVE_MODEL")
	if model == "" {
		model = "llama3.2:3b"
	}
	found := false
	for _, m := range ms {
		found = found || m.Name == model
	}
	if !found {
		t.Skipf("%s isn't installed", model)
	}
	t.Logf("Ollama %s, %d models", v, len(ms))
	no := false
	zero := time.Duration(0)
	s, err := b.Chat(ctx, backends.ChatRequest{Model: model, Think: &no, KeepAlive: &zero,
		Messages: []backends.Message{{Role: backends.RoleUser, Content: "Reply with the single word: pong"}},
		Tools:    []backends.ToolDef{{Name: "get_time", Description: "Current time", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
		Options:  backends.Options{NumCtx: 4096, NumPredict: 64}})
	if err != nil {
		t.Fatal(err)
	}
	var first time.Duration
	t0 := time.Now()
	r, err := backends.Collect(s, func(c backends.ChatChunk) {
		if first == 0 && (c.Content != "" || len(c.ToolCalls) > 0) {
			first = time.Since(t0)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%q tool_calls=%d usage=%+v first token after %v", r.Content, len(r.ToolCalls), r.Usage, first)
	if r.Usage.PromptTokens == 0 || r.Usage.CompletionTokens == 0 {
		t.Errorf("usage missing: %+v", r.Usage)
	}
}

func TestPullAndDelete(t *testing.T) {
	var deleted map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/pull":
			io.WriteString(w, `{"status":"pulling manifest"}`+"\n")
			io.WriteString(w, `{"status":"downloading","total":100,"completed":25}`+"\n")
			io.WriteString(w, `{"status":"downloading","total":100,"completed":25}`+"\n") // repeat: not reported twice
			io.WriteString(w, `{"status":"downloading","total":100,"completed":100}`+"\n")
			io.WriteString(w, `{"status":"success"}`+"\n")
		case r.URL.Path == "/api/delete" && r.Method == http.MethodDelete:
			json.NewDecoder(r.Body).Decode(&deleted)
		}
	})
	var seen []string
	if err := b.Pull(context.Background(), "qwen3:4b", func(s string) { seen = append(seen, s) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "|") != "pulling manifest|downloading 25%|downloading 100%|success" {
		t.Errorf("progress = %v", seen)
	}
	if err := b.Delete(context.Background(), "old:1b"); err != nil || deleted["model"] != "old:1b" {
		t.Errorf("delete: %v %v", deleted, err)
	}
	var _ backends.Manager = b
	var _ backends.CtxLoader = b
	fail := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"pulling manifest"}`+"\n"+`{"error":"pull model manifest: file does not exist"}`+"\n")
	})
	if err := fail.Pull(context.Background(), "nope", nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("pull error: %v", err)
	}
	cut := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"downloading","total":10,"completed":1}`+"\n")
	})
	if err := cut.Pull(context.Background(), "x", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a pull that ends without success must fail: %v", err)
	}
}

func TestTemplateAlwaysThinks(t *testing.T) {
	thinkOnly := "{{- range $i, $_ := .Messages }}{{ if eq .Role \"assistant\" }}<|im_start|>assistant\n{{ if (and $.IsThinkSet .Thinking) -}}<think>{{ .Thinking }}</think>{{ end }}{{ end }}" +
		"{{- if and (ne .Role \"assistant\") $last }}<|im_start|>assistant\n<think>\n{{ end }}{{- end }}"
	hybrid := "{{ if and $.IsThinkSet (not $.Think) }}/no_think{{ end }}<|im_start|>assistant\n<think>\n"
	plain := "{{ .Prompt }}<|im_start|>assistant\n"
	if !templateAlwaysThinks(thinkOnly) || templateAlwaysThinks(hybrid) || templateAlwaysThinks(plain) || templateAlwaysThinks("") {
		t.Error("template detection")
	}
}
