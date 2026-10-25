package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"noskill/internal/app"
	"noskill/internal/approvals"
	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/config"
	"noskill/internal/platform"
	"noskill/internal/scheduler"
	"noskill/internal/secrets"
	"noskill/internal/tools"
)

func TestMain(m *testing.M) {
	platform.MaybeRunSandboxChild()
	os.Exit(m.Run())
}

type quiet struct{}

func (quiet) SupportsActions() bool                               { return false }
func (quiet) Notify(context.Context, platform.Notification) error { return nil }

func oneModel() []backends.ModelInfo {
	return []backends.ModelInfo{{ID: "fake/qwen3:4b", Backend: "fake", Name: "qwen3:4b", ParamsB: 4, Context: 32768, SizeBytes: 2.5e9,
		Capabilities: []backends.Capability{backends.CapChat, backends.CapTools, backends.CapSchema}}}
}

type rig struct {
	t    *testing.T
	app  *app.App
	fake *backendtest.Fake
	ts   *httptest.Server
	dir  string
}

func newRig(t *testing.T, fake *backendtest.Fake, mutate func(*config.Config), nbs ...app.NamedBackend) *rig {
	t.Helper()
	return newRigWith(t, fake, mutate, nil, nbs...)
}

func newRigWith(t *testing.T, fake *backendtest.Fake, mutate func(*config.Config), seed map[string]string, nbs ...app.NamedBackend) *rig {
	t.Helper()
	home := t.TempDir()
	l, err := config.Load(config.Options{File: filepath.Join(home, "config.toml")})
	if err != nil {
		t.Fatal(err)
	}
	l.Config.Orchestrator.Rules = nil
	if mutate != nil {
		mutate(&l.Config)
	}
	if fake.Models == nil {
		fake.Models = oneModel()
	}
	if nbs == nil {
		nbs = []app.NamedBackend{{ID: "fake", Kind: "fake", B: fake}}
	}
	sm := secrets.NewManager(&secrets.Mem{}, filepath.Join(home, "names"))
	for k, v := range seed {
		sm.Set(k, v)
	}
	a, err := app.Open(context.Background(), app.Options{
		Dirs: platform.Dirs{Config: home, Data: home}, Loaded: l, Secrets: sm,
		Backends: nbs, Notifier: quiet{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s, err := New(a, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &rig{t: t, app: a, fake: fake, ts: ts, dir: home}
}

func (g *rig) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	g.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = strings.NewReader(string(j))
	}
	req, _ := http.NewRequest(method, g.ts.URL+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func answerFake(text string) *backendtest.Fake {
	return &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: text}, nil }}
}

func TestLivenessAndModelLists(t *testing.T) {
	g := newRigWith(t, answerFake("x"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "owui", Token: "secret:OWUI", Skills: []string{"*"}}}
	}, map[string]string{"OWUI": "owui-token-0123456789"})
	for _, m := range []string{"GET", "HEAD"} {
		if resp, body := g.do(m, "/", nil, nil); resp.StatusCode != 200 || (m == "GET" && !strings.Contains(string(body), "noskill is running")) {
			t.Errorf("%s /: %d %q", m, resp.StatusCode, body)
		}
	}
	if resp, _ := g.do("GET", "/v1/models", nil, nil); resp.StatusCode != 401 {
		t.Errorf("models without token: %d", resp.StatusCode)
	}
	auth := map[string]string{"Authorization": "Bearer owui-token-0123456789"}
	resp, body := g.do("GET", "/v1/models", nil, auth)
	var list struct{ Data []struct{ ID string } }
	json.Unmarshal(body, &list)
	var ids []string
	for _, d := range list.Data {
		ids = append(ids, d.ID)
	}
	if resp.StatusCode != 200 || strings.Join(ids, ",") != "noskill/auto,noskill/fast,noskill/best,noskill/local-docs,noskill/research,noskill/watcher,qwen3:4b" {
		t.Errorf("/v1/models = %d %v", resp.StatusCode, ids)
	}
	resp, body = g.do("GET", "/api/tags", nil, auth)
	var tags struct {
		Models []struct{ Name, Digest string }
	}
	json.Unmarshal(body, &tags)
	if resp.StatusCode != 200 || len(tags.Models) != 7 || tags.Models[0].Name != "noskill/auto" {
		t.Fatalf("/api/tags = %d %s", resp.StatusCode, body)
	}
	for _, m := range tags.Models {
		if len(m.Digest) != 64 {
			t.Errorf("%s digest %q: `ollama list` crashes below 12 characters", m.Name, m.Digest)
		}
	}
	resp, body = g.do("POST", "/api/show", map[string]string{"model": "noskill/auto"}, auth)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"capabilities"`) {
		t.Errorf("/api/show noskill/auto: %d %s", resp.StatusCode, body)
	}
}

func TestOpenAIChatWholeAndStreamed(t *testing.T) {
	g := newRig(t, answerFake("Paris."), nil)
	req := map[string]any{"model": "noskill/auto", "messages": []map[string]string{{"role": "user", "content": "Capital of France?"}}}
	resp, body := g.do("POST", "/v1/chat/completions", req, nil)
	var out struct {
		ID      string
		Object  string
		Model   string
		Choices []struct {
			Message      struct{ Role, Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			Total int `json:"total_tokens"`
		}
	}
	json.Unmarshal(body, &out)
	if resp.StatusCode != 200 || out.Choices[0].Message.Content != "Paris." || out.Choices[0].FinishReason != "stop" || out.Object != "chat.completion" || out.Model != "noskill/auto" || out.Usage.Total == 0 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Noskill-Run-Id") == "" || resp.Header.Get("X-Noskill-Model") != "qwen3:4b" || !strings.HasSuffix(out.ID, resp.Header.Get("X-Noskill-Run-Id")) {
		t.Errorf("headers = %v id=%s", resp.Header, out.ID)
	}

	req["stream"] = true
	req["stream_options"] = map[string]bool{"include_usage": true}
	resp, body = g.do("POST", "/v1/chat/completions", req, nil)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %s", ct)
	}
	var content string
	var sawRole, sawDone, sawFinish, sawUsage bool
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			sawDone = true
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Role             string
					Content          string
					ReasoningContent string `json:"reasoning_content"`
				}
				FinishReason *string `json:"finish_reason"`
			}
			Usage *struct {
				Total int `json:"total_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		if ch.Usage != nil {
			sawUsage = ch.Usage.Total > 0
		}
		for _, c := range ch.Choices {
			sawRole = sawRole || c.Delta.Role == "assistant"
			content += c.Delta.Content
			sawFinish = sawFinish || (c.FinishReason != nil && *c.FinishReason == "stop")
		}
	}
	if content != "Paris." || !sawRole || !sawFinish || !sawDone || !sawUsage {
		t.Errorf("stream: content=%q role=%v finish=%v done=%v usage=%v\n%s", content, sawRole, sawFinish, sawDone, sawUsage, body)
	}
}

