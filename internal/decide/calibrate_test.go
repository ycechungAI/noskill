package decide

import (
	"math"
	"testing"
)

// separable has right decisions at 0.31–0.60 and wrong ones at 0.05–0.24: any threshold in
// (0.24, 0.31] splits them perfectly.
func separable() []Sample {
	var s []Sample
	for i := 0; i < 30; i++ {
		s = append(s, Sample{P: 0.31 + float64(i)/100, Y: true})
	}
	for i := 0; i < 20; i++ {
		s = append(s, Sample{P: 0.05 + float64(i)/100, Y: false})
	}
	return s
}

func TestCalibrate(t *testing.T) {
	var s []Sample
	for i := 0; i < 10; i++ {
		s = append(s, Sample{P: 0.95, Y: true}, Sample{P: 0.05, Y: false})
	}
	c := Calibrate(s, 10)
	if c.N != 20 || c.Pos != 10 || len(c.Bins) != 2 || c.ECE > 0.051 || c.Brier > 0.0026 {
		t.Fatalf("well calibrated: %+v", c)
	}
	// Always 0.9 sure, right half the time: ECE 0.4.
	s = s[:0]
	for i := 0; i < 10; i++ {
		s = append(s, Sample{P: 0.9, Y: i%2 == 0})
	}
	if c = Calibrate(s, 10); math.Abs(c.ECE-0.4) > 1e-9 {
		t.Fatalf("overconfident ECE = %v", c.ECE)
	}
	if c = Calibrate(nil, 10); c.N != 0 || c.ECE != 0 {
		t.Fatalf("empty: %+v", c)
	}
}

// FR-B6c: thresholds move only on enough evidence, by at most MaxStep, within bounds.
func TestTune(t *testing.T) {
	s := separable()
	got := Tune(s, 0.5, TuneRules{})
	if !got.Changed || got.Threshold != 0.4 || got.Gain <= 0 {
		t.Fatalf("one step of 0.1 toward ~0.3: %+v", got)
	}
	if got = Tune(s, 0.4, TuneRules{}); !got.Changed || got.Threshold > 0.31 || got.Threshold <= 0.24 {
		t.Fatalf("second step reaches the split: %+v", got)
	}
	if got = Tune(s, got.Threshold, TuneRules{}); got.Changed {
		t.Fatalf("at the split it stays: %+v", got)
	}
	if got = Tune(s, 0.5, TuneRules{Lo: 0.45, Hi: 0.9}); got.Threshold < 0.45 {
		t.Fatalf("below the floor: %+v", got)
	}
	if got = Tune(s[:20], 0.5, TuneRules{}); got.Changed {
		t.Fatalf("20 samples are too few: %+v", got)
	}
	// All right: nothing to separate.
	var all []Sample
	for i := 0; i < 50; i++ {
		all = append(all, Sample{P: float64(i) / 50, Y: true})
	}
	if got = Tune(all, 0.5, TuneRules{}); got.Changed {
		t.Fatalf("no wrong decisions: %+v", got)
	}
	// Probabilities that say nothing about the result, or say the opposite, don't move it.
	var noise, opposite []Sample
	for i := 0; i < 60; i++ {
		noise = append(noise, Sample{P: float64(i%10) / 10, Y: (i/10)%2 == 0})
		opposite = append(opposite, Sample{P: float64(i%10) / 10, Y: i%2 == 0 && i%10 < 5})
	}
	if got = Tune(noise, 0.5, TuneRules{}); got.Changed {
		t.Fatalf("noise moved it: %+v", got)
	}
	if got = Tune(opposite, 0.5, TuneRules{}); got.Changed {
		t.Fatalf("anti-correlated moved it: %+v", got)
	}
}
