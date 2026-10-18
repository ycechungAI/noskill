package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"noskill/internal/app"
	"noskill/internal/backends"
	"noskill/internal/backendtest"
)

const decisionReq = `{"model":%q,"state":{"file":"main.go"},"questions":{
 "lang":{"type":"choice","instructions":"Which language?","criteria":{"go":"Go","py":"Python"}},
 "code":{"type":"noul","instructions":"Is it source code?"},
 "size":{"type":"score","instructions":"How big?","criteria":["small","big"]}}}`

// FR-B6b: a chat model answers /v1/systemone by emulation, in the Jev wire form.
func TestSystemoneEmulatedOnAChatModel(t *testing.T) {
	fake := answerFake(`{"lang":"go","code":true,"size":"big"}`)
	g := newRig(t, fake, nil)
	resp, body := g.do("POST", "/v1/systemone", strings.Replace(decisionReq, "%q", `"qwen3:4b"`, 1), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var out struct {
		Model    string
		Emulated bool
		Answers  map[string]map[string]any
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	a := out.Answers
	if !out.Emulated || out.Model != "qwen3:4b" || a["lang"]["choice"] != "go" || a["code"]["noul"] != 1.0 || a["size"]["score"] != 1.0 {
		t.Fatalf("answers = %s", body)
	}
	if l, _ := a["size"]["legend"].(map[string]any); l["0"] != "small" || l["1"] != "big" {
		t.Errorf("legend = %v", a["size"]["legend"])
	}
	if len(fake.Requests) != 1 || len(fake.Requests[0].Format) == 0 || !strings.Contains(fake.Requests[0].Messages[0].Content, "main.go") {
		t.Errorf("the chat model wasn't asked with a schema and the state: %+v", fake.Requests)
	}

	// Errors: an unknown model is a 404, a malformed question a 400.
	if resp, body := g.do("POST", "/v1/systemone", strings.Replace(decisionReq, "%q", `"nope:1b"`, 1), nil); resp.StatusCode != 404 {
		t.Errorf("unknown model: %d %s", resp.StatusCode, body)
	}
	bad := `{"model":"qwen3:4b","questions":{"x":{"type":"score","instructions":"?","criteria":{"a":"b"}}}}`
	if resp, body := g.do("POST", "/v1/systemone", bad, nil); resp.StatusCode != 400 {
		t.Errorf("bad criteria: %d %s", resp.StatusCode, body)
	}
}

// FR-B6b: a JEV model's request goes, unchanged, to the Ollama backend that has it, even when
// another backend comes first.
func TestSystemoneForwardsToTheDecisionModel(t *testing.T) {
	var path, got string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, got = r.URL.Path, string(b)
		io.WriteString(w, `{"answers":{"code":{"noul":0.97}},"usage":{"input_tokens":30,"output_tokens":1}}`)
	}))
	defer ollama.Close()
	chat := answerFake("MUST NOT BE USED")
	jev := &backendtest.Fake{Models: []backends.ModelInfo{{ID: "ollama/tev1:0.8b", Name: "tev1:0.8b", Capabilities: []backends.Capability{backends.CapDecide}}}}
	g := newRig(t, chat, nil,
		app.NamedBackend{ID: "fake", Kind: "fake", B: chat},
		app.NamedBackend{ID: "ollama", Kind: "ollama", URL: ollama.URL, B: jev})
	req := strings.Replace(decisionReq, "%q", `"tev1:0.8b"`, 1)
	resp, body := g.do("POST", "/v1/systemone", req, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"noul":0.97`) || chat.Calls() != 0 {
		t.Fatalf("%d %s chat calls=%d", resp.StatusCode, body, chat.Calls())
	}
	if path != "/v1/systemone" || got != req {
		t.Errorf("forwarded %s %s", path, got)
	}
}