func TestOllamaChatAndGenerate(t *testing.T) {
	g := newRig(t, answerFake("Hello there."), nil)
	// Streaming is the default in Ollama's API.
	resp, body := g.do("POST", "/api/chat", map[string]any{"model": "noskill/auto", "messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("content-type = %s", resp.Header.Get("Content-Type"))
	}
	var text string
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var m struct {
			Message struct{ Content string }
			Done    bool
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q", line)
		}
		text += m.Message.Content
		json.Unmarshal([]byte(line), &last)
	}
	if text != "Hello there." || last["done"] != true || last["done_reason"] != "stop" || last["eval_count"] == nil {
		t.Errorf("stream: %q %v", text, last)
	}
	resp, body = g.do("POST", "/api/chat", map[string]any{"model": "noskill/auto", "stream": false, "messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil)
	var whole struct {
		Message struct{ Role, Content string }
		Done    bool
	}
	json.Unmarshal(body, &whole)
	if resp.StatusCode != 200 || whole.Message.Content != "Hello there." || !whole.Done {
		t.Errorf("whole chat: %s", body)
	}
	// `ollama run model "prompt"` uses /api/generate.
	_, body = g.do("POST", "/api/generate", map[string]any{"model": "noskill/auto", "prompt": "hi", "system": "Be brief.", "stream": false}, nil)
	var gen struct {
		Response string
		Done     bool
	}
	json.Unmarshal(body, &gen)
	if gen.Response != "Hello there." || !gen.Done {
		t.Errorf("generate: %s", body)
	}
	if sys := g.fake.Requests[len(g.fake.Requests)-1].Messages[0].Content; !strings.Contains(sys, "Be brief.") {
		t.Errorf("generate's system prompt was lost: %q", sys)
	}
}

func TestProgressAndToolsStream(t *testing.T) {
	fake := &backendtest.Fake{}
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		if r.Messages[len(r.Messages)-1].Role == backends.RoleTool {
			return backends.Response{Content: "It is 0.40 [1]."}, nil
		}
		return backends.Response{Content: "let me look", ToolCalls: []backends.ToolCall{{Name: "search", Args: json.RawMessage(`{"query":"ollama release"}`)}}}, nil
	}
	for mode, check := range map[string]func(reasoning, content string) bool{
		"thinking": func(r, c string) bool {
			return strings.Contains(r, "Searching: ollama release") && c == "It is 0.40 [1]."
		},
		"think_tags": func(r, c string) bool {
			return r == "" && strings.HasPrefix(c, "<think>\n") && strings.Contains(c, "Searching: ollama release\n") && strings.HasSuffix(c, "</think>\nIt is 0.40 [1].")
		},
		"content": func(r, c string) bool {
			return r == "" && strings.Contains(c, "Searching: ollama release\n\n") && strings.HasSuffix(c, "It is 0.40 [1].")
		},
		"off": func(r, c string) bool { return r == "" && c == "It is 0.40 [1]." },
	} {
		t.Run(mode, func(t *testing.T) {
			g := newRig(t, fake, func(c *config.Config) { c.Server.Progress = mode })
			g.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: g.app.Registry["web.search"].Schema,
				Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) {
					return `[{"title":"R","url":"https://x.example"}]`, nil
				}}
			resp, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/research", "stream": true,
				"messages": []map[string]string{{"role": "user", "content": "what is the latest ollama release"}}}, nil)
			var reasoning, content string
			for _, line := range strings.Split(string(body), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
					var ch struct {
						Choices []struct {
							Delta struct {
								Content string
								RC      string `json:"reasoning_content"`
							}
						}
					}
					json.Unmarshal([]byte(data), &ch)
					for _, c := range ch.Choices {
						content += c.Delta.Content
						reasoning += c.Delta.RC
					}
				}
			}
			if resp.StatusCode != 200 || !check(reasoning, content) {
				t.Errorf("reasoning=%q content=%q\n%s", reasoning, content, body)
			}
			if strings.Contains(content, "let me look") || strings.Contains(reasoning, "let me look") {
				t.Error("text the model wrote before a tool call must not reach the client")
			}
		})
	}
}

