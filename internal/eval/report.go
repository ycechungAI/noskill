package eval

import (
	"fmt"
	"sort"
	"strings"

	"noskill/internal/store"
)

// TypeScore is the pass count for one task type.
type TypeScore struct {
	Type          string
	Passed, Total int
}

func (t TypeScore) Rate() float64 {
	if t.Total == 0 {
		return 0
	}
	return float64(t.Passed) / float64(t.Total)
}

// ByType groups results by task type (skipped tasks don't count), sorted by type.
func ByType(rs []store.EvalResult) []TypeScore {
	m := map[string]*TypeScore{}
	for _, r := range rs {
		if r.Skipped {
			continue
		}
		ts := m[r.Type]
		if ts == nil {
			ts = &TypeScore{Type: r.Type}
			m[r.Type] = ts
		}
		ts.Total++
		if r.Passed {
			ts.Passed++
		}
	}
	out := make([]TypeScore, 0, len(m))
	for _, ts := range m {
		out = append(out, *ts)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Scores turns results into the 0–1 quality the router uses per task type. With only a couple of
// tasks per type, one result mustn't swing the score to 0 or 1, so a half-task of doubt is added.
func Scores(rs []store.EvalResult) map[string]float64 {
	out := map[string]float64{}
	for _, ts := range ByType(rs) {
		out[ts.Type] = (float64(ts.Passed) + 0.5) / (float64(ts.Total) + 1)
	}
	return out
}

// Regression is a drop between two runs of the same suite on the same model.
type Regression struct {
	Type    string
	Before  TypeScore
	After   TypeScore
	Flipped []string // tasks that passed before and fail now
}

// Margin applies to types with at least 3 paired tasks. It is how far a type's pass rate may fall before it counts as a regression (one task in
// a small type is noise; a fall of more than 0.25, or two flipped tasks, is not).
const Margin = 0.25

// Compare lists regressions of cur against prev. Only tasks present in both runs are compared.
func Compare(prev, cur []store.EvalResult) []Regression {
	before := map[string]store.EvalResult{}
	for _, r := range prev {
		if !r.Skipped {
			before[r.TaskID] = r
		}
	}
	var pairedPrev, pairedCur []store.EvalResult
	flips := map[string][]string{}
	for _, r := range cur {
		b, ok := before[r.TaskID]
		if !ok || r.Skipped {
			continue
		}
		pairedPrev, pairedCur = append(pairedPrev, b), append(pairedCur, r)
		if b.Passed && !r.Passed {
			flips[r.Type] = append(flips[r.Type], r.TaskID)
		}
	}
	bt, ct := ByType(pairedPrev), ByType(pairedCur)
	var out []Regression
	for i := range ct {
		if i >= len(bt) || bt[i].Type != ct[i].Type {
			continue
		}
		drop := bt[i].Rate() - ct[i].Rate()
		if (bt[i].Total >= 3 && drop > Margin+1e-9) || len(flips[ct[i].Type]) >= 2 {
			out = append(out, Regression{Type: ct[i].Type, Before: bt[i], After: ct[i], Flipped: flips[ct[i].Type]})
		}
	}
	return out
}

// Text renders a run as a table: one line per type, then the failures.
func Text(run store.EvalRun, rs []store.EvalResult, prev []store.EvalResult) string {
	var b strings.Builder
	rate := 0.0
	if run.Tasks > 0 {
		rate = 100 * float64(run.Passed) / float64(run.Tasks)
	}
	fmt.Fprintf(&b, "%s · %s · %d/%d passed (%.0f%%)", run.Model, run.StartedAt.Format("2006-01-02 15:04"), run.Passed, run.Tasks, rate)
	if run.Skipped > 0 {
		fmt.Fprintf(&b, " · %d skipped", run.Skipped)
	}
	b.WriteString("\n")
	pt := map[string]TypeScore{}
	for _, ts := range ByType(prev) {
		pt[ts.Type] = ts
	}
	for _, ts := range ByType(rs) {
		fmt.Fprintf(&b, "  %-10s %2d/%-2d", ts.Type, ts.Passed, ts.Total)
		if p, ok := pt[ts.Type]; ok {
			fmt.Fprintf(&b, "  (was %d/%d)", p.Passed, p.Total)
		}
		b.WriteString("\n")
	}
	var steps, tokens int
	var ms int64
	n := 0
	for _, r := range rs {
		if r.Skipped {
			continue
		}
		steps, tokens, ms, n = steps+r.Steps, tokens+r.Tokens, ms+r.Ms, n+1
	}
	if n > 0 {
		fmt.Fprintf(&b, "  average: %.1f steps · %d tokens · %.1f s per task\n", float64(steps)/float64(n), tokens/n, float64(ms)/float64(n)/1000)
	}
	used := 0
	for _, r := range rs {
		if len(r.Learned) > 0 {
			used++
		}
	}
	if used > 0 {
		fmt.Fprintf(&b, "  learned skills used in %d of %d tasks\n", used, n)
	}
	for _, r := range rs {
		if !r.Passed && !r.Skipped {
			fmt.Fprintf(&b, "  ✗ %s: %s\n", r.TaskID, r.Why)
		}
	}
	return b.String()
}

// OverallDrop is how far the pass rate fell, in points (0–100), over the tasks both runs ran.
// Negative means it rose.
func OverallDrop(prev, cur []store.EvalResult) float64 {
	before := map[string]store.EvalResult{}
	for _, r := range prev {
		if !r.Skipped {
			before[r.TaskID] = r
		}
	}
	var n, p, c int
	for _, r := range cur {
		b, ok := before[r.TaskID]
		if !ok || r.Skipped {
			continue
		}
		n++
		if b.Passed {
			p++
		}
		if r.Passed {
			c++
		}
	}
	if n == 0 {
		return 0
	}
	return 100 * float64(p-c) / float64(n)
}
