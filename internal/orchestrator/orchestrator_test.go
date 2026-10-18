package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"noskill/internal/backends"
	"noskill/internal/config"
	"noskill/internal/decide"
	"noskill/internal/platform"
)

type fakeDecider struct {
	a     decide.Answers
	err   error
	calls int
	qs    map[string]decide.Question
}

func (f *fakeDecider) Decide(_ context.Context, _ map[string]any, qs map[string]decide.Question) (decide.Answers, error) {
	f.calls++
	f.qs = qs
	return f.a, f.err
}

func conf(v float64) *float64 { return &v }

func answer(choice string, c *float64) decide.Answers {
	return decide.Answers{Answers: map[string]decide.Answer{"task_type": {Type: decide.Choice, Choice: choice, Confidence: c}}}
}

func TestHeuristicsSkipTheDecisionCall(t *testing.T) {
	d := &fakeDecider{}
	c := Classifier{Decider: d}
	for _, tc := range []struct {
		r    Request
		want string
	}{
		{Request{Text: "what's this?", Images: 1}, "vision"},
		{Request{Text: "why does this fail?\n```go\nfunc main() {}\n```"}, "code"},
		{Request{Text: "fix the nil check in server.go"}, "code"},
		{Request{Text: "anything", SkillType: "research"}, "research"},
	} {
		got, err := c.Classify(context.Background(), tc.r)
		if err != nil || got.TaskType != tc.want || got.Uncertain {
			t.Errorf("%q: got %+v, %v", tc.r.Text, got, err)
		}
	}
	if d.calls != 0 {
		t.Errorf("decider called %d times", d.calls)
	}
}

func TestDecisionTier(t *testing.T) {
	d := &fakeDecider{a: answer("research", conf(0.91))}
	got, _ := Classifier{Decider: d}.Classify(context.Background(), Request{Text: "What changed in Ollama 0.35 today?"})
	if got.TaskType != "research" || got.Uncertain || got.Source != "decision" || !got.Has("web") {
		t.Fatalf("got %+v", got)
	}
	if _, ok := d.qs["task_type"].Choices["vision"]; ok {
		t.Error("vision offered as a text-only choice")
	}
	if _, ok := d.qs["difficulty"]; ok {
		t.Error("difficulty asked without AskDifficulty")
	}

	d.a = answer("code", conf(0.4))
	got, _ = Classifier{Decider: d}.Classify(context.Background(), Request{Text: "hmm"})
	if !got.Uncertain {
		t.Errorf("0.4 confidence accepted: %+v", got)
	}

	d.a = answer("code", nil)
	d.a.Emulated = true
	got, _ = Classifier{Decider: d}.Classify(context.Background(), Request{Text: "hmm"})
	if got.Uncertain || got.Source != "emulated" || got.Confidence != -1 {
		t.Errorf("emulated: %+v", got)
	}

	d.err = errors.New("ollama down")
	got, err := Classifier{Decider: d}.Classify(context.Background(), Request{Text: "hmm"})
	if err != nil || got.TaskType != "chat" || !got.Uncertain || got.Source != "default" {
		t.Errorf("failure: %+v, %v", got, err)
	}
}

func TestDifficultyTurnsThinkOn(t *testing.T) {
	d := &fakeDecider{a: answer("reasoning", conf(0.8))}
	d.a.Answers["difficulty"] = decide.Answer{Type: decide.Score, Score: 2.2}
	got, _ := Classifier{Decider: d, AskDifficulty: true}.Classify(context.Background(), Request{Text: "x"})
	if th := got.ThinkFor(); th == nil || !*th {
		t.Errorf("think = %v for difficulty %.1f", th, got.Difficulty)
	}
}

func TestMemoryBudget(t *testing.T) {
	gib := uint64(1 << 30)
	for _, tc := range []struct {
		m    platform.Memory
		want int64
	}{
		{platform.Memory{TotalBytes: 16 * gib, AppleSilicon: true}, 6 * GB},
		{platform.Memory{TotalBytes: 36 * gib, AppleSilicon: true}, int64(24 * gib)},
		{platform.Memory{TotalBytes: 32 * gib, VRAMBytes: 12 * GB}, 11*GB + GB/2},
		{platform.Memory{TotalBytes: 16 * gib}, int64(8 * gib)},
	} {
		if got, _ := MemoryBudget(config.AutoFloat{Auto: true}, tc.m); got != tc.want {
			t.Errorf("%+v: %d, want %d", tc.m, got, tc.want)
		}
	}
	if got, _ := MemoryBudget(config.AutoFloat{Value: 10}, platform.Memory{}); got != 10*GB {
		t.Errorf("override = %d", got)
	}
}

