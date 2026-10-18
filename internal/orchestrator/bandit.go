package orchestrator

import (
	"math"
	"math/rand/v2"
)

// Learned routing (ORCHESTRATOR.md "Learned routing", FR-O4): a Beta per (task type, difficulty
// bucket, model). The benchmark score (or 0.5 unbenched) is the prior, worth priorStrength runs;
// each rated run adds its reward. Routing uses the posterior mean, or a Thompson sample when
// exploring.

const priorStrength = 4.0

// Evidence is what runs added to one arm: reward and its complement, summed.
type Evidence struct{ Alpha, Beta float64 }

// Bucket groups the classifier's difficulty: 0 trivial or easy, 1 moderate (or not asked),
// 2 hard or very hard.
func Bucket(difficulty float64) int {
	switch {
	case difficulty < 0:
		return 1
	case difficulty < 1.5:
		return 0
	case difficulty < 2.5:
		return 1
	}
	return 2
}

// Reward is a run's value for the bandit: 1 for success, 0.5 partial, 0 fail, minus the
// preset's latency penalty (capped at half). ok is false when the outcome is too unsure to count.
func Reward(outcome string, conf, seconds float64, preset string) (r float64, ok bool) {
	base, known := map[string]float64{"success": 1, "partial": 0.5, "fail": 0}[outcome]
	if !known || conf < 0.6 {
		return 0, false
	}
	wl, found := latencyWeight[preset]
	if !found {
		wl = latencyWeight["balanced"]
	}
	return math.Max(0, base-math.Min(0.5, wl*seconds)), true
}

// posterior returns the Beta parameters for a prior quality and the evidence.
func posterior(prior float64, e Evidence) (a, b float64) {
	prior = math.Min(0.99, math.Max(0.01, prior))
	return prior*priorStrength + e.Alpha, (1-prior)*priorStrength + e.Beta
}

func betaMean(a, b float64) float64 { return a / (a + b) }

// betaSample draws from Beta(a, b) through two gamma draws (Marsaglia–Tsang).
func betaSample(r *rand.Rand, a, b float64) float64 {
	x, y := gammaSample(r, a), gammaSample(r, b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

func gammaSample(r *rand.Rand, k float64) float64 {
	if k < 1 {
		return gammaSample(r, k+1) * math.Pow(r.Float64(), 1/k)
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if math.Log(u) < 0.5*x*x+d-d*v+d*math.Log(v) {
			return d * v
		}
	}
}
