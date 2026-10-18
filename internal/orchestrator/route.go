package orchestrator

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/backends"
	"noskill/internal/config"
)

// Profile is what the router knows about one installed chat model. Bench fields come from
// `models bench`; until then they're zero and the defaults below apply.
type Profile struct {
	Info        backends.ModelInfo
	Ctx         int                // the fixed num_ctx noskill sends this model (8k default)
	SizeBytes   int64              // memory when loaded at Ctx; 0 = Info.SizeBytes
	LoadSeconds float64            // swap time measured at bench; 0 = estimated from size
	TokPerSec   float64            // 0 = 30
	Quality     map[string]float64 // 0–1 per task type; "*" = any: the bandit's prior (FR-O4)
}

func (p Profile) size() int64 {
	if p.SizeBytes > 0 {
		return p.SizeBytes
	}
	return p.Info.SizeBytes
}

func (p Profile) load() float64 {
	if p.LoadSeconds > 0 {
		return p.LoadSeconds
	}
	return 1 + float64(p.size())/GB*0.8 // ~4 s for 5 GB, close to S4's 4.3 s for the coder
}

func (p Profile) quality(task string) float64 {
	if q, ok := p.Quality[task]; ok {
		return q
	}
	if q, ok := p.Quality["*"]; ok {
		return q
	}
	return 0.5
}

// State is the machine right now (Ollama /api/ps plus the router's own memory).
type State struct {
	Budget       int64
	BudgetNote   string                 // set when the budget grew past the base because memory is free (ADR-0012)
	Resident     []backends.LoadedModel // pinned decision and embedding models
	Primary      string                 // loaded chat model, "" if none
	PrimarySince time.Time
	Streaming    bool // the primary is streaming an answer to a client
	Now          time.Time
	// Background: a scheduled job. It uses the loaded primary when that can serve it, and swaps
	// only after Idle reaches BackgroundIdle (rule 6).
	Background bool
	Idle       time.Duration
}

// BackgroundIdle is how long you must have been away before a job may swap the primary model.
const BackgroundIdle = 10 * time.Minute

func (s State) residentBytes() int64 {
	var n int64
	for _, m := range s.Resident {
		n += m.SizeBytes
	}
	return n
}

// Router implements the selection rules in ORCHESTRATOR.md "Selection" (FR-O2, FR-O3).
type Router struct {
	Cfg      config.Orchestrator
	Profiles []Profile
	// Evidence is the bandit's learned part per model for this request's task type and difficulty
	// bucket (FR-O4). Explore draws Thompson samples for the models that need no swap.
	Evidence map[string]Evidence
	Explore  bool
	Rand     *rand.Rand
	// Exclude rules models out (escalation retries on the next-best model).
	Exclude []string
}

// Decision is the router's pick.
type Decision struct {
	Model string
	Swap  bool // a different model must be loaded
	// UnloadResident: a hard need forces a model that only fits without the decision model;
	// decisions are emulated by the new model until it swaps back.
	UnloadResident bool
	// WaitForStream: the swap must wait until the current answer finishes streaming (rule 5).
	WaitForStream bool
	// Defer: a background job would have to swap the primary while you're active; it waits
	// (rule 6).
	Defer       bool
	Think       *bool
	Explanation Explanation
}

// Candidate is one row of `route explain`.
type Candidate struct {
	Model    string
	Loaded   bool
	Rule     bool
	Quality  float64 // the bandit's posterior mean (or a sample when exploring)
	Runs     float64 // rated runs behind Quality beyond the prior
	Latency  float64 // seconds, including load
	Score    float64
	Rejected string // why it can't serve this request; "" if it can
}

type Explanation struct {
	Class      Classification
	Preset     string
	Budget     int64
	BudgetNote string
	Resident   int64
	Primary    string
	Cands      []Candidate
	Picked     string
	Why        string
	Notes      []string
}

var ErrNoModel = errors.New("no installed model can serve this request")

// weights per preset: quality is 0–1, latency is seconds.
var latencyWeight = map[string]float64{"fast": 0.05, "balanced": 0.02, "best": 0.005}

const expectedOutputTokens = 300

