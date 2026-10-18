package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Literal formats the current value of a settable key as a TOML literal ("30m" quoted, numbers
// bare, string lists as arrays). Call it after Set on a scratch Config.
func Literal(c Config, key string) (string, bool) {
	var idx []int
	walk(reflect.TypeOf(Config{}), "", func(k string, i []int) {
		if k == key {
			idx = i
		}
	})
	if idx == nil {
		return "", false
	}
	switch x := reflect.ValueOf(c).FieldByIndex(idx).Interface().(type) {
	case Duration:
		return strconv.Quote(x.String()), true
	case AutoFloat:
		if x.Auto {
			return `"auto"`, true
		}
		return strconv.FormatFloat(x.Value, 'f', -1, 64), true
	case string:
		return strconv.Quote(x), true
	case bool:
		return strconv.FormatBool(x), true
	case int:
		return strconv.Itoa(x), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	}
	return "", false
}

// EditText sets `key = literal` inside [section] of a TOML document and leaves everything else —
// comments, order, spacing — as it was. A missing key is added at the end of its section; a
// missing section at the end of the file. A trailing comment on the replaced line is kept.
func EditText(text, section, key, literal string) string {
	lines := strings.Split(text, "\n")
	header := "[" + section + "]"
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(stripComment(l)) == header {
			start = i
			break
		}
	}
	if start < 0 {
		text = strings.TrimRight(text, "\n")
		if text != "" {
			text += "\n\n"
		}
		return text + header + "\n" + key + " = " + literal + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[[") || strings.HasPrefix(t, "[[") {
			end = i
			break
		}
	}
	last := start
	for i := start + 1; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if t != "" && !strings.HasPrefix(t, "#") {
			last = i
		}
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key && !strings.HasPrefix(t, "#") {
			comment := ""
			if c := commentOf(lines[i]); c != "" {
				comment = "  " + c
			}
			lines[i] = key + " = " + literal + comment
			return strings.Join(lines, "\n")
		}
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, key+" = "+literal)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}

// stripComment removes a trailing # comment that isn't inside a string.
func stripComment(l string) string {
	if i := commentIndex(l); i >= 0 {
		return l[:i]
	}
	return l
}

func commentOf(l string) string {
	if i := commentIndex(l); i >= 0 {
		return strings.TrimSpace(l[i:])
	}
	return ""
}

func commentIndex(l string) int {
	inStr := byte(0)
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case inStr != 0:
			if c == '\\' && inStr == '"' {
				i++
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '#':
			return i
		}
	}
	return -1
}

// SplitKey splits "orchestrator.preset" into section and key; "skills.files.allow" into
// "skills.files" and "allow".
func SplitKey(k string) (section, key string, err error) {
	i := strings.LastIndex(k, ".")
	if i <= 0 || i == len(k)-1 {
		return "", "", fmt.Errorf("%q isn't a key like section.name", k)
	}
	return k[:i], k[i+1:], nil
}