// The budget grows past the base only by free memory, never past the ceiling, and not at all
// under pressure (ADR-0012).
func TestBurstBudget(t *testing.T) {
	gib := uint64(1 << 30)
	mac := platform.Memory{TotalBytes: 16 * gib, AppleSilicon: true, Pressure: "normal"}
	base, _ := MemoryBudget(config.AutoFloat{Auto: true}, mac)
	ceil := BurstCeiling(config.AutoFloat{Auto: true}, base, mac)
	if ceil != int64(16*gib)*2/3 {
		t.Fatalf("ceiling %d", ceil)
	}
	for _, tc := range []struct {
		name     string
		avail    uint64
		pressure string
		ours     int64
		want     int64
		mustGrow bool
	}{
		{"you're busy: 3 GB free", 3 * GB, "normal", 4 * GB, base, false},
		{"half free, our 4 GB counts", 6 * GB, "normal", 4 * GB, 8 * GB, true},
		{"all free: capped at Metal's limit", 14 * GB, "normal", 0, ceil, true},
		{"pressure warn", 14 * GB, "warn", 0, base, false},
		{"free unknown", 0, "normal", 0, base, false},
	} {
		m := mac
		m.AvailableBytes, m.Pressure = tc.avail, tc.pressure
		got, note := BurstBudget(base, ceil, m, tc.ours)
		if got != tc.want || (note != "") != tc.mustGrow {
			t.Errorf("%s: %d %q, want %d", tc.name, got, note, tc.want)
		}
	}
	if c := BurstCeiling(config.AutoFloat{Value: 0}, base, mac); c != 0 {
		t.Errorf("memory_burst_gb = 0 gives ceiling %d", c)
	}
	gpu := platform.Memory{TotalBytes: 32 * gib, VRAMBytes: 12 * GB}
	if c := BurstCeiling(config.AutoFloat{Auto: true}, 11*GB, gpu); c != 11*GB {
		t.Errorf("a discrete GPU grew to %d", c)
	}
}

// Models and sizes from ORCHESTRATOR.md "Measured fit on a 16 GB Mac".
func profiles() []Profile {
	chat := []backends.Capability{backends.CapChat, backends.CapTools, backends.CapSchema}
	mk := func(name string, size float64, load float64, q map[string]float64, extra ...backends.Capability) Profile {
		return Profile{Info: backends.ModelInfo{Name: name, Capabilities: append(append([]backends.Capability{}, chat...), extra...)},
			Ctx: 8192, SizeBytes: int64(size * GB), LoadSeconds: load, Quality: q}
	}
	return []Profile{
		mk("qwen3:4b", 3.9, 2.8, map[string]float64{"*": 0.62, "code": 0.5}),
		mk("qwen2.5-coder:7b", 5.1, 4.3, map[string]float64{"*": 0.56, "code": 0.78}),
		mk("qwen3:8b", 6.6, 7, map[string]float64{"*": 0.75}),
		mk("qwen2.5vl:7b", 7.3, 6.1, map[string]float64{"*": 0.5}, backends.CapVision),
	}
}

func cfg() config.Orchestrator { return config.Defaults().Orchestrator }

func state16(primary string, since time.Duration) State {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return State{Budget: 8 * GB, Resident: []backends.LoadedModel{{Name: "tev1:0.8b", SizeBytes: 2.7 * GB}},
		Primary: primary, PrimarySince: now.Add(-since), Now: now}
}

func TestExampleA_ResearchNoSwap(t *testing.T) {
	d, err := Router{Cfg: cfg(), Profiles: profiles()}.Route(Classification{TaskType: "research", Confidence: 0.91, Needs: []string{"web"}}, Request{}, state16("qwen3:4b", time.Hour))
	if err != nil || d.Model != "qwen3:4b" || d.Swap {
		t.Fatalf("%+v %v\n%s", d, err, d.Explanation.Text())
	}
	// qwen3:8b scores higher on quality but doesn't fit next to the resident decision model.
	for _, c := range d.Explanation.Cands {
		if c.Model == "qwen3:8b" && !strings.Contains(c.Rejected, "doesn't fit") {
			t.Errorf("8b: %+v", c)
		}
	}
}