func (r Router) Route(c Classification, req Request, st State) (Decision, error) {
	if st.Now.IsZero() {
		st.Now = time.Now()
	}
	wl, ok := latencyWeight[r.Cfg.Preset]
	if !ok {
		wl = latencyWeight["balanced"]
	}
	ex := Explanation{Class: c, Preset: r.Cfg.Preset, Budget: st.Budget, BudgetNote: st.BudgetNote, Resident: st.residentBytes(), Primary: st.Primary}
	room := st.Budget - ex.Resident

	needCtx := req.InputTokens + 1024 // room for the answer
	byName := map[string]Profile{}
	cands := map[string]*Candidate{}
	var order []string
	for _, p := range r.Profiles {
		name := p.Info.Name
		byName[name] = p
		cd := &Candidate{Model: name, Loaded: name == st.Primary}
		ev := r.Evidence[name]
		pa, pb := posterior(p.quality(c.TaskType), ev)
		cd.Quality, cd.Runs = betaMean(pa, pb), ev.Alpha+ev.Beta
		if r.Explore && cd.Loaded && r.Rand != nil {
			cd.Quality = betaSample(r.Rand, pa, pb)
		}
		cd.Latency = expectedOutputTokens / tokps(p)
		if !cd.Loaded {
			cd.Latency += p.load()
		}
		cd.Score = cd.Quality - wl*cd.Latency
		switch {
		case slices.Contains(r.Exclude, name):
			cd.Rejected = "already tried for this request"
		case !p.Info.Has(backends.CapChat):
			cd.Rejected = "not a chat model"
		case c.Has("vision") && !p.Info.Has(backends.CapVision):
			cd.Rejected = "no vision"
		case c.Has("tools") && !p.Info.Has(backends.CapTools) && !p.Info.Has(backends.CapSchema):
			cd.Rejected = "no tool calling or JSON schema"
		case st.Budget > 0 && p.size() > st.Budget:
			cd.Rejected = "larger than the memory budget (" + gb(p.size()) + ")"
		case st.Budget > 0 && p.size() > room && !c.Has("vision"):
			cd.Rejected = fmt.Sprintf("doesn't fit next to the resident models (%s > %s)", gb(p.size()), gb(room))
		}
		cands[name] = cd
		order = append(order, name)
	}
	// Context is a soft filter: when no model has enough, the input gets chunked instead.
	enough := func(n string) bool { p := byName[n]; return p.Ctx == 0 || p.Ctx >= needCtx }
	if slices.ContainsFunc(order, func(n string) bool { return cands[n].Rejected == "" && enough(n) }) {
		for _, n := range order {
			if cands[n].Rejected == "" && !enough(n) {
				cands[n].Rejected = fmt.Sprintf("context %d < %d needed", byName[n].Ctx, needCtx)
			}
		}
	} else if req.InputTokens > 0 {
		ex.Notes = append(ex.Notes, "no model's context holds the input; it will be summarised or chunked")
	}
	ok2 := func(n string) bool { cd, ok := cands[n]; return ok && cd.Rejected == "" }

	finish := func(model, why string) (Decision, error) {
		d := Decision{Model: model, Swap: model != st.Primary, Think: c.ThinkFor()}
		p := byName[model]
		if st.Budget > 0 && p.size() > room {
			d.UnloadResident = true
			why += fmt.Sprintf("; unloads the resident models for now (%s > %s)", gb(p.size()), gb(room))
		}
		if d.Swap && st.Streaming && st.Primary != "" {
			d.WaitForStream = true
			why += "; waits for the current answer to finish"
		}
		if d.Swap && st.Primary != "" {
			why += fmt.Sprintf("; swap %s → %s, est. load %.1f s", st.Primary, model, p.load())
		}
		if d.Swap && st.Background && st.Primary != "" && st.Idle < BackgroundIdle {
			d.Defer = true // rule 6: a job doesn't take the model from under you
			why += fmt.Sprintf("; the job waits until you've been idle %s (idle %s)", BackgroundIdle, st.Idle.Round(time.Second))
		}
		ex.Picked, ex.Why = model, why
		for _, n := range order {
			ex.Cands = append(ex.Cands, *cands[n])
		}
		sort.SliceStable(ex.Cands, func(i, j int) bool {
			a, b := ex.Cands[i], ex.Cands[j]
			if (a.Rejected == "") != (b.Rejected == "") {
				return a.Rejected == ""
			}
			return a.Score > b.Score
		})
		d.Explanation = ex
		return d, nil
	}

	// 1. User rules win unless a hard need rules the model out (FR-O2).
	for i, rule := range r.Cfg.Rules {
		if !ruleMatches(rule, c, r.Cfg.Preset) {
			continue
		}
		cd, ok := cands[rule.Use]
		switch {
		case !ok:
			ex.Notes = append(ex.Notes, fmt.Sprintf("rule %d (%s) skipped: %s isn't installed", i+1, ruleText(rule), rule.Use))
		case cd.Rejected != "":
			ex.Notes = append(ex.Notes, fmt.Sprintf("rule %d (%s) skipped: %s %s", i+1, ruleText(rule), rule.Use, cd.Rejected))
		default:
			cd.Rule = true
			return finish(rule.Use, "user rule '"+ruleText(rule)+"'")
		}
	}

	best := ""
	for _, n := range order {
		if ok2(n) && (best == "" || cands[n].Score > cands[best].Score) {
			best = n
		}
	}
	if best == "" {
		ex.Why = "every installed model was ruled out"
		for _, n := range order {
			ex.Cands = append(ex.Cands, *cands[n])
		}
		return Decision{Explanation: ex}, ErrNoModel
	}

	if r.Explore {
		ex.Notes = append(ex.Notes, "exploring: the loaded model's quality is a sample from its posterior")
	}
	// 2. Summarise and extract use whatever is loaded: every model is good enough.
	if (c.TaskType == "summarise" || c.TaskType == "extract") && ok2(st.Primary) {
		return finish(st.Primary, c.TaskType+" uses the loaded model")
	}
	// 6. Background jobs use the loaded model when it can serve them, and swap it only once
	// you've been away for a while.
	if st.Background && ok2(st.Primary) {
		return finish(st.Primary, "background job uses the loaded model")
	}
	if st.Primary == "" || !ok2(st.Primary) {
		if st.Primary != "" {
			return finish(best, "hard need: loaded "+st.Primary+" "+cands[st.Primary].Rejected)
		}
		return finish(best, "highest score; nothing loaded")
	}
	if best == st.Primary {
		return finish(best, "loaded and highest score")
	}
	// 3. Soft gain: swap only for a real quality gain that's worth the load time, never while
	// uncertain, holding a fresh model, or streaming.
	gain := cands[best].Quality - cands[st.Primary].Quality
	switch {
	case c.Uncertain:
		return finish(st.Primary, "task type uncertain; staying on the loaded model")
	case st.Streaming:
		return finish(st.Primary, "loaded model is streaming; no soft swap")
	case !st.PrimarySince.IsZero() && st.Now.Sub(st.PrimarySince) < time.Duration(r.Cfg.HoldSeconds)*time.Second:
		return finish(st.Primary, fmt.Sprintf("hold: %s was loaded %ds ago (hold_seconds %d)", st.Primary, int(st.Now.Sub(st.PrimarySince).Seconds()), r.Cfg.HoldSeconds))
	case gain < r.Cfg.MinSwapGain:
		return finish(st.Primary, fmt.Sprintf("%s is only %.2f better (min_swap_gain %.2f); staying", best, gain, r.Cfg.MinSwapGain))
	}
	return finish(best, fmt.Sprintf("quality +%.2f is worth the load under preset %s", gain, ex.Preset))
}

