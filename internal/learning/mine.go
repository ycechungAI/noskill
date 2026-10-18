package learning

import (
	"strings"
	"time"

	"noskill/internal/store"
)

// Mining rules (LEARNING.md §2, FR-L2).
const (
	MinConfidence = 0.7                 // only success this sure feeds synthesis
	RepeatCount   = 3                   // the same tool pattern this many times …
	RepeatWindow  = 30 * 24 * time.Hour // … within this window
	minPatternLen = 2                   // one tool call isn't a procedure worth a skill
)

// MineRules are the configurable thresholds ([learning] min_tool_calls, novelty_threshold).
type MineRules struct {
	MinToolCalls int
	Novelty      float64
}

// Worth says why a finished run should be drafted into a skill, or "" when it shouldn't.
// coverage is the best match of the run's request against existing skills (learned ones included),
// and repeats is how many recent runs share its tool pattern.
func Worth(run store.Run, coverage float64, repeats int, r MineRules) string {
	if run.NoLearn || run.Status != "done" || run.Outcome != "success" || run.OutcomeConf < MinConfidence {
		return ""
	}
	if run.ToolCalls >= r.MinToolCalls && coverage < r.Novelty {
		return "novel"
	}
	if repeats >= RepeatCount && strings.Count(run.ToolPattern, ">")+1 >= minPatternLen {
		return "repeated"
	}
	return ""
}
