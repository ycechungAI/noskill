package app

import "strings"

// Staple is a model noskill is built and tested around. The status page offers to pull the ones that
// aren't installed (FR-B1); pulling is the only place noskill downloads weights, and only when the
// user presses the button.
type Staple struct {
	Name   string  `json:"name"`
	Role   string  `json:"role"`
	SizeGB float64 `json:"size_gb"` // approximate download
}

// Staples are the models noskill recommends. qwen3.5 isn't here on purpose (see orchestrator.exclude).
var Staples = []Staple{
	{"granite4:3b", "main model: chat and tool use", 2.1},
	{"gemma3:1b", "backup model: summaries and extraction, no tools", 0.8},
	{"tev1:0.8b", "decision (JEV) model: fast typed answers for routing and judging", 1.5},
	{"nomic-embed-text", "embedding model: skill selection and document search", 0.3},
}

// missingStaples are the staples none of the installed models match.
func missingStaples(have []ModelStatus) []Staple {
	var out []Staple
	for _, s := range Staples {
		found := false
		for _, m := range have {
			if m.Name == s.Name || strings.TrimSuffix(m.Name, ":latest") == s.Name {
				found = true
			}
		}
		if !found {
			out = append(out, s)
		}
	}
	return out
}
