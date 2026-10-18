// Package schema checks tool arguments against the JSON Schema subset tool definitions use
// (SKILL_FORMAT.md). It also repairs the usual small-model slips: numbers and booleans sent as
// strings, and missing optional arguments that have a default.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Normalize returns args with defaults filled and top-level strings coerced to the declared
// type, or an error written for the model to read and retry from. An empty schema accepts any
// object. Unknown properties are kept: skills decide whether they matter.
func Normalize(schema, args json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %v", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("arguments must be a JSON object, got %s", typeOf(v))
	}
	if len(schema) == 0 {
		return args, nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, fmt.Errorf("tool schema is invalid: %v", err)
	}
	v = coerce(s, v, true)
	if errs := validate(s, v, ""); len(errs) > 0 {
		if len(errs) > 3 {
			errs = append(errs[:3], fmt.Sprintf("and %d more", len(errs)-3))
		}
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return json.Marshal(v)
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func typesOf(s map[string]any) []string {
	switch t := s["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if str, ok := x.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func matchesType(want string, v any) bool {
	got := typeOf(v)
	switch want {
	case "number":
		return got == "number" || got == "integer"
	case "integer":
		return got == "integer"
	}
	return got == want
}

// coerce repairs scalar mistakes. Only the top level and nested objects are touched; arrays of
// scalars are left alone.
func coerce(s map[string]any, v any, top bool) any {
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for name, ps := range props {
			sub, _ := ps.(map[string]any)
			if sub == nil {
				continue
			}
			cur, present := x[name]
			if !present || cur == nil {
				if d, ok := sub["default"]; ok {
					x[name] = d
				}
				continue
			}
			x[name] = coerce(sub, cur, false)
		}
		return x
	case string:
		ts := typesOf(s)
		if len(ts) == 0 || matchesAny(ts, v) {
			return v
		}
		t := strings.TrimSpace(x)
		for _, want := range ts {
			switch want {
			case "integer", "number":
				if f, err := strconv.ParseFloat(t, 64); err == nil && (want == "number" || f == math.Trunc(f)) {
					return f
				}
			case "boolean":
				switch strings.ToLower(t) {
				case "true", "yes":
					return true
				case "false", "no":
					return false
				}
			}
		}
	}
	return v
}

func matchesAny(ts []string, v any) bool {
	for _, t := range ts {
		if matchesType(t, v) {
			return true
		}
	}
	return false
}

func validate(s map[string]any, v any, path string) []string {
	var errs []string
	name := path
	if name == "" {
		name = "arguments"
	} else {
		name = `"` + path + `"`
	}
	if ts := typesOf(s); len(ts) > 0 && !matchesAny(ts, v) {
		return []string{fmt.Sprintf("%s must be %s, got %s", name, strings.Join(ts, " or "), typeOf(v))}
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s must be one of %s", name, joinAny(enum)))
		}
	}
	switch x := v.(type) {
	case float64:
		if m, ok := s["minimum"].(float64); ok && x < m {
			errs = append(errs, fmt.Sprintf("%s must be at least %v", name, m))
		}
		if m, ok := s["maximum"].(float64); ok && x > m {
			errs = append(errs, fmt.Sprintf("%s must be at most %v", name, m))
		}
	case string:
		if m, ok := s["minLength"].(float64); ok && float64(len([]rune(x))) < m {
			errs = append(errs, fmt.Sprintf("%s must have at least %v characters", name, m))
		}
		if m, ok := s["maxLength"].(float64); ok && float64(len([]rune(x))) > m {
			errs = append(errs, fmt.Sprintf("%s must have at most %v characters", name, m))
		}
	case []any:
		if m, ok := s["minItems"].(float64); ok && float64(len(x)) < m {
			errs = append(errs, fmt.Sprintf("%s needs at least %v items", name, m))
		}
		if m, ok := s["maxItems"].(float64); ok && float64(len(x)) > m {
			errs = append(errs, fmt.Sprintf("%s takes at most %v items", name, m))
		}
		if is, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				errs = append(errs, validate(is, e, fmt.Sprintf("%s[%d]", strings.Trim(name, `"`), i))...)
			}
		}
	case map[string]any:
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if key, _ := r.(string); key != "" {
					if val, present := x[key]; !present || val == nil {
						errs = append(errs, fmt.Sprintf("argument %q is required", key))
					}
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, _ := props[k].(map[string]any)
			if val, present := x[k]; present && sub != nil && val != nil {
				p := k
				if path != "" {
					p = path + "." + k
				}
				errs = append(errs, validate(sub, val, p)...)
			}
		}
		if ap, ok := s["additionalProperties"].(bool); ok && !ap {
			for k := range x {
				if _, known := props[k]; !known {
					errs = append(errs, fmt.Sprintf("unknown argument %q", k))
				}
			}
		}
	}
	return errs
}

func joinAny(vs []any) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}
