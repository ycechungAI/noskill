package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// JEV calls a decision model through Ollama's /v1/systemone (TypeSafe Jev API, Ollama ≥ 0.35).
// The model stays resident (keep_alive -1, ADR-0006); the orchestrator reloads it when its prompt
// cache passes decision_cache_gb.
type JEV struct {
	BaseURL string // e.g. http://127.0.0.1:11434
	Model   string // e.g. tev1:0.8b
	Client  *http.Client
}

type jevRequest struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]Question `json:"questions"`
	KeepAlive int                 `json:"keep_alive"`
}

type jevResponse struct {
	Answers map[string]struct {
		Choice        *string            `json:"choice"`
		Noul          *float64           `json:"noul"`
		Score         *float64           `json:"score"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	} `json:"answers"`
	Usage Usage  `json:"usage"`
	Error string `json:"error"`
}

func (j JEV) Decide(ctx context.Context, state map[string]any, qs map[string]Question) (Answers, error) {
	if err := Validate(qs); err != nil {
		return Answers{}, err
	}
	body, err := json.Marshal(jevRequest{Model: j.Model, State: state, Questions: qs, KeepAlive: -1})
	if err != nil {
		return Answers{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(j.BaseURL, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Answers{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := j.Client
	if c == nil {
		c = http.DefaultClient
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return Answers{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Answers{}, err
	}
	var r jevResponse
	_ = json.Unmarshal(raw, &r)
	// 404: the endpoint (Ollama < 0.35) or the model is missing; emulate instead.
	if resp.StatusCode == http.StatusNotFound {
		return Answers{}, fmt.Errorf("%w: %s %s", ErrUnavailable, j.Model, strings.TrimSpace(r.Error))
	}
	if resp.StatusCode != http.StatusOK {
		msg := r.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return Answers{}, fmt.Errorf("decide %s: status %d: %s", j.Model, resp.StatusCode, msg)
	}
	out := Answers{Model: j.Model, Answers: map[string]Answer{}, Usage: r.Usage, Elapsed: time.Since(t0)}
	for name, q := range qs {
		a, ok := r.Answers[name]
		if !ok {
			return Answers{}, fmt.Errorf("decide %s: no answer for %s", j.Model, name)
		}
		ans := Answer{Type: q.Type, Probabilities: a.Probabilities, Confidence: a.Confidence}
		switch q.Type {
		case Choice:
			if a.Choice == nil {
				return Answers{}, fmt.Errorf("decide %s: %s has no choice", j.Model, name)
			}
			ans.Choice = *a.Choice
		case Noul:
			if a.Noul == nil {
				return Answers{}, fmt.Errorf("decide %s: %s has no noul", j.Model, name)
			}
			ans.Noul = *a.Noul
		case Score:
			if a.Score == nil {
				return Answers{}, fmt.Errorf("decide %s: %s has no score", j.Model, name)
			}
			ans.Score = *a.Score
		}
		out.Answers[name] = ans
	}
	return out, nil
}