func TestModelNameResolutionAndErrors(t *testing.T) {
	g := newRig(t, answerFake("ok"), func(c *config.Config) { c.Server.PassthroughPlainNames = false })
	chat := func(model string) (*http.Response, []byte) {
		return g.do("POST", "/v1/chat/completions", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil)
	}
	if resp, _ := chat("noskill/qwen3:4b"); resp.StatusCode != 200 || resp.Header.Get("X-Noskill-Model") != "qwen3:4b" {
		t.Errorf("pin by model: %v", resp.Header)
	}
	if resp, _ := chat("noskill/qwen3"); resp.StatusCode != 200 && resp.StatusCode != 404 {
		t.Errorf("noskill/<model without tag> = %d", resp.StatusCode)
	}
	resp, body := chat("noskill/nothing")
	var e struct {
		Error struct{ Message, Type string }
	}
	json.Unmarshal(body, &e)
	if resp.StatusCode != 404 || e.Error.Type != "not_found_error" || !strings.Contains(e.Error.Message, "noskill/auto") || !strings.Contains(e.Error.Message, "qwen3:4b") {
		t.Errorf("unknown name: %d %s", resp.StatusCode, body)
	}
	resp, body = g.do("POST", "/api/chat", map[string]any{"model": "noskill/nothing", "messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil)
	var oe struct{ Error string }
	json.Unmarshal(body, &oe)
	if resp.StatusCode != 404 || !strings.Contains(oe.Error, "noskill/auto") {
		t.Errorf("Ollama-format error: %d %s", resp.StatusCode, body)
	}
	if resp, _ := g.do("POST", "/v1/chat/completions", "{not json", nil); resp.StatusCode != 400 {
		t.Errorf("bad json: %d", resp.StatusCode)
	}
	if resp, _ := g.do("POST", "/v1/chat/completions", map[string]any{"model": "x", "messages": []map[string]string{{"role": "wizard", "content": "hi"}}}, nil); resp.StatusCode != 400 {
		t.Errorf("bad role: %d", resp.StatusCode)
	}
	if resp, _ := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/auto"}, nil); resp.StatusCode != 400 {
		t.Errorf("no messages: %d", resp.StatusCode)
	}
	if resp, _ := g.do("GET", "/v1/nope", nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
}

func TestTokenScopesLimitSkills(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "limited", Token: "secret:LIM", Skills: []string{"files"}}}
	}, map[string]string{"LIM": "limited-token-0123456789"})
	auth := map[string]string{"Authorization": "Bearer limited-token-0123456789"}
	resp, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/research", "messages": []map[string]string{{"role": "user", "content": "hi"}}}, auth)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "research") {
		t.Errorf("forced skill outside the token's scope: %d %s", resp.StatusCode, body)
	}
	_, body = g.do("GET", "/v1/models", nil, auth)
	if strings.Contains(string(body), "noskill/research") {
		t.Errorf("a skill the token can't use is listed: %s", body)
	}
}

func TestPlainNamesPassThroughToTheBackend(t *testing.T) {
	var got struct {
		path, auth, origin, host string
		body                     string
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.auth, got.origin, got.host, got.body = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Origin"), r.Host, string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"message":{"content":"a"},"done":false}`)
		fl.Flush()
		fmt.Fprintln(w, `{"message":{"content":"b"},"done":true}`)
	}))
	defer backend.Close()
	fake := answerFake("MUST NOT BE USED")
	g := newRig(t, fake, nil, app.NamedBackend{ID: "ollama", Kind: "ollama", URL: backend.URL, Key: "backend-key", B: fake})
	resp, body := g.do("POST", "/api/chat", `{"model":"qwen3:4b","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer client-token", "Origin": "http://127.0.0.1:" + strings.Split(g.ts.URL, ":")[2]})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"content":"a"`) || fake.Calls() != 0 {
		t.Fatalf("%d %s calls=%d", resp.StatusCode, body, fake.Calls())
	}
	if got.path != "/api/chat" || got.auth != "Bearer backend-key" || got.origin != "" || !strings.HasPrefix(got.host, "127.0.0.1:") || !strings.Contains(got.body, `"qwen3:4b"`) {
		t.Errorf("forwarded request = %+v (the client's token and Origin must not reach the backend)", got)
	}
	// Model management passes through only when allowed.
	g2 := newRig(t, fake, func(c *config.Config) { c.Server.AllowModelManagement = false }, app.NamedBackend{ID: "ollama", Kind: "ollama", URL: backend.URL, B: fake})
	if resp, _ := g2.do("POST", "/api/pull", `{"model":"x"}`, nil); resp.StatusCode != 403 {
		t.Errorf("pull with management off: %d", resp.StatusCode)
	}
	// Backend down → 503 with Retry-After.
	g3 := newRig(t, fake, nil, app.NamedBackend{ID: "ollama", Kind: "ollama", URL: "http://127.0.0.1:1", B: fake})
	resp, _ = g3.do("GET", "/api/ps", nil, nil)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("backend down: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestBackgroundTasksSkipEverything(t *testing.T) {
	g := newRig(t, answerFake("A title"), nil)
	resp, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/auto", "messages": []map[string]string{
		{"role": "user", "content": "### Task:\nGenerate a concise title for the chat history below.\n<chat_history>look up the latest news</chat_history>"}}}, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "A title") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(g.fake.Requests[0].Tools) != 0 {
		t.Error("a background task got tools")
	}
	var src string
	var noLearn bool
	g.app.DB.QueryRow(`SELECT source, no_learn FROM runs`).Scan(&src, &noLearn)
	if src != "background" || !noLearn {
		t.Errorf("run = %s noLearn=%v", src, noLearn)
	}
}

