package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var numberRe = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?`)

// Verdict says whether an answer passed and, when not, why in one line.
func (c Check) Verdict(answer string) (bool, string) {
	a := strings.TrimSpace(answer)
	low := strings.ToLower(a)
	if a == "" {
		return false, "empty answer"
	}
	for _, s := range c.Contains {
		if !strings.Contains(low, strings.ToLower(s)) {
			return false, fmt.Sprintf("missing %q", s)
		}
	}
	if len(c.Any) > 0 {
		ok := false
		for _, s := range c.Any {
			if strings.Contains(low, strings.ToLower(s)) {
				ok = true
				break
			}
		}
		if !ok {
			return false, fmt.Sprintf("none of %q", c.Any)
		}
	}
	for _, s := range c.Not {
		if strings.Contains(low, strings.ToLower(s)) {
			return false, fmt.Sprintf("contains %q", s)
		}
	}
	for _, r := range c.Regex {
		re, err := regexp.Compile(r)
		if err != nil {
			return false, "bad regex in the task: " + err.Error()
		}
		if !re.MatchString(a) {
			return false, fmt.Sprintf("doesn't match /%s/", r)
		}
	}
	for _, r := range c.NotRegex {
		re, err := regexp.Compile(r)
		if err != nil {
			return false, "bad regex in the task: " + err.Error()
		}
		if re.MatchString(a) {
			return false, fmt.Sprintf("matches /%s/", r)
		}
	}
	if c.Number != nil {
		n, ok := lastNumber(a)
		if !ok {
			return false, "no number in the answer"
		}
		if math.Abs(n-*c.Number) > 1e-6*math.Max(1, math.Abs(*c.Number)) {
			return false, fmt.Sprintf("number %v, want %v", n, *c.Number)
		}
	}
	if c.MaxWords > 0 {
		if n := len(strings.Fields(a)); n > c.MaxWords {
			return false, fmt.Sprintf("%d words, at most %d", n, c.MaxWords)
		}
	}
	if c.Lines > 0 {
		n := 0
		for _, l := range strings.Split(a, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		if n != c.Lines {
			return false, fmt.Sprintf("%d lines, want %d", n, c.Lines)
		}
	}
	if len(c.JSON) > 0 {
		obj, ok := findObject(a)
		if !ok {
			return false, "no JSON object in the answer"
		}
		for k, want := range c.JSON {
			if !matchValue(obj[k], want) {
				return false, fmt.Sprintf("json %s = %v, want %v", k, obj[k], want)
			}
		}
	}
	return true, ""
}

// lastNumber is the last number in the text; thousands separators are dropped.
func lastNumber(s string) (float64, bool) {
	all := numberRe.FindAllString(s, -1)
	if len(all) == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(all[len(all)-1], ",", ""), 64)
	return n, err == nil
}

// findObject returns the first JSON object in the text (models wrap it in prose or code fences).
func findObject(s string) (map[string]any, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var m map[string]any
		if dec.Decode(&m) == nil {
			return m, true
		}
	}
	return nil, false
}

// matchValue: numbers compare as numbers, strings as case-insensitive substrings (so "widget"
// accepts "widgets"), arrays need every expected element, with '#' ignored.
func matchValue(got, want any) bool {
	switch w := want.(type) {
	case int:
		return matchValue(got, float64(w))
	case float64:
		switch g := got.(type) {
		case float64:
			return math.Abs(g-w) < 1e-6
		case string:
			n, err := strconv.ParseFloat(strings.TrimSpace(g), 64)
			return err == nil && math.Abs(n-w) < 1e-6
		}
		return false
	case string:
		g, ok := got.(string)
		return ok && strings.Contains(strings.ToLower(g), strings.ToLower(w))
	case []any:
		g, ok := got.([]any)
		if !ok {
			return false
		}
		for _, x := range w {
			found := false
			for _, y := range g {
				if gs, ok := y.(string); ok {
					y = strings.TrimPrefix(gs, "#")
				}
				if matchValue(y, x) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return false
}

// Verdict checks the files an agent task left in dir.
func (e End) Verdict(dir string) (bool, string) {
	for _, p := range e.Exists {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			return false, p + " doesn't exist"
		}
	}
	for _, p := range e.Absent {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			return false, p + " should be gone"
		}
	}
	for p, s := range e.FileHas {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || !strings.Contains(strings.ToLower(string(b)), strings.ToLower(s)) {
			return false, fmt.Sprintf("%s doesn't contain %q", p, s)
		}
	}
	for p, s := range e.FileLacks {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return false, p + " is missing"
		}
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(s)) {
			return false, fmt.Sprintf("%s shouldn't contain %q", p, s)
		}
	}
	return true, ""
}

// compile validates the regexes, so a bad task fails when it is loaded, not when it runs.
func (c Check) compile() (int, error) {
	n := 0
	for _, r := range append(append([]string{}, c.Regex...), c.NotRegex...) {
		if _, err := regexp.Compile(r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
