package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"noskill/internal/decide"
	"noskill/internal/orchestrator"
	"noskill/internal/store"
)

// Decision calibration and threshold tuning (FR-B6c, MODEL_ADAPTERS.md "Calibration").
// Thresholded decisions are stored with their probability; a run's outcome later says whether
// acting on them was right. `noskill calibration` reports how well each model's probabilities
// match, and with orchestrator.auto_tune on, a threshold moves at most once a day, in small steps,
// within fixed limits, and only on clear evidence. Every change is kept and can be reverted.

const (
	DecisionRouter = "router.task_type" // trust the task type (rules fire) when its confidence ≥ threshold
	DecisionJudge  = "judge.answered"   // the judge says the reply answered when P(yes) ≥ threshold
)

// decisionDefaults are the thresholds before any tuning (ORCHESTRATOR.md, judge.go).
var decisionDefaults = map[string]float64{DecisionRouter: 0.6, DecisionJudge: 0.5}

var decisionRules = map[string]decide.TuneRules{
	DecisionRouter: {Lo: 0.3, Hi: 0.9},
	DecisionJudge:  {Lo: 0.2, Hi: 0.8},
}

const (
	calibrationWindow = 90 * 24 * time.Hour
	calibrationLimit  = 500
	tuneEvery         = 24 * time.Hour
	// routerLabelConf is how sure a run's outcome must be to say whether routing on the task type
	// worked; any source counts, since the judges don't judge the router.
	routerLabelConf = 0.7
)

// tuneClock remembers when each (decision, model) was last considered for tuning in this process.
type tuneClock struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (c *tuneClock) due(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	if t, ok := c.last[key]; ok && now.Sub(t) < tuneEvery {
		return false
	}
	c.last[key] = now
	return true
}

// recordDecision stores one decision for calibration. It never fails the run.
func (a *App) recordDecision(ctx context.Context, d store.Decision) {
	if d.RunID == "" || d.Model == "" || d.P < 0 {
		return
	}
	if d.At.IsZero() {
		d.At = time.Now()
	}
	if err := a.DB.AddDecision(ctx, d); err != nil {
		slog.Debug("couldn't record a decision", "name", d.Name, "err", err)
	}
}

// recordRouting stores the router's task-type decision once the run has an ID.
func (a *App) recordRouting(ctx context.Context, runID string, c orchestrator.Classification) {
	if (c.Source != "decision" && c.Source != "emulated") || c.Confidence < 0 {
		return
	}
	a.recordDecision(ctx, store.Decision{RunID: runID, Name: DecisionRouter, Model: c.Model, Emulated: c.Source == "emulated",
		Answer: c.TaskType, P: c.Confidence, Threshold: c.Threshold})
}

// label turns a decision and its run's outcome into a sample, or reports that the outcome can't
// say whether the decision was right.
func label(name string, o store.DecisionOutcome) (decide.Sample, bool) {
	switch name {
	case DecisionRouter:
		// Acting on the task type was right when the run it routed succeeded.
		if o.Outcome == "partial" || o.Conf < routerLabelConf {
			return decide.Sample{}, false
		}
		return decide.Sample{P: o.P, Y: o.Outcome == "success"}, true
	case DecisionJudge:
		// Only outcomes the judges didn't set: your rating, a check, an implicit signal.
		switch o.Source {
		case "explicit", "check", "implicit":
		default:
			return decide.Sample{}, false
		}
		return decide.Sample{P: o.P, Y: o.Outcome != "fail"}, true
	}
	return decide.Sample{}, false
}

