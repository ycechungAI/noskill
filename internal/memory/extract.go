package memory

import (
	"regexp"
	"strings"
)

// Candidate is a fact found in something the user said.
type Candidate struct {
	Key, Value string
	Source     string // explicit | extracted
	Confidence float64
}

var (
	reRemember = regexp.MustCompile(`(?i)^(?:please\s+)?remember(?:\s+that)?[:,]?\s+(.{3,300})$`)
	reName     = regexp.MustCompile(`(?i)\bmy name is ([\p{L}][\p{L}'-]{1,30}(?: [\p{L}][\p{L}'-]{1,30})?)`)
	rePrefer   = regexp.MustCompile(`(?i)^i (?:really |strongly )?(prefer|like|love|hate|dislike|always use|usually use|use|work (?:in|with|on)|live in) (.{2,200})$`)
	reRule     = regexp.MustCompile(`(?i)^(?:please\s+)?(always|never) (.{3,200})$`)
)

// Extract finds durable facts in a user's message: "remember that…" (explicit), their name, stated
// preferences, and standing instructions ("always answer in French"). It reads only the user's own
// words, never tool output or web pages, so a page can't plant a memory. Anything credential-like
// is dropped.
func Extract(userText string) []Candidate {
	var out []Candidate
	add := func(c Candidate) {
		if LooksSecret(c.Key + " " + c.Value) {
			return
		}
		out = append(out, c)
	}
	for _, sent := range sentences(userText) {
		s := strings.TrimSpace(sent)
		switch {
		case reRemember.MatchString(s):
			v := reRemember.FindStringSubmatch(s)[1]
			add(Candidate{Key: keyFor(v), Value: strings.TrimRight(v, ".!"), Source: "explicit", Confidence: 1})
		case reRule.MatchString(s):
			m := reRule.FindStringSubmatch(s)
			add(Candidate{Key: strings.ToLower(m[1]) + " " + keyFor(m[2]), Value: strings.ToLower(m[1]) + " " + strings.TrimRight(m[2], ".!"), Source: "extracted", Confidence: 0.6})
		case rePrefer.MatchString(s):
			m := rePrefer.FindStringSubmatch(s)
			add(Candidate{Key: "i " + strings.ToLower(m[1]) + " " + keyFor(m[2]), Value: "User " + strings.ToLower(m[1]) + " " + strings.TrimRight(m[2], ".!"), Source: "extracted", Confidence: 0.6})
		}
		if m := reName.FindStringSubmatch(s); m != nil {
			add(Candidate{Key: "name", Value: "The user's name is " + m[1], Source: "extracted", Confidence: 0.8})
		}
	}
	return out
}

func sentences(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for _, part := range regexp.MustCompile(`[.!?]\s+`).Split(line, -1) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// keyFor makes a short stable key from the first few words of a fact.
func keyFor(v string) string {
	w := strings.Fields(strings.ToLower(v))
	if len(w) > 5 {
		w = w[:5]
	}
	return strings.Trim(strings.Join(w, " "), ".,;:!?\"'")
}
