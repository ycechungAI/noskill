package decide

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
)

var routerQs = map[string]Question{
	"task_type": {Type: Choice, Instructions: "What kind of task is this?",
		Choices: map[string]string{"code": "Write or change code", "chat": "Simple conversation"}},
	"needs_web":  {Type: Noul, Instructions: "Does answering need current information from the internet?"},
	"difficulty": {Type: Score, Instructions: "How hard is this task?", Scale: []string{"Trivial", "Easy", "Moderate", "Hard", "Very hard"}},
}

func TestJEVRequestAndAnswers(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"answers":{
			"task_type":{"choice":"code","probabilities":{"code":0.93,"chat":0.07},"confidence":0.9},
			"needs_web":{"noul":0.12},
			"difficulty":{"score":2.4,"probabilities":{"Moderate":0.6},"confidence":0.4}},
			"usage":{"input_tokens":975,"output_tokens":3}}`))
	}))
	defer srv.Close()
	j := JEV{BaseURL: srv.URL, Model: "tev1:0.8b"}
	a, err := j.Decide(context.Background(), map[string]any{"request": "refactor this"}, routerQs)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "tev1:0.8b" || got["keep_alive"] != float64(-1) {
		t.Errorf("request = %v", got)
	}
	tt := got["questions"].(map[string]any)["task_type"].(map[string]any)
	if _, ok := tt["criteria"].(map[string]any); !ok {
		t.Errorf("choice criteria should be a map: %v", tt)
	}
	diff := got["questions"].(map[string]any)["difficulty"].(map[string]any)
	if _, ok := diff["criteria"].([]any); !ok {
		t.Errorf("score criteria should be a list: %v", diff)
	}
	if a.Answers["task_type"].Choice != "code" || *a.Answers["task_type"].Confidence != 0.9 {
		t.Errorf("task_type = %+v", a.Answers["task_type"])
	}
	if a.Answers["needs_web"].Noul != 0.12 || a.Answers["difficulty"].Level(routerQs["difficulty"]) != "Moderate" {
		t.Errorf("answers = %+v", a.Answers)
	}
	if a.Usage.InputTokens != 975 {
		t.Errorf("usage = %+v", a.Usage)
	}
}

func TestJEV404IsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"model 'tev1:0.8b' not found"}`))
	}))
	defer srv.Close()
	_, err := JEV{BaseURL: srv.URL, Model: "tev1:0.8b"}.Decide(context.Background(), nil, routerQs)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestEmulatedUsesSchemaAndParses(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{{Content: `{"difficulty":"Hard","needs_web":true,"task_type":"code"}`}}}
	e := Emulated{Backend: fake, Model: "qwen3:4b", NumCtx: 8192}
	a, err := e.Decide(context.Background(), map[string]any{"request": "x"}, routerQs)
	if err != nil {
		t.Fatal(err)
	}
	req := fake.Requests[0]
	if req.Think == nil || *req.Think || req.Format == nil || req.Options.NumCtx != 8192 {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(string(req.Format), `"enum":["chat","code"]`) {
		t.Errorf("schema = %s", req.Format)
	}
	if !a.Emulated || a.Answers["task_type"].Choice != "code" || a.Answers["needs_web"].Noul != 1 || a.Answers["difficulty"].Score != 3 {
		t.Errorf("answers = %+v", a)
	}
	if a.Answers["task_type"].Confidence != nil {
		t.Error("emulated confidence should be unknown (nil)")
	}
}

func TestEmulatedRejectsOffSchemaAnswers(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{{Content: `{"difficulty":"Hard","needs_web":true,"task_type":"poetry"}`}}}
	if _, err := (Emulated{Backend: fake, Model: "m"}).Decide(context.Background(), nil, routerQs); err == nil {
		t.Fatal("answer outside the options accepted")
	}
}

func TestFallbackEmulatesWhenUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	fake := &backendtest.Fake{Script: []backends.Response{{Content: `{"difficulty":"Easy","needs_web":false,"task_type":"chat"}`}}}
	f := Fallback{Primary: JEV{BaseURL: srv.URL, Model: "tev1:0.8b"}, Secondary: Emulated{Backend: fake, Model: "qwen3:4b"}}
	a, err := f.Decide(context.Background(), nil, routerQs)
	if err != nil || !a.Emulated {
		t.Fatalf("a = %+v, err = %v", a, err)
	}
}

func TestValidate(t *testing.T) {
	for name, qs := range map[string]map[string]Question{
		"bad name":     {"Task-Type": {Type: Noul, Instructions: "x"}},
		"one choice":   {"t": {Type: Choice, Instructions: "x", Choices: map[string]string{"a": "a"}}},
		"no text":      {"t": {Type: Noul}},
		"unknown type": {"t": {Type: "rank", Instructions: "x"}},
	} {
		if Validate(qs) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