func TestChatApprovalFlow(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "a.mkv")
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	os.WriteFile(src, []byte("episode"), 0o644)
	args, _ := json.Marshal(map[string]any{"plan": []map[string]string{{"from": src, "to": dst}}, "dry_run": false})
	mk := func() *backendtest.Fake {
		return &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
			if r.Messages[len(r.Messages)-1].Role == backends.RoleTool {
				if strings.Contains(r.Messages[len(r.Messages)-1].Content, "declined") {
					return backends.Response{Content: "Understood, nothing moved."}, nil
				}
				return backends.Response{Content: "Moved 1 file."}, nil
			}
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: "move_many", Args: args}}}, nil
		}}
	}
	cfgFn := func(c *config.Config) {
		c.Server.ExposeSkills = []string{"files"}
		c.Skills.Settings = map[string]map[string]any{"files": {"allow": "write:" + work}}
	}
	msgs := func(parts ...string) []map[string]string {
		var out []map[string]string
		for i, p := range parts {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			out = append(out, map[string]string{"role": role, "content": p})
		}
		return out
	}
	ask := func(g *rig, hdr map[string]string, parts ...string) string {
		_, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/files", "messages": msgs(parts...)}, hdr)
		var out struct {
			Choices []struct{ Message struct{ Content string } }
		}
		if json.Unmarshal(body, &out) != nil || len(out.Choices) == 0 {
			t.Fatalf("unexpected response: %s", body)
		}
		return out.Choices[0].Message.Content
	}

	g := newRig(t, mk(), cfgFn)
	reply := ask(g, nil, "move the episode into a Show folder")
	if !strings.Contains(reply, "Reply **yes**") || !strings.Contains(reply, "move_many") {
		t.Fatalf("first reply = %q", reply)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("moved before approval")
	}
	pending, _ := g.app.Approvals.List(context.Background(), false, 0)
	if len(pending) != 1 || pending[0].ConversationFP == "" || pending[0].ResumeToken == "" {
		t.Fatalf("pending = %+v", pending)
	}

	// An unrelated message neither approves nor cancels.
	_ = ask(g, nil, "move the episode into a Show folder", reply, "what's the weather like on mars? also, I think maybe you should go ahead")
	if still, _ := g.app.Approvals.List(context.Background(), false, 0); len(still) < 1 {
		t.Fatal("an unrelated (long) message resolved the approval")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("moved on an unclear reply")
	}
	// Another conversation can't answer it.
	other := ask(g, nil, "something else entirely", "ok", "yes")
	_ = other
	if _, err := os.Stat(src); err != nil {
		t.Fatal("a different conversation's 'yes' approved the move")
	}

	// A plain yes in the same conversation resumes the run and finishes the job.
	final := ask(g, nil, "move the episode into a Show folder", reply, "yes go ahead")
	if final != "Moved 1 file." {
		t.Errorf("after yes: %q", final)
	}
	if got, _ := os.ReadFile(dst); string(got) != "episode" {
		t.Errorf("file not moved after approval: %v", got)
	}
	done, _ := g.app.Approvals.Get(context.Background(), pending[0].ID)
	if done.Status != "approved" || done.AnsweredVia != "chat" {
		t.Errorf("approval = %+v", done)
	}

	// "no" cancels without a model call.
	os.Rename(dst, src)
	g2 := newRig(t, mk(), cfgFn)
	reply2 := ask(g2, nil, "move the episode into a Show folder")
	before := g2.fake.Calls()
	if got := ask(g2, nil, "move the episode into a Show folder", reply2, "No, cancel that."); !strings.Contains(got, "won't do that") {
		t.Errorf("after no: %q", got)
	}
	if g2.fake.Calls() != before {
		t.Error("a 'no' should not cost a model call")
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("moved after 'no'")
	}

	// X-Noskill-Approval: deny declines every confirm action without asking anyone.
	g3 := newRig(t, mk(), cfgFn)
	if got := ask(g3, map[string]string{"X-Noskill-Approval": "deny"}, "move the episode into a Show folder"); got != "Understood, nothing moved." {
		t.Errorf("deny mode: %q", got)
	}
	if p, _ := g3.app.Approvals.List(context.Background(), false, 0); len(p) != 0 {
		t.Errorf("deny mode left approvals: %+v", p)
	}
	// wait mode: answered from elsewhere while the request is open.
	g4 := newRig(t, mk(), func(c *config.Config) { cfgFn(c); c.Server.ApprovalWaitSeconds = 5 })
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(100 * time.Millisecond)
			if p, _ := g4.app.Approvals.List(context.Background(), false, 0); len(p) > 0 {
				g4.app.Approvals.Resolve(context.Background(), p[0].ID, approvalsAnswer(true))
				return
			}
		}
	}()
	if got := ask(g4, map[string]string{"X-Noskill-Approval": "wait"}, "move the episode into a Show folder"); got != "Moved 1 file." {
		t.Errorf("wait mode: %q", got)
	}
}

