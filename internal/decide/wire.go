package decide

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// The TypeSafe Jev wire form, for the proxy's /v1/systemone (FR-B6b, PROXY.md): a request names a
// model, a state and the questions; the reply has one answer per question and the token usage.

// WireRequest is a /v1/systemone request body.
type WireRequest struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// UnmarshalJSON reads the Jev form: criteria is a label → description map for choice and a list,
// lowest first, for score.
func (q *Question) UnmarshalJSON(b []byte) error {
	var w struct {
		Type         Type            `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*q = Question{Type: w.Type, Instructions: w.Instructions}
	if len(w.Criteria) == 0 || string(w.Criteria) == "null" {
		return nil
	}
	switch w.Type {
	case Choice:
		if err := json.Unmarshal(w.Criteria, &q.Choices); err != nil {
			return fmt.Errorf("choice criteria must be an object of label → description: %w", err)
		}
	case Score:
		if err := json.Unmarshal(w.Criteria, &q.Scale); err != nil {
			return fmt.Errorf("score criteria must be a list of levels, lowest first: %w", err)
		}
	}
	return nil
}

// Wire is the Jev reply for a: choice → choice, probabilities, confidence; noul → noul; score →
// score, legend (position → label), probabilities, confidence. Fields that are unknown (no
// confidence without logprobs) are left out. An emulated reply says so in "emulated".
func (a Answers) Wire(qs map[string]Question) map[string]any {
	ans := map[string]any{}
	for name, x := range a.Answers {
		m := map[string]any{}
		switch x.Type {
		case Choice:
			m["choice"] = x.Choice
		case Noul:
			m["noul"] = x.Noul
		case Score:
			m["score"] = x.Score
			legend := map[string]string{}
			for i, l := range qs[name].Scale {
				legend[strconv.Itoa(i)] = l
			}
			m["legend"] = legend
		}
		if len(x.Probabilities) > 0 {
			m["probabilities"] = x.Probabilities
		}
		if x.Confidence != nil {
			m["confidence"] = *x.Confidence
		}
		ans[name] = m
	}
	out := map[string]any{"model": a.Model, "answers": ans, "usage": a.Usage}
	if a.Emulated {
		out["emulated"] = true
	}
	return out
}