func tokps(p Profile) float64 {
	if p.TokPerSec > 0 {
		return p.TokPerSec
	}
	return 30
}

func ruleMatches(r config.Rule, c Classification, preset string) bool {
	if r.When.TaskType == "" && len(r.When.Needs) == 0 && r.When.Preset == "" {
		return false
	}
	if r.When.Preset != "" && r.When.Preset != preset {
		return false
	}
	if r.When.TaskType != "" && (c.Uncertain || r.When.TaskType != c.TaskType) {
		return false
	}
	for _, n := range r.When.Needs {
		if !c.Has(n) {
			return false
		}
	}
	return true
}

func ruleText(r config.Rule) string {
	var p []string
	if r.When.TaskType != "" {
		p = append(p, r.When.TaskType)
	}
	if len(r.When.Needs) > 0 {
		p = append(p, "needs "+needsString(r.When.Needs))
	}
	if r.When.Preset != "" {
		p = append(p, "preset "+r.When.Preset)
	}
	return strings.Join(p, ", ")
}

// Text renders the explanation the way `noskill route explain` prints it (FR-O6).
func (e Explanation) Text() string {
	var b strings.Builder
	conf := "unknown"
	if e.Class.Confidence >= 0 {
		conf = fmt.Sprintf("%.2f", e.Class.Confidence)
	}
	if e.Class.Source == "not needed" {
		fmt.Fprintf(&b, "task_type=%s (not needed)", e.Class.TaskType)
	} else {
		fmt.Fprintf(&b, "task_type=%s (%s %s)", e.Class.TaskType, e.Class.Source, conf)
	}
	if e.Class.Uncertain {
		b.WriteString(" uncertain")
	}
	if len(e.Class.Needs) > 0 {
		fmt.Fprintf(&b, " needs=%s", needsString(e.Class.Needs))
	}
	b.WriteString("\n")
	primary := e.Primary
	if primary == "" {
		primary = "none"
	}
	budget := gb(e.Budget)
	if e.BudgetNote != "" {
		budget += " (" + e.BudgetNote + ")"
	}
	fmt.Fprintf(&b, "budget %s · resident %s · primary: %s · preset %s\n", budget, gb(e.Resident), primary, e.Preset)
	b.WriteString("candidates:")
	for i, c := range e.Cands {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " %s", c.Model)
		var tags []string
		if c.Rule {
			tags = append(tags, "rule")
		}
		if c.Loaded {
			tags = append(tags, "loaded")
		}
		if c.Rejected != "" {
			tags = append(tags, "✗ "+c.Rejected)
		} else {
			// FR-O6: the numbers behind the score: quality (benchmark prior plus rated runs), how
			// many rated runs moved it, and the time to answer (a swap included when not loaded).
			tags = append(tags, fmt.Sprintf("score %.2f = quality %.2f", c.Score, c.Quality))
			if c.Runs > 0 {
				tags = append(tags, fmt.Sprintf("%.0f rated runs", c.Runs))
			} else {
				tags = append(tags, "no rated runs yet")
			}
			tags = append(tags, fmt.Sprintf("~%.0f s", c.Latency))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(tags, ", "))
	}
	b.WriteString("\n")
	if e.Picked != "" {
		fmt.Fprintf(&b, "picked: %s: %s\n", e.Picked, e.Why)
	} else {
		fmt.Fprintf(&b, "no pick: %s\n", e.Why)
	}
	if e.Class.Note != "" {
		fmt.Fprintf(&b, "note: %s\n", e.Class.Note)
	}
	for _, n := range e.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}