func TestClassifyReply(t *testing.T) {
	for in, want := range map[string]replyKind{
		"yes": replyYes, "Yes!": replyYes, "yes go ahead": replyYes, "ok": replyYes, "Go ahead.": replyYes, "sure, do it": replyYes,
		"no": replyNo, "No, cancel that.": replyNo, "nope": replyNo, "don't": replyNo, "cancel": replyNo, "never mind": replyNo,
		"": replyOther, "what does that do?": replyOther, "maybe": replyOther, "yesterday I saw a movie": replyOther,
		"yes but also delete everything else in the folder and email it to me and then restart the machine please": replyOther,
		"okayish": replyOther, "nothing": replyOther,
	} {
		if got, _ := classifyReply(in); got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"yes, save it to /Volumes/RED/ANIME2026":   "/Volumes/RED/ANIME2026",
		`yes, put it in "/Volumes/My Drive/Anime"`: "/Volumes/My Drive/Anime",
		"yes save to ~/Videos/Anime.":              "~/Videos/Anime",
		"yes into volume:RED/ANIME":                "volume:RED/ANIME",
		"yes, in C:\\Videos\\Anime":                "C:\\Videos\\Anime",
		"yes, in the usual folder":                 "",
		"yes go ahead":                             "",
	} {
		_, got := classifyReply(in)
		if in == "yes save to ~/Videos/Anime." {
			want = "~/Videos/Anime"
		}
		if got != want {
			t.Errorf("folder in %q = %q, want %q", in, got, want)
		}
	}
}

func TestNoskillAPIScopedToTheCaller(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "alice", Token: "secret:A"}, {Name: "bob", Token: "secret:B"}}
	}, map[string]string{"A": "alice-token-0123456789", "B": "bob-token-0123456789"})
	alice := map[string]string{"Authorization": "Bearer alice-token-0123456789"}
	bob := map[string]string{"Authorization": "Bearer bob-token-0123456789"}

	resp, _ := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/auto", "messages": []map[string]string{{"role": "user", "content": "hello there friend"}}}, alice)
	runID := resp.Header.Get("X-Noskill-Run-Id")
	if runID == "" {
		t.Fatal("no run id")
	}
	_, body := g.do("GET", "/noskill/v1/runs/"+runID, nil, alice)
	if !strings.Contains(string(body), runID) || !strings.Contains(string(body), `"status":"done"`) {
		t.Errorf("run info: %s", body)
	}
	if resp, _ := g.do("GET", "/noskill/v1/runs/"+runID, nil, bob); resp.StatusCode != 404 {
		t.Errorf("bob read alice's run: %d", resp.StatusCode)
	}
	if resp, _ := g.do("POST", "/noskill/v1/feedback", map[string]any{"run_id": runID, "rating": "down"}, bob); resp.StatusCode != 404 {
		t.Errorf("bob rated alice's run: %d", resp.StatusCode)
	}
	resp, body = g.do("POST", "/noskill/v1/feedback", map[string]any{"run_id": runID, "rating": "up", "comment": "nice"}, alice)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"outcome":"success"`) {
		t.Errorf("feedback: %d %s", resp.StatusCode, body)
	}
	// By conversation: the same messages find the same run.
	resp, body = g.do("POST", "/noskill/v1/feedback", map[string]any{"rating": "down", "messages": []map[string]string{
		{"role": "user", "content": "hello there friend"}, {"role": "assistant", "content": "ok"}}}, alice)
	if resp.StatusCode != 200 || !strings.Contains(string(body), runID) {
		t.Errorf("feedback by messages: %d %s", resp.StatusCode, body)
	}
	if resp, _ := g.do("POST", "/noskill/v1/feedback", map[string]any{"run_id": runID, "rating": "meh"}, alice); resp.StatusCode != 400 {
		t.Errorf("bad rating: %d", resp.StatusCode)
	}
	resp, body = g.do("GET", "/noskill/v1/health", nil, alice)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"ok"`) || !strings.Contains(string(body), "sandbox") {
		t.Errorf("health: %s", body)
	}
}

