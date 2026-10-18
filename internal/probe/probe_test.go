package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
)

// expected finds the task a request is about and what a perfect model would do.
func expected(req backends.ChatRequest) task {
	user := req.Messages[len(req.Messages)-1].Content
	for _, t := range tasks {
		if t.prompt == user {
			return t
		}
	}
	panic("unknown task " + user)
}

func perfectCall(t task) (name, args string) {
	return t.tool, fmt.Sprintf(`{%q:%q}`, t.arg, "about "+t.want)
}

// model builds a fake: nativeRight/constrainedRight say how many of the 10 tasks it gets right in
// each mode. Wrong means: a tool call on a no-tool task, a wrong tool otherwise.
func model(nativeRight, constrainedRight int) *backendtest.Fake {
	n, c := 0, 0
	return &backendtest.Fake{Reply: func(req backends.ChatRequest) (backends.Response, error) {
		t := expected(req)
		native := len(req.Tools) > 0
		var right bool
		if native {
			n++
			right = n <= nativeRight
		} else {
			c++
			right = c <= constrainedRight
		}
		name, args := perfectCall(t)
		if !right {
			if t.tool == "" {
				name, args = "web_search", `{"query":"x"}`
			} else {
				name, args = "get_time", `{"timezone":"nowhere"}`
				if t.tool == "get_time" {
					name = "read_file"
					args = `{"path":"z"}`
				}
			}
		}
		if native {
			if t.tool == "" && right {
				return backends.Response{Content: "an answer"}, nil
			}
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: name, Args: json.RawMessage(args)}}}, nil
		}
		if t.tool == "" && right {
			return backends.Response{Content: `{"final":"an answer"}`}, nil
		}
		return backends.Response{Content: fmt.Sprintf(`{"thought":"t","action":{"tool":%q,"args":%s}}`, name, args)}, nil
	}}
}

var withTools = backends.ModelInfo{Name: "m", Capabilities: []backends.Capability{backends.CapChat, backends.CapTools, backends.CapSchema}}

func TestNativeStaysDefault(t *testing.T) {
	// Constrained is only 5 points better (can't happen with 10 tasks: 10-point steps); equal here.
	res, err := Run(context.Background(), model(9, 9), withTools, 4096)
	if err != nil || res.Strategy != backends.StrategyNative {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Native.Passed != 9 || res.Constrained.Passed != 9 {
		t.Errorf("scores = %+v %+v", res.Native, res.Constrained)
	}
}

func TestConstrainedNeedsTenPointsMore(t *testing.T) {
	res, _ := Run(context.Background(), model(5, 6), withTools, 4096)
	if res.Strategy != backends.StrategyConstrained {
		t.Errorf("10 points better must switch: %+v", res)
	}
	res, _ = Run(context.Background(), model(10, 10), withTools, 4096)
	if res.Strategy != backends.StrategyNative {
		t.Errorf("%+v", res)
	}
	res, _ = Run(context.Background(), model(10, 0), withTools, 4096)
	if res.Strategy != backends.StrategyNative || res.Constrained.Passed != 0 {
		t.Errorf("%+v", res)
	}
}

func TestNoNativeToolsMeansConstrained(t *testing.T) {
	info := backends.ModelInfo{Name: "plain", Capabilities: []backends.Capability{backends.CapChat, backends.CapSchema}}
	f := model(0, 8)
	res, err := Run(context.Background(), f, info, 4096)
	if err != nil || res.Strategy != backends.StrategyConstrained || res.Native != nil {
		t.Fatalf("%+v %v", res, err)
	}
	for _, r := range f.Requests {
		if len(r.Tools) > 0 || r.Format == nil || !strings.Contains(r.Messages[0].Content, "web_search") {
			t.Fatal("constrained requests must carry the schema and the tool menu, not native tools")
		}
	}
}

func TestNoSchemaMeansPrompted(t *testing.T) {
	info := backends.ModelInfo{Name: "bare", Capabilities: []backends.Capability{backends.CapChat}}
	f := &backendtest.Fake{}
	res, err := Run(context.Background(), f, info, 4096)
	if err != nil || res.Strategy != backends.StrategyPrompted || f.Calls() != 0 {
		t.Fatalf("%+v %v calls=%d", res, err, f.Calls())
	}
}

func TestMalformedOutputCountsAsErrorNotCrash(t *testing.T) {
	f := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) {
		return backends.Response{Content: "not json at all"}, nil
	}}
	info := backends.ModelInfo{Name: "m", Capabilities: []backends.Capability{backends.CapChat, backends.CapSchema}}
	res, err := Run(context.Background(), f, info, 4096)
	if err != nil || res.Constrained.Errors != 10 || res.Constrained.Passed != 0 {
		t.Fatalf("%+v %v", res.Constrained, err)
	}
}

func TestMissingModelAborts(t *testing.T) {
	f := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) {
		return backends.Response{}, &backends.Error{Backend: "ollama", Status: 404, Msg: "model not found"}
	}}
	if _, err := Run(context.Background(), f, withTools, 4096); err == nil {
		t.Fatal("a missing model must stop the probe, not score 0/10")
	}
}