func (a *App) decisionSamples(ctx context.Context, name, model string, now time.Time) ([]decide.Sample, error) {
	rows, err := a.DB.DecisionOutcomes(ctx, name, model, now.Add(-calibrationWindow), calibrationLimit)
	if err != nil {
		return nil, err
	}
	var out []decide.Sample
	for _, r := range rows {
		if s, ok := label(name, r); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// decisionThreshold is the threshold in force for a decision by model. With auto_tune on it first
// tunes, at most once a day per (decision, model).
func (a *App) decisionThreshold(ctx context.Context, name, model string) float64 {
	def := decisionDefaults[name]
	if model == "" {
		return def
	}
	if a.Cfg.Orchestrator.AutoTune && a.tuneClock.due(name+"|"+model, time.Now()) {
		if _, err := a.TuneThreshold(ctx, name, model, true, "auto"); err != nil {
			slog.Debug("threshold tuning failed", "name", name, "model", model, "err", err)
		}
	}
	if t, ok, err := a.DB.CurrentThreshold(ctx, name, model); err == nil && ok {
		return t.Threshold
	}
	return def
}

// TuneThreshold suggests a new threshold for (name, model) from labelled decisions, and stores it
// when apply is set and the rules allow a change. source is auto (auto_tune) or manual (CLI).
func (a *App) TuneThreshold(ctx context.Context, name, model string, apply bool, source string) (decide.Suggestion, error) {
	def, ok := decisionDefaults[name]
	if !ok {
		return decide.Suggestion{}, &UserError{fmt.Sprintf("unknown decision %q (%s, %s)", name, DecisionRouter, DecisionJudge)}
	}
	now := time.Now()
	cur := def
	if t, ok, err := a.DB.CurrentThreshold(ctx, name, model); err != nil {
		return decide.Suggestion{}, err
	} else if ok {
		cur = t.Threshold
	}
	samples, err := a.decisionSamples(ctx, name, model, now)
	if err != nil {
		return decide.Suggestion{}, err
	}
	s := decide.Tune(samples, cur, decisionRules[name])
	if apply && s.Changed {
		err = a.DB.AddThreshold(ctx, store.Threshold{Name: name, Model: model, Threshold: s.Threshold, Previous: cur,
			Samples: s.Samples, Gain: s.Gain, Source: source, Why: s.Why, At: now})
		if err == nil {
			slog.Info("decision threshold tuned", "name", name, "model", model, "from", cur, "to", s.Threshold, "why", s.Why)
		}
	}
	return s, err
}

// RevertThreshold undoes the last change to (name, model)'s threshold.
func (a *App) RevertThreshold(ctx context.Context, name, model string) (store.Threshold, error) {
	t, ok, err := a.DB.CurrentThreshold(ctx, name, model)
	if err != nil {
		return store.Threshold{}, err
	}
	if !ok {
		return store.Threshold{}, &UserError{fmt.Sprintf("%s for %s has never been changed", name, model)}
	}
	r := store.Threshold{Name: name, Model: model, Threshold: t.Previous, Previous: t.Threshold, Source: "revert",
		Why: fmt.Sprintf("reverted the %s change of %s", t.Source, t.At.Format(time.DateTime)), At: time.Now()}
	return r, a.DB.AddThreshold(ctx, r)
}

// CalibrationRow is one (decision, model) in the calibration report.
type CalibrationRow struct {
	Name        string             `json:"name"`
	Model       string             `json:"model"`
	Emulated    bool               `json:"emulated"`
	Decisions   int                `json:"decisions"`
	Labelled    int                `json:"labelled"`
	Calibration decide.Calibration `json:"calibration"`
	Default     float64            `json:"default_threshold"`
	Threshold   float64            `json:"threshold"`
	Suggestion  decide.Suggestion  `json:"suggestion"`
	Changes     []store.Threshold  `json:"changes,omitempty"`
}

// Calibration reports every decision made in the window, without changing anything.
func (a *App) Calibration(ctx context.Context) ([]CalibrationRow, error) {
	now := time.Now()
	keys, err := a.DB.DecisionKeys(ctx, now.Add(-calibrationWindow))
	if err != nil {
		return nil, err
	}
	var out []CalibrationRow
	for _, k := range keys {
		samples, err := a.decisionSamples(ctx, k.Name, k.Model, now)
		if err != nil {
			return nil, err
		}
		row := CalibrationRow{Name: k.Name, Model: k.Model, Emulated: k.Emulated, Decisions: k.Count, Labelled: len(samples),
			Calibration: decide.Calibrate(samples, 10), Default: decisionDefaults[k.Name], Threshold: decisionDefaults[k.Name]}
		if row.Changes, err = a.DB.Thresholds(ctx, k.Name, k.Model, 5); err != nil {
			return nil, err
		}
		if len(row.Changes) > 0 {
			row.Threshold = row.Changes[0].Threshold
		}
		if _, ok := decisionDefaults[k.Name]; ok {
			row.Suggestion = decide.Tune(samples, row.Threshold, decisionRules[k.Name])
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Decisions > out[j].Decisions })
	return out, nil
}