func TestEventsStream(t *testing.T) {
	g := newRig(t, answerFake("ok"), nil)
	req, _ := http.NewRequest("GET", g.ts.URL+"/noskill/v1/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 20)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	if l := <-lines; l != "event: hello" {
		t.Fatalf("first line = %q", l)
	}
	time.Sleep(300 * time.Millisecond)
	g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/auto", "messages": []map[string]string{{"role": "user", "content": "hello there friend"}}}, nil)
	deadline := time.After(6 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			if l == "event: run.finished" {
				return
			}
		case <-deadline:
			t.Fatal("no run.finished event")
		}
	}
}

func approvalsAnswer(approve bool) approvals.Answer {
	return approvals.Answer{Approve: approve, Via: "tray"}
}

func TestStatusPageAndAdminCalls(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "chat", Token: "secret:C"}, {Name: "tray", Token: "secret:T", Admin: true}}
	}, map[string]string{"C": "chat-token-0123456789", "T": "tray-token-0123456789"})
	chat := map[string]string{"Authorization": "Bearer chat-token-0123456789"}
	tray := map[string]string{"Authorization": "Bearer tray-token-0123456789"}

	// The page itself is static and needs no token; the data calls do.
	resp, body := g.do("GET", "/ui", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<title>noskill</title>") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("/ui: %d %s", resp.StatusCode, resp.Header)
	}
	if resp, _ := g.do("GET", "/noskill/v1/status", nil, nil); resp.StatusCode != 401 {
		t.Errorf("status without a token: %d", resp.StatusCode)
	}
	if resp, _ := g.do("GET", "/noskill/v1/status", nil, chat); resp.StatusCode != 403 {
		t.Errorf("status for a chat token: %d", resp.StatusCode)
	}
	resp, body = g.do("GET", "/noskill/v1/status", nil, tray)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"models"`) || !strings.Contains(string(body), `"jobs_paused":false`) {
		t.Errorf("status: %d %s", resp.StatusCode, body)
	}

	// Jobs: pause all, then resume all brings back only the ones it paused.
	ctx := context.Background()
	sch := &scheduler.Store{DB: g.app.DB}
	for _, n := range []string{"a", "b"} {
		if _, err := sch.Add(ctx, scheduler.Job{Name: n, TargetKind: "prompt", Target: "hi", Schedule: "every:1h", Missed: "skip", Overlap: "skip", Enabled: true, NextRunAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if resp, _ := g.do("POST", "/noskill/v1/jobs/a/pause", nil, tray); resp.StatusCode != 200 {
		t.Fatalf("pause: %d", resp.StatusCode)
	}
	if resp, body := g.do("POST", "/noskill/v1/jobs/all/pause", nil, tray); resp.StatusCode != 200 || !strings.Contains(string(body), `"jobs":1`) {
		t.Fatalf("pause all: %d %s", resp.StatusCode, body)
	}
	if resp, body := g.do("POST", "/noskill/v1/jobs/all/resume", nil, tray); resp.StatusCode != 200 || !strings.Contains(string(body), `"jobs":1`) {
		t.Fatalf("resume all: %d %s", resp.StatusCode, body)
	}
	jobs, _ := sch.List(ctx)
	for _, j := range jobs {
		if j.Enabled != (j.Name == "b") {
			t.Errorf("%s enabled=%v", j.Name, j.Enabled)
		}
	}
	if resp, _ := g.do("POST", "/noskill/v1/jobs/b/explode", nil, tray); resp.StatusCode != 400 {
		t.Errorf("unknown action: %d", resp.StatusCode)
	}
	if resp, _ := g.do("POST", "/noskill/v1/jobs/b/pause", nil, chat); resp.StatusCode != 403 {
		t.Errorf("a chat token paused a job: %d", resp.StatusCode)
	}

	// An approval raised by a job is visible to the admin only.
	it, err := g.app.Approvals.Create(ctx, approvals.New{Source: "job", Kind: "confirm", Summary: "move 3 files", Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, body := g.do("GET", "/noskill/v1/approvals", nil, chat); strings.Contains(string(body), it.ID) {
		t.Error("a chat client saw a job's approval")
	}
	if _, body := g.do("GET", "/noskill/v1/approvals", nil, tray); !strings.Contains(string(body), it.ID) {
		t.Errorf("the admin didn't see it: %s", body)
	}
}

// noskill/fast and noskill/best route under that preset, so a rule limited to a preset picks the
// speed model or the quality model for opencode and other clients that choose by model name.
func TestPresetModelNames(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		mu.Lock()
		seen = append(seen, r.Model)
		mu.Unlock()
		return backends.Response{Content: `ok`}, nil
	}}
	quick := oneModel()[0]
	quick.ID, quick.Name = "fake/quick:3b", "quick:3b"
	fake.Models = append(oneModel(), quick)
	g := newRig(t, fake, func(c *config.Config) {
		c.Orchestrator.Rules = []config.Rule{
			{When: config.RuleWhen{Preset: "best"}, Use: "qwen3:4b"},
			{When: config.RuleWhen{Preset: "fast"}, Use: "quick:3b"},
		}
	})
	got := func(name string) string {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		resp, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": name, "messages": []map[string]string{{"role": "user", "content": "hi there"}}}, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, body)
		}
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}
	if m := got("noskill/best"); m != "qwen3:4b" {
		t.Errorf("noskill/best ran %s", m)
	}
	if m := got("noskill/fast"); m != "quick:3b" {
		t.Errorf("noskill/fast ran %s", m)
	}
	if resp, body := g.do("POST", "/v1/chat/completions", map[string]any{"model": "noskill/nope", "messages": []map[string]string{{"role": "user", "content": "hi"}}}, nil); resp.StatusCode != 404 || !strings.Contains(string(body), "noskill/fast") || !strings.Contains(string(body), "noskill/best") {
		t.Errorf("404 should list the preset names: %d %s", resp.StatusCode, body)
	}
}

// The Downloads and History tabs: admin only, a clear message when there is no torrent app, and a
// link that isn't a magnet, https .torrent or Nyaa page is refused before anything is added. The
// tracker is told not to use any torrent app, so the test never touches one on this machine.
func TestDownloadsEndpoints(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "chat", Token: "secret:C"}, {Name: "tray", Token: "secret:T", Admin: true}}
		c.Skills.Settings = map[string]map[string]any{"anime-tracker": {"client": "none", "ask_before_download": "unsure"}}
	}, map[string]string{"C": "chat-token-0123456789", "T": "tray-token-0123456789"})
	chat := map[string]string{"Authorization": "Bearer chat-token-0123456789"}
	tray := map[string]string{"Authorization": "Bearer tray-token-0123456789"}

	for _, c := range []struct{ method, path string }{{"GET", "/noskill/v1/downloads"}, {"POST", "/noskill/v1/downloads"}, {"POST", "/noskill/v1/downloads/file-now"}} {
		if resp, _ := g.do(c.method, c.path, map[string]string{"url": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, nil); resp.StatusCode != 401 {
			t.Errorf("%s %s without a token: %d", c.method, c.path, resp.StatusCode)
		}
		if resp, _ := g.do(c.method, c.path, map[string]string{"url": "x"}, chat); resp.StatusCode != 403 {
			t.Errorf("%s %s for a chat token: %d", c.method, c.path, resp.StatusCode)
		}
	}
	resp, body := g.do("GET", "/noskill/v1/downloads", nil, tray)
	var v struct {
		AskMode  string          `json:"ask_mode"`
		AskFirst bool            `json:"ask_first"`
		Problem  string          `json:"problem"`
		Items    json.RawMessage `json:"items"`
		Finished json.RawMessage `json:"finished"`
		Filed    json.RawMessage `json:"filed"`
		Folders  json.RawMessage `json:"folders"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &v) != nil || !v.AskFirst || v.AskMode != "unsure" || v.Problem == "" || string(v.Items) != "[]" || string(v.Finished) != "[]" || string(v.Filed) != "[]" {
		t.Fatalf("GET downloads: %d %s", resp.StatusCode, body)
	}
	resp, body = g.do("POST", "/noskill/v1/downloads", map[string]string{"url": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, tray)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "no usable torrent app") {
		t.Errorf("no torrent app: %d %s", resp.StatusCode, body)
	}
	if resp, _ = g.do("POST", "/noskill/v1/downloads", map[string]string{"dest": "x"}, tray); resp.StatusCode != 400 {
		t.Errorf("no link: %d", resp.StatusCode)
	}
	resp, body = g.do("POST", "/noskill/v1/downloads/file-now", nil, tray)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "no usable torrent app") {
		t.Errorf("file now without a torrent app: %d %s", resp.StatusCode, body)
	}
	if _, page := g.do("GET", "/ui", nil, nil); !strings.Contains(string(page), `data-tab="history"`) || !strings.Contains(string(page), `id="addf"`) {
		t.Error("the page lacks the tabs or the add form")
	}
}

