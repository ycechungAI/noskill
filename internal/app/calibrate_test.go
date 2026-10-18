package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/config"
	"noskill/internal/decide"
	"noskill/internal/store"
)

// seedJudged adds runs with a judge.answered decision by model and an explicit outcome: right
// answers at P 0.31–0.60, wrong ones at 0.05–0.24 (see decide.TestTune).
func seedJudged(t *testing.T, db *store.DB, model string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	add := func(i int, p float64, ok bool) {
		id := fmt.Sprintf("seed-%s-%02d", model, i)
		if err := db.InsertRun(ctx, store.Run{ID: id, Source: "cli", Status: "done", StartedAt: now}); err != nil {
			t.Fatal(err)
		}
		outcome := "fail"
		if ok {
			outcome = "success"
		}
		db.SetOutcome(ctx, id, outcome, 1, "explicit")
		if err := db.AddDecision(ctx, store.Decision{RunID: id, Name: DecisionJudge, Model: model, P: p, Threshold: 0.5, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		add(i, 0.31+float64(i)/100, true)
	}
	for i := 0; i < 20; i++ {
		add(30+i, 0.05+float64(i)/100, false)
	}
}

// FR-B6c: decisions are recorded, the report suggests a threshold, and nothing changes
// until the user applies it or turns auto_tune on. Every change can be reverted.
func TestCalibrationTracksAndTunes(t *testing.T) {
	ctx := context.Background()
	fake := &backendtest.Fake{Script: []backends.Response{{Content: "Paris."}}}
	e := newApp(t, fake, noRules, "")
	j := &fakeJudge{answered: 0.9, score: 3}
	e.app.SetJudge(j)
	res, err := e.app.Run(ctx, RunRequest{Messages: user("Capital of France?")})
	if err != nil {
		t.Fatal(err)
	}
	e.app.WaitBackground()
	if rows, _ := e.app.DB.DecisionKeys(ctx, time.Now().Add(-time.Hour)); len(rows) != 1 || rows[0].Name != DecisionJudge || rows[0].Model != "fake-judge" {
		t.Fatalf("judge decision not recorded: %+v", rows)
	}
	// The judge's own verdict doesn't label its decision; your rating does.
	e.app.Rate(ctx, res.RunID, "good", "")
	if s, _ := e.app.decisionSamples(ctx, DecisionJudge, "fake-judge", time.Now()); len(s) != 1 || !s[0].Y {
		t.Fatalf("samples after rating: %+v", s)
	}

	seedJudged(t, e.app.DB, "tev1:0.8b")
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.5 {
		t.Fatalf("auto_tune is off by default, threshold %v", th)
	}
	rows, err := e.app.Calibration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row *CalibrationRow
	for i := range rows {
		if rows[i].Model == "tev1:0.8b" {
			row = &rows[i]
		}
	}
	if row == nil || row.Labelled != 50 || row.Threshold != 0.5 || !row.Suggestion.Changed || row.Suggestion.Threshold != 0.4 {
		t.Fatalf("report: %+v", row)
	}
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.5 {
		t.Fatalf("the report changed the threshold to %v", th)
	}

	if s, err := e.app.TuneThreshold(ctx, DecisionJudge, "tev1:0.8b", true, "manual"); err != nil || s.Threshold != 0.4 {
		t.Fatalf("tune: %+v %v", s, err)
	}
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.4 {
		t.Fatalf("applied threshold %v", th)
	}
	if _, err := e.app.RevertThreshold(ctx, DecisionJudge, "tev1:0.8b"); err != nil {
		t.Fatal(err)
	}
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.5 {
		t.Fatalf("reverted threshold %v", th)
	}
	if _, err := e.app.RevertThreshold(ctx, DecisionRouter, "tev1:0.8b"); err == nil {
		t.Error("reverting a threshold that never changed must fail")
	}
}

func TestAutoTuneOncePerDay(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, func(c *config.Config) { c.Orchestrator.AutoTune = true }, "")
	seedJudged(t, e.app.DB, "tev1:0.8b")
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.4 {
		t.Fatalf("first read tunes one step: %v", th)
	}
	if th := e.app.decisionThreshold(ctx, DecisionJudge, "tev1:0.8b"); th != 0.4 {
		t.Fatalf("second read the same day must not tune again: %v", th)
	}
	ch, _ := e.app.DB.Thresholds(ctx, DecisionJudge, "tev1:0.8b", 10)
	if len(ch) != 1 || ch[0].Source != "auto" || ch[0].Previous != 0.5 {
		t.Fatalf("changes: %+v", ch)
	}
	// Verdicts use it: P(yes) 0.45 now counts as answered.
	out, _ := judgeVerdictAt(fakeAnswers(0.45, 3), 0.4)
	if out != "success" {
		t.Errorf("verdict at 0.4: %s", out)
	}
	if out, _ = judgeVerdict(fakeAnswers(0.45, 3)); out != "fail" {
		t.Errorf("verdict at the default: %s", out)
	}
}

func fakeAnswers(answered, score float64) (a decide.Answers) {
	j := &fakeJudge{answered: answered, score: score}
	a, _ = j.Decide(context.Background(), nil, nil)
	return a
}
