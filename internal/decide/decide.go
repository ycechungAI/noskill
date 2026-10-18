// Package decide is the `decide` primitive (FR-B6, FR-B6a): typed questions about an input,
// answered with probabilities in one call. Served by a JEV decision model through Ollama's
// /v1/systemone, or emulated by a chat model with constrained JSON when none is installed.
// Router classification, skill rerank, outcome judging, the injection screen and skills all use it.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Type string

const (
	Choice Type = "choice" // pick one label
	Noul   Type = "noul"   // yes/no: a number, P(yes) on tev1, a score on tev1:0.8b (thresholds per model)
	Score  Type = "score"  // a position on an ordered scale
)

// Question is one typed question. Choices is for choice (label → description), Scale for score
// (lowest → highest).
type Question struct {
	Type         Type
	Instructions string
	Choices      map[string]string
	Scale        []string
}

// MarshalJSON writes the Jev API form: criteria is a map for choice and a list for score.
func (q Question) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": q.Type, "instructions": q.Instructions}
	switch q.Type {
	case Choice:
		m["criteria"] = q.Choices
	case Score:
		m["criteria"] = q.Scale
	}
	return json.Marshal(m)
}

type Answer struct {
	Type          Type               `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"` // 0-based expected position on Scale
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is nil when unknown (emulated without logprobs); callers then use their
	// conservative defaults (MODEL_ADAPTERS.md "Fallback").
	Confidence *float64 `json:"confidence,omitempty"`
}

// Level returns the score rounded to the nearest scale label.
func (a Answer) Level(q Question) string {
	i := int(a.Score + 0.5)
	if i < 0 || i >= len(q.Scale) {
		return ""
	}
	return q.Scale[i]
}

type Answers struct {
	Model    string            `json:"model"`
	Emulated bool              `json:"emulated"`
	Answers  map[string]Answer `json:"answers"`
	Usage    Usage             `json:"usage"`
	Elapsed  time.Duration     `json:"elapsed"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Decider interface {
	Decide(ctx context.Context, state map[string]any, qs map[string]Question) (Answers, error)
}

// ErrUnavailable means this decider can't serve decisions here (no decision model, or Ollama is
// older than 0.35); Fallback then emulates.
var ErrUnavailable = errors.New("decision model unavailable")

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate checks questions before they're sent.
func Validate(qs map[string]Question) error {
	if len(qs) == 0 {
		return errors.New("decide: no questions")
	}
	for name, q := range qs {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("decide: question name %q must be lower_snake_case", name)
		}
		if q.Instructions == "" {
			return fmt.Errorf("decide: %s has no instructions", name)
		}
		switch q.Type {
		case Choice:
			if len(q.Choices) < 2 {
				return fmt.Errorf("decide: choice %s needs at least 2 options", name)
			}
		case Score:
			if len(q.Scale) < 2 {
				return fmt.Errorf("decide: score %s needs at least 2 levels", name)
			}
		case Noul:
		default:
			return fmt.Errorf("decide: %s has unknown type %q", name, q.Type)
		}
	}
	return nil
}

// Fallback serves decisions from Primary (the JEV model) and emulates with Secondary when the
// primary is unavailable. A nil Primary always emulates.
type Fallback struct {
	Primary   Decider
	Secondary Decider
}

func (f Fallback) Decide(ctx context.Context, state map[string]any, qs map[string]Question) (Answers, error) {
	if f.Primary != nil {
		a, err := f.Primary.Decide(ctx, state, qs)
		if err == nil || !errors.Is(err, ErrUnavailable) || f.Secondary == nil {
			return a, err
		}
	}
	if f.Secondary == nil {
		return Answers{}, ErrUnavailable
	}
	return f.Secondary.Decide(ctx, state, qs)
}