func TestExampleB_CodeRuleThenNoSwapBack(t *testing.T) {
	r := Router{Cfg: cfg(), Profiles: profiles()}
	d, _ := r.Route(Classification{TaskType: "code", Confidence: 0.93}, Request{}, state16("qwen3:4b", time.Hour))
	if d.Model != "qwen2.5-coder:7b" || !d.Swap || !strings.Contains(d.Explanation.Why, "user rule") {
		t.Fatalf("%+v\n%s", d, d.Explanation.Text())
	}
	// "thanks, explain it simply": qwen3:4b is 0.06 better, below min_swap_gain → stay.
	d, _ = r.Route(Classification{TaskType: "chat", Confidence: 0.9}, Request{}, state16("qwen2.5-coder:7b", time.Hour))
	if d.Model != "qwen2.5-coder:7b" || d.Swap {
		t.Fatalf("swapped back: %+v\n%s", d, d.Explanation.Text())
	}
}

func TestExampleC_VisionUnloadsResident(t *testing.T) {
	c := Classification{TaskType: "vision", Confidence: 1, Needs: []string{"vision"}}
	d, err := Router{Cfg: cfg(), Profiles: profiles()}.Route(c, Request{Images: 1}, state16("qwen3:4b", time.Hour))
	if err != nil || d.Model != "qwen2.5vl:7b" || !d.UnloadResident {
		t.Fatalf("%+v %v\n%s", d, err, d.Explanation.Text())
	}
}

func TestUncertainTypeDoesntFireRules(t *testing.T) {
	d, _ := Router{Cfg: cfg(), Profiles: profiles()}.Route(Classification{TaskType: "code", Confidence: 0.4, Uncertain: true}, Request{}, state16("qwen3:4b", time.Hour))
	if d.Model != "qwen3:4b" {
		t.Fatalf("%+v", d)
	}
}

func TestHoldAndStreaming(t *testing.T) {
	c := cfg()
	c.MinSwapGain = 0.05
	r := Router{Cfg: c, Profiles: profiles()}
	cl := Classification{TaskType: "creative", Confidence: 0.9}
	// The coder is loaded; qwen3:4b is 0.06 better, enough with min_swap_gain 0.05.
	if d, _ := r.Route(cl, Request{}, state16("qwen2.5-coder:7b", time.Hour)); d.Model != "qwen3:4b" {
		t.Fatalf("no soft swap: %s", d.Explanation.Text())
	}
	if d, _ := r.Route(cl, Request{}, state16("qwen2.5-coder:7b", 30*time.Second)); d.Model != "qwen2.5-coder:7b" || !strings.Contains(d.Explanation.Why, "hold") {
		t.Errorf("hold ignored: %s", d.Explanation.Text())
	}
	st := state16("qwen2.5-coder:7b", time.Hour)
	st.Streaming = true
	if d, _ := r.Route(cl, Request{}, st); d.Swap {
		t.Errorf("soft swap while streaming: %s", d.Explanation.Text())
	}
	// A hard need still swaps, after the stream ends.
	st.Primary = "qwen3:4b"
	d, _ := r.Route(Classification{TaskType: "vision", Needs: []string{"vision"}}, Request{}, st)
	if !d.Swap || !d.WaitForStream {
		t.Errorf("hard need while streaming: %+v", d)
	}
}

func TestSummariseUsesLoaded(t *testing.T) {
	d, _ := Router{Cfg: cfg(), Profiles: profiles()}.Route(Classification{TaskType: "summarise", Confidence: 0.9}, Request{}, state16("qwen2.5-coder:7b", time.Hour))
	if d.Model != "qwen2.5-coder:7b" {
		t.Fatalf("%+v", d)
	}
}

func TestNothingLoadedAndNoModel(t *testing.T) {
	r := Router{Cfg: cfg(), Profiles: profiles()}
	d, err := r.Route(Classification{TaskType: "chat", Confidence: 0.9}, Request{}, state16("", 0))
	if err != nil || d.Model != "qwen3:4b" {
		t.Fatalf("%+v %v", d, err)
	}
	r.Profiles = profiles()[:1]
	_, err = r.Route(Classification{TaskType: "vision", Needs: []string{"vision"}}, Request{}, state16("qwen3:4b", 0))
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v", err)
	}
}

func TestRuleSkippedWhenNotInstalled(t *testing.T) {
	c := cfg()
	c.Rules = append([]config.Rule{{When: config.RuleWhen{TaskType: "code"}, Use: "deepseek-coder:6.7b"}}, c.Rules...)
	d, _ := Router{Cfg: c, Profiles: profiles()}.Route(Classification{TaskType: "code", Confidence: 0.9}, Request{}, state16("qwen3:4b", time.Hour))
	if d.Model != "qwen2.5-coder:7b" || !strings.Contains(d.Explanation.Text(), "isn't installed") {
		t.Fatalf("%s", d.Explanation.Text())
	}
}