func TestModelActionsFromTheStatusPage(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "chat", Token: "secret:C"}, {Name: "tray", Token: "secret:T", Admin: true}}
	}, map[string]string{"C": "chat-token-0123456789", "T": "tray-token-0123456789"})
	chat := map[string]string{"Authorization": "Bearer chat-token-0123456789"}
	tray := map[string]string{"Authorization": "Bearer tray-token-0123456789"}

	for _, c := range []struct{ method, path string }{{"GET", "/noskill/v1/tasks"}, {"POST", "/noskill/v1/tasks"}, {"POST", "/noskill/v1/tasks/t1/stop"}} {
		if resp, _ := g.do(c.method, c.path, map[string]string{"kind": "probe", "model": "qwen3:4b"}, nil); resp.StatusCode != 401 {
			t.Errorf("%s %s without a token: %d", c.method, c.path, resp.StatusCode)
		}
		if resp, _ := g.do(c.method, c.path, map[string]string{"kind": "probe", "model": "qwen3:4b"}, chat); resp.StatusCode != 403 {
			t.Errorf("%s %s for a chat token: %d", c.method, c.path, resp.StatusCode)
		}
	}
	if resp, body := g.do("POST", "/noskill/v1/tasks", map[string]string{"kind": "format", "model": "qwen3:4b"}, tray); resp.StatusCode != 400 {
		t.Errorf("unknown kind: %d %s", resp.StatusCode, body)
	}
	if resp, body := g.do("POST", "/noskill/v1/tasks", map[string]string{"kind": "bench", "model": "nope:1b"}, tray); resp.StatusCode != 404 {
		t.Errorf("unknown model: %d %s", resp.StatusCode, body)
	}

	// Models noskill is built around that aren't installed are offered for pulling, and pulling takes
	// any well-formed name: the user decides what fits on the disk.
	_, body := g.do("GET", "/noskill/v1/status", nil, tray)
	if !strings.Contains(string(body), `"missing":[`) || !strings.Contains(string(body), `"name":"tev1:0.8b"`) {
		t.Errorf("the status doesn't offer the missing staples: %s", body)
	}
	if resp, body := g.do("POST", "/noskill/v1/tasks", map[string]string{"kind": "pull", "model": "bad name; rm -rf"}, tray); resp.StatusCode != 400 {
		t.Errorf("a malformed name to pull: %d %s", resp.StatusCode, body)
	}

	// A model in orchestrator.exclude is never loaded or timed from the page either.
	g.app.Cfg.Orchestrator.Exclude = []string{"qwen3:4b"}
	if resp, body := g.do("POST", "/noskill/v1/tasks", map[string]string{"kind": "bench", "model": "qwen3:4b"}, tray); resp.StatusCode != 409 || !strings.Contains(string(body), "orchestrator.exclude") {
		t.Errorf("excluded model: %d %s", resp.StatusCode, body)
	}
	g.app.Cfg.Orchestrator.Exclude = nil

	// A task starts, ends and stays in the list.
	resp, body := g.do("POST", "/noskill/v1/tasks", map[string]string{"kind": "probe", "model": "qwen3:4b"}, tray)
	if resp.StatusCode != 202 {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	var state string
	for i := 0; i < 100 && state != "done" && state != "failed"; i++ {
		time.Sleep(50 * time.Millisecond)
		_, body = g.do("GET", "/noskill/v1/tasks", nil, tray)
		var v struct {
			Tasks []struct{ ID, Kind, Model, State string } `json:"tasks"`
		}
		if json.Unmarshal(body, &v) != nil || len(v.Tasks) != 1 || v.Tasks[0].Kind != "probe" || v.Tasks[0].Model != "qwen3:4b" {
			t.Fatalf("tasks: %s", body)
		}
		state = v.Tasks[0].State
	}
	if state != "done" && state != "failed" {
		t.Fatalf("the probe never finished: %s", body)
	}
}

