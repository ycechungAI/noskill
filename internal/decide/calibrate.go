package decide

import (
	"fmt"
	"math"
	"sort"
)

// Calibration and threshold tuning (FR-B6c, MODEL_ADAPTERS.md "Calibration"). Every
// thresholded decision is stored with its probability; once its run has a trusted outcome it
// becomes a Sample. The report shows how well the probabilities match what happened, and Tune
// suggests a threshold that separates right from wrong better than the current one.

// Sample is one decision with a known result: P is the probability the decision gave (P(yes), or
// the confidence of the chosen label), Y whether acting on it was right.
type Sample struct {
	P float64
	Y bool
}

// Bin is one confidence bucket of a reliability table.
type Bin struct {
	Lo, Hi float64
	N      int
	MeanP  float64 // average probability given
	Rate   float64 // share that turned out right
}

// Calibration summarises samples: the reliability table, expected calibration error and Brier score.
type Calibration struct {
	N     int     `json:"n"`
	Pos   int     `json:"positive"`
	ECE   float64 `json:"ece"`
	Brier float64 `json:"brier"`
	Bins  []Bin   `json:"bins"`
}

// Calibrate builds the reliability table over nbins equal-width bins (empty bins are left out).
func Calibrate(s []Sample, nbins int) Calibration {
	if nbins <= 0 {
		nbins = 10
	}
	c := Calibration{N: len(s)}
	if len(s) == 0 {
		return c
	}
	sumP := make([]float64, nbins)
	hit := make([]int, nbins)
	n := make([]int, nbins)
	for _, x := range s {
		p := clamp01(x.P)
		i := min(int(p*float64(nbins)), nbins-1)
		n[i]++
		sumP[i] += p
		y := 0.0
		if x.Y {
			hit[i]++
			c.Pos++
			y = 1
		}
		c.Brier += (p - y) * (p - y)
	}
	c.Brier /= float64(len(s))
	for i := range n {
		if n[i] == 0 {
			continue
		}
		b := Bin{Lo: float64(i) / float64(nbins), Hi: float64(i+1) / float64(nbins), N: n[i],
			MeanP: sumP[i] / float64(n[i]), Rate: float64(hit[i]) / float64(n[i])}
		c.ECE += float64(n[i]) / float64(len(s)) * math.Abs(b.MeanP-b.Rate)
		c.Bins = append(c.Bins, b)
	}
	return c
}

// Separation is Youden's J at threshold t: the share of right decisions at or above t minus the
// share of wrong ones at or above t. 1 = t splits them perfectly, 0 = no better than chance.
func Separation(s []Sample, t float64) float64 {
	var pos, neg, tp, fp int
	for _, x := range s {
		above := x.P >= t
		if x.Y {
			pos++
			if above {
				tp++
			}
		} else {
			neg++
			if above {
				fp++
			}
		}
	}
	if pos == 0 || neg == 0 {
		return 0
	}
	return float64(tp)/float64(pos) - float64(fp)/float64(neg)
}

// TuneRules bound automatic changes: thresholds move only with enough evidence, in small steps,
// within fixed limits, and only for a clear gain.
type TuneRules struct {
	MinSamples int     // labelled decisions needed (default 40)
	MinEach    int     // of each kind, right and wrong (default 8)
	MaxStep    float64 // largest change in one tune (default 0.1)
	Lo, Hi     float64 // the threshold always stays in [Lo, Hi] (default 0.2–0.9)
	MinGain    float64 // separation gain needed to move (default 0.05)
	MinSep     float64 // the new threshold must separate at least this well (default 0.2)
}

func (r TuneRules) withDefaults() TuneRules {
	if r.MinSamples == 0 {
		r.MinSamples = 40
	}
	if r.MinEach == 0 {
		r.MinEach = 8
	}
	if r.MaxStep == 0 {
		r.MaxStep = 0.1
	}
	if r.Lo == 0 && r.Hi == 0 {
		r.Lo, r.Hi = 0.2, 0.9
	}
	if r.MinGain == 0 {
		r.MinGain = 0.05
	}
	if r.MinSep == 0 {
		r.MinSep = 0.2
	}
	return r
}

// Suggestion is Tune's answer. Changed is false when the current threshold stays.
type Suggestion struct {
	Current   float64 `json:"current"`
	Threshold float64 `json:"threshold"`
	Changed   bool    `json:"changed"`
	Gain      float64 `json:"gain"` // separation at Threshold minus at Current
	Samples   int     `json:"samples"`
	Why       string  `json:"why"`
}

// Tune suggests a threshold for samples. It searches [Lo, Hi] in steps of 0.01 for the best
// separation, then moves from current toward it by at most MaxStep. Ties go to the threshold
// nearest current, so noise doesn't make it wander.
func Tune(s []Sample, current float64, r TuneRules) Suggestion {
	r = r.withDefaults()
	out := Suggestion{Current: current, Threshold: current, Samples: len(s)}
	pos := 0
	for _, x := range s {
		if x.Y {
			pos++
		}
	}
	switch {
	case len(s) < r.MinSamples:
		out.Why = fmt.Sprintf("%d labelled decisions; %d needed", len(s), r.MinSamples)
		return out
	case pos < r.MinEach || len(s)-pos < r.MinEach:
		out.Why = fmt.Sprintf("%d right and %d wrong; %d of each needed", pos, len(s)-pos, r.MinEach)
		return out
	}
	base := Separation(s, current)
	cands := make([]float64, 0, 100)
	for t := math.Round(r.Lo*100) / 100; t <= r.Hi+1e-9; t += 0.01 {
		cands = append(cands, math.Round(t*100)/100)
	}
	sort.SliceStable(cands, func(i, j int) bool { return math.Abs(cands[i]-current) < math.Abs(cands[j]-current) })
	best, bestJ := current, base
	for _, t := range cands {
		if j := Separation(s, t); j > bestJ+1e-9 {
			best, bestJ = t, j
		}
	}
	if bestJ < r.MinSep {
		out.Why = fmt.Sprintf("no threshold separates right from wrong (best %.2f at %.2f; %.2f needed)", bestJ, best, r.MinSep)
		return out
	}
	if bestJ-base < r.MinGain {
		out.Why = fmt.Sprintf("best separation %.2f at %.2f is within %.2f of the current %.2f", bestJ, best, r.MinGain, base)
		return out
	}
	next := current + math.Max(-r.MaxStep, math.Min(r.MaxStep, best-current))
	next = math.Round(math.Max(r.Lo, math.Min(r.Hi, next))*100) / 100
	out.Threshold, out.Changed = next, next != current
	out.Gain = Separation(s, next) - base
	out.Why = fmt.Sprintf("separation %.2f → %.2f (best %.2f at %.2f)", base, base+out.Gain, bestJ, best)
	if !out.Changed || out.Gain <= 0 {
		out.Threshold, out.Changed, out.Gain = current, false, 0
		out.Why = fmt.Sprintf("a step of at most %.2f toward %.2f doesn't help yet", r.MaxStep, best)
	}
	return out
}

func clamp01(p float64) float64 { return math.Max(0, math.Min(1, p)) }
