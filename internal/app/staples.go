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

// RecommendedChat are chat models the Auto fill picker offers to download, best first. The page
// shows them a few at a time. qwen3.5 and qwen3 aren't here: qwen3.5 is excluded, and qwen3 always
// reasons, which is too slow for a form fill (AGENTS.md).
var RecommendedChat = []Staple{
	{"granite4:3b", "main model: chat and tool use, fast on a laptop", 2.1},
	{"gemma3:1b", "small and quick: plain chat and summaries", 0.8},
	{"llama3.2:3b", "small general chat model", 2.0},
	{"qwen2.5:3b", "small general chat model with good instruction following", 1.9},
	{"gemma3:4b", "general chat model, larger than gemma3:1b", 3.3},
	{"qwen2.5-coder:7b", "code-focused chat model", 4.7},
	{"mistral:7b", "general chat model at 7B", 4.1},
	{"phi4-mini", "small reasoning-light model for quick answers", 2.5},
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