func TestBanditEvidenceMovesRouting(t *testing.T) {
	research := Classification{TaskType: "research", Confidence: 0.9}
	st := state16("qwen2.5-coder:7b", time.Hour)
	d, _ := Router{Cfg: cfg(), Profiles: profiles()}.Route(research, Request{}, st)
	if d.Model != "qwen2.5-coder:7b" {
		t.Fatalf("without evidence the 0.06 gain shouldn't swap: %s", d.Explanation.Text())
	}
	ev := map[string]Evidence{"qwen3:4b": {Alpha: 20}, "qwen2.5-coder:7b": {Beta: 20}}
	d, _ = Router{Cfg: cfg(), Profiles: profiles(), Evidence: ev}.Route(research, Request{}, st)
	if d.Model != "qwen3:4b" || !d.Swap {
		t.Fatalf("evidence ignored: %s", d.Explanation.Text())
	}
	for _, c := range d.Explanation.Cands {
		if c.Model == "qwen3:4b" && (c.Runs != 20 || c.Quality < 0.9) {
			t.Errorf("%+v", c)
		}
	}
}

func TestExploreSamplesOnlyTheLoadedModel(t *testing.T) {
	r := Router{Cfg: cfg(), Profiles: profiles(), Explore: true, Rand: rand.New(rand.NewPCG(1, 2))}
	d, _ := r.Route(Classification{TaskType: "research", Confidence: 0.9}, Request{}, state16("qwen3:4b", time.Hour))
	ex := d.Explanation
	if !strings.Contains(strings.Join(ex.Notes, " "), "exploring") {
		t.Fatalf("%s", ex.Text())
	}
	for _, c := range ex.Cands {
		switch c.Model {
		case "qwen3:4b":
			if c.Quality == 0.62 {
				t.Error("the loaded model wasn't sampled")
			}
		case "qwen2.5-coder:7b":
			if math.Abs(c.Quality-0.56) > 1e-9 {
				t.Errorf("a model that needs a swap was sampled: %v", c.Quality)
			}
		}
	}
}

func TestRewardAndBuckets(t *testing.T) {
	if r, ok := Reward("success", 1, 10, "balanced"); !ok || math.Abs(r-0.8) > 1e-9 {
		t.Errorf("success in 10 s: %v %v", r, ok)
	}
	if r, ok := Reward("fail", 0.8, 1, "balanced"); !ok || r != 0 {
		t.Errorf("fail: %v %v", r, ok)
	}
	if r, _ := Reward("success", 1, 600, "fast"); r != 0.5 {
		t.Errorf("penalty isn't capped at half: %v", r)
	}
	if _, ok := Reward("success", 0.5, 1, "balanced"); ok {
		t.Error("an unsure outcome counted")
	}
	if Bucket(-1) != 1 || Bucket(0) != 0 || Bucket(2) != 1 || Bucket(3.4) != 2 {
		t.Error("buckets")
	}
	r := rand.New(rand.NewPCG(3, 4))
	sum := 0.0
	for range 4000 {
		sum += betaSample(r, 2, 6)
	}
	if m := sum / 4000; math.Abs(m-0.25) > 0.02 {
		t.Errorf("Beta(2,6) mean %v", m)
	}
}

func TestBackgroundJobsKeepTheLoadedModel(t *testing.T) {
	r := Router{Cfg: cfg(), Profiles: profiles()}
	st := state16("qwen3:4b", time.Hour)
	st.Background, st.Idle = true, time.Minute
	d, _ := r.Route(Classification{TaskType: "research", Confidence: 0.9}, Request{}, st)
	if d.Model != "qwen3:4b" || d.Defer || !strings.Contains(d.Explanation.Why, "background job uses the loaded model") {
		t.Fatalf("%+v %s", d, d.Explanation.Why)
	}
	// A code job's rule wants the coder: it waits while you're active …
	d, _ = r.Route(Classification{TaskType: "code", Confidence: 0.9}, Request{}, st)
	if d.Model != "qwen2.5-coder:7b" || !d.Defer {
		t.Fatalf("%+v %s", d, d.Explanation.Why)
	}
	// … and swaps once you've been away 10 minutes.
	st.Idle = 11 * time.Minute
	if d, _ = r.Route(Classification{TaskType: "code", Confidence: 0.9}, Request{}, st); d.Defer || !d.Swap {
		t.Fatalf("%+v", d)
	}
	// Escalation rules out the model that already failed.
	r.Exclude = []string{"qwen3:4b"}
	st.Background = false
	if d, _ = r.Route(Classification{TaskType: "research", Confidence: 0.9}, Request{}, st); d.Model == "qwen3:4b" {
		t.Fatalf("excluded model picked: %s", d.Explanation.Text())
	}
}

