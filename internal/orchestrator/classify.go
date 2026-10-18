// Package orchestrator picks a model for each request (ORCHESTRATOR.md, FR-O1–FR-O3, FR-O6).
package orchestrator

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"noskill/internal/decide"
)

// Request is what the router knows about one request or sub-task.
type Request struct {
	Text        string
	Images      int    // attachments that need a vision model
	Tools       bool   // the run will offer tools
	InputTokens int    // estimated prompt size
	SkillType   string // task type declared by an already-selected skill, if any
}

// Classification is the classifier's answer (FR-O1).
type Classification struct {
	TaskType   string
	Confidence float64 // 0–1; -1 when the source gives none (emulated decisions)
	// Uncertain is set when the best tier was below the threshold. Task-type rules and soft swaps
	// don't fire on an uncertain type; hard needs still do (ORCHESTRATOR.md "Classification").
	Uncertain  bool
	Needs      []string // hard needs: vision, web, tools, context
	Difficulty float64  // 0-based position on difficultyScale; -1 when not asked
	Source     string   // heuristic | skill | decision | emulated | default
	Model      string   // the model that answered task_type (Source decision or emulated)
	Threshold  float64  // the confidence threshold it was held to
	Elapsed    time.Duration
	Note       string
}

func (c Classification) Has(need string) bool { return slices.Contains(c.Needs, need) }

// Classifier runs the tiers cheapest first and stops at the first confident answer. The
// embedding-kNN tier needs the outcomes store and arrives with learning (block 1b).
type Classifier struct {
	Decider   decide.Decider // JEV with emulated fallback; nil = heuristics only
	Threshold float64        // default 0.6
	// ThresholdFor overrides Threshold per deciding model once calibration has tuned it (ROADMAP
	// Phase 4); nil or a result ≤ 0 keeps Threshold.
	ThresholdFor  func(model string) float64
	AskDifficulty bool // add the difficulty question (used to decide think)
}

var taskDescriptions = map[string]string{
	"chat":      "Conversation, opinions, quick questions answerable from general knowledge",
	"code":      "Write, change, explain, review or debug source code",
	"reasoning": "Multi-step logic, analysis or comparing options",
	"math":      "Calculations, equations, proofs",
	"research":  "Needs current or external information looked up on the web",
	"summarise": "Shorten or summarise given text, a page or a document",
	"extract":   "Pull structured fields or lists out of given text",
	"translate": "Translate between languages",
	"creative":  "Stories, poems, names, marketing copy",
	"plan":      "Break a goal into steps, schedule or organise work",
}

var difficultyScale = []string{"Trivial", "Easy", "Moderate", "Hard", "Very hard"}

var (
	reFence    = regexp.MustCompile("(?m)^\\s*```")
	reSrcFile  = regexp.MustCompile(`(?i)\b[\w-]+\.(go|py|js|jsx|ts|tsx|rs|java|kt|swift|c|cc|cpp|h|hpp|cs|rb|php|sh|ps1|sql|lua)\b`)
	reCodeVerb = regexp.MustCompile(`(?i)\b(fix|refactor|implement|debug|compile|write (a|the) (function|script|test|class)|unit test|stack ?trace)\b`)
	reURL      = regexp.MustCompile(`(?i)\bhttps?://\S+`)
	reFresh    = regexp.MustCompile(`(?i)\b(latest|today|tonight|this week|right now|current (price|version|status)|news)\b`)
)

func (c Classifier) Classify(ctx context.Context, r Request) (Classification, error) {
	start := time.Now()
	th := c.Threshold
	if th == 0 {
		th = 0.6
	}
	out := Classification{Confidence: -1, Difficulty: -1}
	if r.Tools {
		out.Needs = append(out.Needs, "tools")
	}
	if reURL.MatchString(r.Text) || reFresh.MatchString(r.Text) {
		out.Needs = append(out.Needs, "web")
	}
	done := func(t string, conf float64, src string) (Classification, error) {
		out.TaskType, out.Confidence, out.Source, out.Elapsed = t, conf, src, time.Since(start)
		return out, nil
	}

	// Tier 1: heuristics.
	switch {
	case r.Images > 0:
		out.Needs = append(out.Needs, "vision")
		return done("vision", 1, "heuristic")
	case r.SkillType != "":
		return done(r.SkillType, 0.95, "skill")
	case reFence.MatchString(r.Text):
		return done("code", 0.9, "heuristic")
	case reSrcFile.MatchString(r.Text) && reCodeVerb.MatchString(r.Text):
		return done("code", 0.8, "heuristic")
	}

	// Tier 3 (and 4 through decide.Fallback): one decision call.
	if c.Decider == nil {
		out.Uncertain = true
		out.Note = "no decision model; heuristics found nothing"
		return done("chat", 0, "default")
	}
	qs := map[string]decide.Question{
		"task_type": {Type: decide.Choice, Instructions: "What kind of task is this request?", Choices: taskDescriptions},
	}
	if c.AskDifficulty {
		qs["difficulty"] = decide.Question{Type: decide.Score, Instructions: "How hard is this request for a small language model?", Scale: difficultyScale}
	}
	text := r.Text
	if len(text) > 4000 { // the question only needs the gist; keeps the decision call near 250 ms
		text = text[:4000]
	}
	a, err := c.Decider.Decide(ctx, map[string]any{"request": text}, qs)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return out, err
		}
		out.Uncertain = true
		out.Note = "decision failed: " + err.Error()
		return done("chat", 0, "default")
	}
	tt := a.Answers["task_type"]
	if d, ok := a.Answers["difficulty"]; ok {
		out.Difficulty = d.Score
	}
	if c.ThresholdFor != nil {
		if t := c.ThresholdFor(a.Model); t > 0 {
			th = t
		}
	}
	out.Model, out.Threshold = a.Model, th
	src, conf := "decision", -1.0
	if a.Emulated {
		src = "emulated"
	}
	switch {
	case tt.Confidence != nil:
		conf = *tt.Confidence
	case tt.Probabilities[tt.Choice] > 0:
		conf = tt.Probabilities[tt.Choice]
	}
	// An emulated answer without a confidence is the last tier, so it's accepted as is; otherwise
	// rules would never fire for people without a decision model.
	if conf >= 0 && conf < th {
		out.Uncertain = true
		out.Note = "task type below the confidence threshold"
	}
	return done(tt.Choice, conf, src)
}

// ThinkFor says whether to turn thinking on: difficulty Moderate or harder (ORCHESTRATOR.md
// "Default routing table"). nil when difficulty wasn't asked.
func (c Classification) ThinkFor() *bool {
	if c.Difficulty < 0 {
		return nil
	}
	t := c.Difficulty >= 1.5
	return &t
}

func needsString(n []string) string { return strings.Join(n, ",") }
