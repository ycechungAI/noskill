package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

const search = `{"type":"object","required":["query"],"properties":{
  "query":{"type":"string","minLength":1},
  "max_results":{"type":"integer","default":5,"minimum":1,"maximum":10},
  "recency":{"type":"string","enum":["day","week","month","year"]},
  "deep":{"type":"boolean"},
  "tags":{"type":"array","items":{"type":"string"},"maxItems":2}}}`

func norm(t *testing.T, args string) (map[string]any, error) {
	out, err := Normalize(json.RawMessage(search), json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m, nil
}

func TestDefaultsAndCoercion(t *testing.T) {
	m, err := norm(t, `{"query":"ollama","max_results":"3","deep":"yes"}`)
	if err != nil || m["max_results"] != 3.0 || m["deep"] != true {
		t.Fatalf("%v %v", m, err)
	}
	m, err = norm(t, `{"query":"x"}`)
	if err != nil || m["max_results"] != 5.0 {
		t.Fatalf("default not filled: %v %v", m, err)
	}
	m, _ = norm(t, `{"query":"x","extra":1}`)
	if m["extra"] != 1.0 {
		t.Errorf("unknown arguments must be kept: %v", m)
	}
	if m, err := norm(t, `null`); err == nil {
		t.Errorf("null args with a required field accepted: %v", m)
	}
}

func TestErrorsAreReadableForTheModel(t *testing.T) {
	for want, args := range map[string]string{
		`argument "query" is required`:              `{}`,
		`"max_results" must be at most 10`:          `{"query":"x","max_results":50}`,
		`"max_results" must be integer, got string`: `{"query":"x","max_results":"many"}`,
		`"recency" must be one of day, week`:        `{"query":"x","recency":"decade"}`,
		`"query" must have at least 1`:              `{"query":""}`,
		`takes at most 2 items`:                     `{"query":"x","tags":["a","b","c"]}`,
		`"tags[1]" must be string`:                  `{"query":"x","tags":["a",2]}`,
		`must be a JSON object`:                     `[1]`,
		`not valid JSON`:                            `{"query":`,
	} {
		_, err := norm(t, args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", args, err)
		}
	}
	// An integer field refuses 2.5 instead of rounding it.
	if _, err := norm(t, `{"query":"x","max_results":2.5}`); err == nil {
		t.Error("2.5 accepted as integer")
	}
}

func TestEmptySchemaAcceptsAnyObject(t *testing.T) {
	out, err := Normalize(nil, json.RawMessage(`{"a":1}`))
	if err != nil || string(out) != `{"a":1}` {
		t.Fatalf("%s %v", out, err)
	}
	if _, err := Normalize(nil, json.RawMessage(`"str"`)); err == nil {
		t.Error("non-object accepted")
	}
	out, _ = Normalize(nil, nil)
	if string(out) != "{}" {
		t.Errorf("empty args = %s", out)
	}
}

func TestAdditionalPropertiesFalse(t *testing.T) {
	_, err := Normalize(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`), json.RawMessage(`{"a":"x","b":1}`))
	if err == nil || !strings.Contains(err.Error(), `unknown argument "b"`) {
		t.Errorf("%v", err)
	}
}
