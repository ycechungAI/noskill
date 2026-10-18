package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/backends"
)

// Emulated answers decisions with a chat model and a JSON schema built from the questions
// (FR-B6a). It's ~10× slower than a JEV model (S3) and has no calibrated confidence: Confidence
// stays nil, and callers use their conservative defaults.
type Emulated struct {
	Backend backends.Backend
	Model   string
	NumCtx  int
}

func (e Emulated) Decide(ctx context.Context, state map[string]any, qs map[string]Question) (Answers, error) {
	if err := Validate(qs); err != nil {
		return Answers{}, err
	}
	names := make([]string, 0, len(qs))
	for n := range qs {
		names = append(names, n)
	}
	sort.Strings(names)
	schema, err := emulationSchema(names, qs)
	if err != nil {
		return Answers{}, err
	}
	stateJSON, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return Answers{}, err
	}
	var prompt strings.Builder
	prompt.WriteString("Answer each question about the input below. Reply with JSON only.\n\nInput:\n")
	prompt.Write(stateJSON)
	prompt.WriteString("\n\nQuestions:\n")
	for _, n := range names {
		q := qs[n]
		fmt.Fprintf(&prompt, "- %s (%s): %s\n", n, q.Type, q.Instructions)
		switch q.Type {
		case Choice:
			labels := sortedKeys(q.Choices)
			for _, l := range labels {
				fmt.Fprintf(&prompt, "    %s: %s\n", l, q.Choices[l])
			}
		case Score:
			fmt.Fprintf(&prompt, "    one of, lowest to highest: %s\n", strings.Join(q.Scale, ", "))
		case Noul:
			prompt.WriteString("    true or false\n")
		}
	}
	no := false
	zero := 0.0
	t0 := time.Now()
	s, err := e.Backend.Chat(ctx, backends.ChatRequest{
		Model:    e.Model,
		Messages: []backends.Message{{Role: backends.RoleUser, Content: prompt.String()}},
		Format:   schema,
		Think:    &no,
		Options:  backends.Options{NumCtx: e.NumCtx, NumPredict: 64 + 32*len(qs), Temperature: &zero},
	})
	if err != nil {
		return Answers{}, err
	}
	resp, err := backends.Collect(s, nil)
	if err != nil {
		return Answers{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &raw); err != nil {
		return Answers{}, fmt.Errorf("emulated decide on %s: not JSON: %v", e.Model, err)
	}
	out := Answers{Model: e.Model, Emulated: true, Answers: map[string]Answer{}, Elapsed: time.Since(t0),
		Usage: Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}}
	for _, n := range names {
		q := qs[n]
		v, ok := raw[n]
		if !ok {
			return Answers{}, fmt.Errorf("emulated decide on %s: no answer for %s", e.Model, n)
		}
		a := Answer{Type: q.Type}
		switch q.Type {
		case Choice:
			s, _ := v.(string)
			if _, ok := q.Choices[s]; !ok {
				return Answers{}, fmt.Errorf("emulated decide on %s: %s = %v is not an option", e.Model, n, v)
			}
			a.Choice, a.Probabilities = s, map[string]float64{s: 1}
		case Noul:
			b, ok := v.(bool)
			if !ok {
				return Answers{}, fmt.Errorf("emulated decide on %s: %s = %v is not true/false", e.Model, n, v)
			}
			if b {
				a.Noul = 1
			}
		case Score:
			s, _ := v.(string)
			i := slices.Index(q.Scale, s)
			if i < 0 {
				return Answers{}, fmt.Errorf("emulated decide on %s: %s = %v is not on the scale", e.Model, n, v)
			}
			a.Score, a.Probabilities = float64(i), map[string]float64{s: 1}
		}
		out.Answers[n] = a
	}
	return out, nil
}

// emulationSchema: enum of labels for choice and score, boolean for noul, all required.
func emulationSchema(names []string, qs map[string]Question) (json.RawMessage, error) {
	props := map[string]any{}
	for _, n := range names {
		q := qs[n]
		switch q.Type {
		case Choice:
			props[n] = map[string]any{"type": "string", "enum": sortedKeys(q.Choices)}
		case Score:
			props[n] = map[string]any{"type": "string", "enum": q.Scale}
		case Noul:
			props[n] = map[string]any{"type": "boolean"}
		}
	}
	return json.Marshal(map[string]any{"type": "object", "properties": props, "required": names, "additionalProperties": false})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