func TestAddJobFromTheStatusPage(t *testing.T) {
	g := newRigWith(t, answerFake("ok"), func(c *config.Config) {
		c.Server.Tokens = []config.Token{{Name: "chat", Token: "secret:C"}, {Name: "tray", Token: "secret:T", Admin: true}}
	}, map[string]string{"C": "chat-token-0123456789", "T": "tray-token-0123456789"})
	chat := map[string]string{"Authorization": "Bearer chat-token-0123456789"}
	tray := map[string]string{"Authorization": "Bearer tray-token-0123456789"}
	spec := map[string]any{"target": "Summarise my notes folder please", "every": "6h", "notify": []string{"os"}}

	if resp, _ := g.do("POST", "/noskill/v1/jobs", spec, nil); resp.StatusCode != 401 {
		t.Errorf("without a token: %d", resp.StatusCode)
	}
	if resp, _ := g.do("POST", "/noskill/v1/jobs", spec, chat); resp.StatusCode != 403 {
		t.Errorf("for a chat token: %d", resp.StatusCode)
	}
	for name, bad := range map[string]map[string]any{
		"no schedule":     {"target": "Summarise my notes folder please"},
		"both schedules":  {"target": "Summarise my notes folder please", "every": "6h", "cron": "0 3 * * *"},
		"bad cron":        {"target": "Summarise my notes folder please", "cron": "99 * * * *"},
		"unknown skill":   {"target": "nosuchskill", "every": "6h"},
		"unknown channel": {"target": "Summarise my notes folder please", "every": "6h", "notify": []string{"pigeon"}},
		"bad anime stage": {"target": "anime:nope", "every": "6h"},
		"anime + model":   {"target": "anime:download", "every": "6h", "model": "granite4:3b"},
		"bad condition":   {"target": "Summarise my notes folder please", "every": "6h", "when": []string{"full-moon"}},
	} {
		if resp, body := g.do("POST", "/noskill/v1/jobs", bad, tray); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	resp, body := g.do("POST", "/noskill/v1/jobs", spec, tray)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"summarise-my-notes-folder"`) {
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	// An anime job runs the tracker itself: no skill, no model.
	resp, body = g.do("POST", "/noskill/v1/jobs", map[string]any{"target": "anime:download,file", "cron": "0 14 * * 6"}, tray)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"name":"anime-download-file"`) || !strings.Contains(string(body), `"target_kind":"anime"`) {
		t.Fatalf("anime job: %d %s", resp.StatusCode, body)
	}
	_, body = g.do("GET", "/noskill/v1/status", nil, tray)
	if !strings.Contains(string(body), `"name":"summarise-my-notes-folder"`) || !strings.Contains(string(body), "every:6h") {
		t.Errorf("the job isn't in the status: %s", body)
	}
}