func TestBatchOrderAndHandoff(t *testing.T) {
	// coder, 4b(loaded), coder, vl, 4b(loaded) → the loaded model's two first, then coder ×2, then vl.
	got := BatchOrder([]string{"coder", "4b", "coder", "vl", "4b"}, []bool{false, true, false, false, true})
	if fmt.Sprint(got) != "[1 4 0 2 3]" {
		t.Errorf("order %v", got)
	}
	mk := func(name string, q float64, caps ...backends.Capability) Profile {
		return Profile{Info: backends.ModelInfo{Name: name, Capabilities: caps}, Ctx: 8192, Quality: map[string]float64{"*": q}}
	}
	c := Classification{TaskType: "research", Needs: []string{"tools"}}
	driver := mk("qwen3:4b", 0.7, backends.CapChat, backends.CapTools)
	if ok, why := Handoff(c, driver, mk("coder", 0.6, backends.CapChat, backends.CapTools), Evidence{}, Evidence{}, 8192, 0.15); !ok || why == "" {
		t.Error("a slightly worse loaded model should finish the task")
	}
	if ok, _ := Handoff(c, driver, mk("coder", 0.3, backends.CapChat, backends.CapTools), Evidence{}, Evidence{}, 8192, 0.15); ok {
		t.Error("a much worse model must not take over")
	}
	if ok, _ := Handoff(c, driver, mk("vl", 0.7, backends.CapChat), Evidence{}, Evidence{}, 8192, 0.15); ok {
		t.Error("a model without tool calling can't drive a run with tools")
	}
	if ok, _ := Handoff(c, driver, mk("coder", 0.7, backends.CapChat, backends.CapTools), Evidence{}, Evidence{}, 16384, 0.15); ok {
		t.Error("too little context")
	}
}

// FR-B6c: a calibrated threshold for the deciding model replaces the default.
func TestClassifierThresholdPerModel(t *testing.T) {
	d := &fakeDecider{a: answer("code", conf(0.5))}
	d.a.Model = "tev1:0.8b"
	var asked string
	c := Classifier{Decider: d, ThresholdFor: func(m string) float64 { asked = m; return 0.45 }}
	got, _ := c.Classify(context.Background(), Request{Text: "hmm"})
	if asked != "tev1:0.8b" || got.Uncertain || got.Model != "tev1:0.8b" || got.Threshold != 0.45 {
		t.Fatalf("tuned 0.45: %+v (asked for %q)", got, asked)
	}
	c.ThresholdFor = func(string) float64 { return 0 }
	if got, _ = c.Classify(context.Background(), Request{Text: "hmm"}); !got.Uncertain || got.Threshold != 0.6 {
		t.Fatalf("no tuned value keeps 0.6: %+v", got)
	}
}

// A rule can be limited to a preset: the speed model answers by default and the quality model when
// the run asks for the best preset (`run --preset best`).
func TestRuleForAPreset(t *testing.T) {
	c := cfg()
	c.Rules = []config.Rule{
		{When: config.RuleWhen{Preset: "best"}, Use: "qwen2.5-coder:7b"},
		{When: config.RuleWhen{TaskType: "chat"}, Use: "qwen3:4b"},
	}
	chat := Classification{TaskType: "chat", Confidence: 0.9}
	for preset, want := range map[string]string{"best": "qwen2.5-coder:7b", "balanced": "qwen3:4b", "fast": "qwen3:4b"} {
		c.Preset = preset
		d, err := Router{Cfg: c, Profiles: profiles()}.Route(chat, Request{}, state16("", 0))
		if err != nil || d.Model != want || !strings.Contains(d.Explanation.Text(), "user rule") {
			t.Errorf("%s: %s %v\n%s", preset, d.Model, err, d.Explanation.Text())
		}
	}
	c.Preset = "best"
	if got := ruleText(c.Rules[0]); got != "preset best" {
		t.Errorf("rule text %q", got)
	}
}
