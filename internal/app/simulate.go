package app

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"noskill/internal/agent"
	"noskill/internal/approvals"
	"noskill/internal/backends"
	"noskill/internal/eval"
	"noskill/internal/store"
)

// The learning exit check (ROADMAP Phase 2 "Exit", LEARNING.md "Measuring"): weeks of simulated use
// on one model, then the held-out multi-step tasks again. A simulated day is a few requests from
// the training workload, each rated good or bad by its check the way a user would rate it; a
// simulated night is the learning worker's work for that night (mine, draft, refine, merge,
// within the nightly caps), done in the foreground. The simulated user says yes when a learned
// skill asks before its first use. Wall-clock rules (idle time, 30-day staleness) don't apply.
//
// It must run in a home of its own: it writes runs, traces and learned skills.

// SimOptions configures a simulation.
type SimOptions struct {
	Model    string
	Days     int // simulated days (28 = 4 weeks)
	PerDay   int // requests a day
	Seed     int64
	Progress func(SimProgress)
}

// SimProgress is reported after each request and each night.
type SimProgress struct {
	Phase     string // before | day | night | after | done
	Day       int
	Done      int // work items finished (eval tasks, requests, nights)
	Total     int
	Now       string
	Learned   []string // learned skill name (status)
	GoodToday int
	RunsToday int
}

// SimReport is the result: the held-out suite before and after, and what was learned.
type SimReport struct {
	Before, After *EvalOutcome
	Requests      int
	Good          int
	Nights        []string // what each night did
	Learned       []string // name (status, version)
	// Measured on the held-out tasks.
	SuccessBefore, SuccessAfter float64 // pass rate, 0–1
	StepsBefore, StepsAfter     float64 // median model steps over passed tasks
	LearnedShare                float64 // share of "after" tasks that used a learned skill
	SuccessGain, StepsCut       float64 // relative: (after-before)/before and (before-after)/before
	Pass                        bool    // either is ≥ 0.15
}

// Simulate runs the exit check. The home must be empty of learned skills and eval runs.
func (a *App) Simulate(ctx context.Context, o SimOptions) (*SimReport, error) {
	if o.Days <= 0 {
		o.Days = 28
	}
	if o.PerDay <= 0 {
		o.PerDay = 3
	}
	if n := len(a.learnedState()); n > 0 {
		return nil, &UserError{fmt.Sprintf("this home already has %d learned skill(s); simulate in a fresh home (--home)", n)}
	}
	if runs, _ := a.DB.EvalRuns(ctx, "", 1); len(runs) > 0 {
		return nil, &UserError{"this home already has eval runs; simulate in a fresh home (--home)"}
	}
	if !a.Cfg.Learning.Enabled {
		return nil, &UserError{"learning is off ([learning] enabled = false)"}
	}
	m, err := a.FindModel(ctx, o.Model)
	if err != nil {
		return nil, err
	}
	held, err := a.EvalTasks(nil, []string{"multi"}, false)
	if err != nil {
		return nil, err
	}
	held = slices.DeleteFunc(held, func(t eval.Task) bool { return t.Source != "builtin" })
	work, err := eval.Workload()
	if err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(o.Seed))
	total := 2*len(held) + o.Days*o.PerDay + o.Days
	done := 0
	rep := &SimReport{}
	say := func(p SimProgress) {
		if o.Progress == nil {
			return
		}
		p.Done, p.Total = done, total
		for n, v := range a.learnedState() {
			st := "?"
			if s, ok := a.Skills.Get(n); ok {
				st = s.Status()
			}
			p.Learned = append(p.Learned, fmt.Sprintf("%s (%s %s)", n, st, v))
		}
		sort.Strings(p.Learned)
		o.Progress(p)
	}
	evalProgress := func(phase string) func(string) {
		return func(s string) {
			done++
			say(SimProgress{Phase: phase, Now: s})
		}
	}

	// Before: no learned skills exist yet; NoLearned says so explicitly.
	rep.Before, err = a.Eval(ctx, EvalOptions{Model: m.Name, Tasks: held, Suite: "multi", Note: "simulate: before", NoLearned: true, Progress: evalProgress("before")})
	if err != nil {
		return nil, err
	}

	for day := 1; day <= o.Days; day++ {
		good, runs := 0, 0
		for i := 0; i < o.PerDay; i++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			t := work[rng.Intn(len(work))]
			ok, why, err := a.simRequest(ctx, m.Name, t)
			if err != nil {
				return nil, err
			}
			runs++
			rep.Requests++
			if ok {
				good++
				rep.Good++
			}
			done++
			say(SimProgress{Phase: "day", Day: day, Now: fmt.Sprintf("day %d: %s %s", day, t.ID, map[bool]string{true: "good", false: "bad: " + why}[ok]), GoodToday: good, RunsToday: runs})
		}
		did, err := a.simNight(ctx, day)
		if err != nil {
			return nil, err
		}
		rep.Nights = append(rep.Nights, fmt.Sprintf("day %d: %d/%d good; %s", day, good, runs, orStr(did, "nothing to learn")))
		done++
		say(SimProgress{Phase: "night", Day: day, Now: fmt.Sprintf("night %d: %s", day, orStr(did, "nothing to learn"))})
	}

	rep.After, err = a.Eval(ctx, EvalOptions{Model: m.Name, Tasks: held, Suite: "multi", Note: "simulate: after", Progress: evalProgress("after")})
	if err != nil {
		return nil, err
	}
	for n, v := range a.learnedState() {
		st := "?"
		if s, ok := a.Skills.Get(n); ok {
			st = s.Status()
		}
		rep.Learned = append(rep.Learned, fmt.Sprintf("%s (%s, %s)", n, st, v))
	}
	sort.Strings(rep.Learned)
	rep.measure()
	say(SimProgress{Phase: "done", Now: rep.Summary()})
	return rep, nil
}

// simRequest is one request of a simulated day, rated by its check. ok is the rating.
func (a *App) simRequest(ctx context.Context, model string, t eval.Task) (ok bool, why string, err error) {
	dir, err := os.MkdirTemp("", "noskill-sim-")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(dir)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	for name, body := range t.Files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			return false, "", err
		}
	}
	var called, effects []string
	req := RunRequest{
		Source: "cli", Model: model, MaxSteps: 12,
		Messages: []backends.Message{{Role: backends.RoleUser, Content: strings.ReplaceAll(t.Prompt, "{dir}", filepath.ToSlash(dir))}},
		// Like the eval tasks, the request names the skills it needs and a matching learned skill
		// (a candidate on trial too) joins them.
		Skills: t.Skills, plusLearned: true,
		Allow: []string{"read:" + dir, "write:" + dir},
		// The simulated user is at the terminal and says yes: to confirm actions in the throwaway
		// folder and to a learned skill's first use.
		Prompt: func(context.Context, approvals.Item) (bool, error) { return true, nil },
		OnEvent: func(e agent.Event) {
			if e.Kind == agent.EventToolCall && !slices.Contains(called, e.Tool) {
				called = append(called, e.Tool)
			}
			if e.Kind == agent.EventFileEffect && !slices.Contains(effects, e.Op) {
				effects = append(effects, e.Op)
			}
		},
	}
	out, runErr := a.Run(ctx, req)
	a.bg.Wait() // the decision judge mustn't land after the rating
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	switch {
	case runErr != nil:
		why = runErr.Error()
	case out == nil:
		why = "no result"
	default:
		ok, why = t.Check.Verdict(out.Answer)
		if ok && !t.ToolsMet(called, a.learnedTools(called), effects) {
			ok, why = false, "didn't call "+strings.Join(t.Tools, " or ")
		}
		if ok {
			ok, why = t.End.Verdict(dir)
		}
	}
	if out == nil || out.RunID == "" {
		return ok, why, nil
	}
	rating := "bad"
	if ok {
		rating = "good"
	}
	if _, err := a.Rate(ctx, out.RunID, rating, "simulated user"); err != nil {
		return ok, why, fmt.Errorf("rating run %s: %w", out.RunID, err)
	}
	return ok, why, nil
}

// simNight is one night of the learning worker: the curator once a week, then queued items within
// the nightly caps.
func (a *App) simNight(ctx context.Context, day int) (string, error) {
	var did []string
	if day%7 == 0 {
		if r, err := a.Curate(ctx); err != nil {
			return "", err
		} else if r != "" {
			did = append(did, "curator: "+r)
		}
	}
	caps := map[string]int{"draft": a.Cfg.Learning.MaxDraftsPerNight, "refine": a.Cfg.Learning.MaxRefinesPerNight, "merge": 1 << 30}
	for {
		items, err := a.DB.ListLearning(ctx, 200)
		if err != nil {
			return "", err
		}
		var next *store.LearnItem
		for i := len(items) - 1; i >= 0; i-- { // oldest first
			if it := items[i]; it.Status == "pending" && caps[it.Kind] > 0 {
				next = &items[i]
				break
			}
		}
		if next == nil {
			break
		}
		caps[next.Kind]--
		it, err := a.LearnNow(ctx, next.ID)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch {
		case err != nil:
			did = append(did, fmt.Sprintf("%s failed: %v", next.Kind, err))
		default:
			did = append(did, fmt.Sprintf("%s: %s", it.Kind, orStr(it.Result, it.Status)))
		}
	}
	return strings.Join(did, "; "), nil
}

func (r *SimReport) measure() {
	rate := func(rs []store.EvalResult) float64 {
		n, p := 0, 0
		for _, x := range rs {
			if !x.Skipped {
				n++
				if x.Passed {
					p++
				}
			}
		}
		if n == 0 {
			return 0
		}
		return float64(p) / float64(n)
	}
	steps := func(rs []store.EvalResult) float64 {
		var s []int
		for _, x := range rs {
			if x.Passed {
				s = append(s, x.Steps)
			}
		}
		return median(s)
	}
	r.SuccessBefore, r.SuccessAfter = rate(r.Before.Results), rate(r.After.Results)
	r.StepsBefore, r.StepsAfter = steps(r.Before.Results), steps(r.After.Results)
	used := 0
	for _, x := range r.After.Results {
		if len(x.Learned) > 0 {
			used++
		}
	}
	if len(r.After.Results) > 0 {
		r.LearnedShare = float64(used) / float64(len(r.After.Results))
	}
	if r.SuccessBefore > 0 {
		r.SuccessGain = (r.SuccessAfter - r.SuccessBefore) / r.SuccessBefore
	} else if r.SuccessAfter > 0 {
		r.SuccessGain = 1
	}
	if r.StepsBefore > 0 && r.StepsAfter > 0 {
		r.StepsCut = (r.StepsBefore - r.StepsAfter) / r.StepsBefore
	}
	r.Pass = r.SuccessGain >= 0.15 || (r.StepsCut >= 0.15 && r.SuccessAfter >= r.SuccessBefore)
}

func median(s []int) float64 {
	if len(s) == 0 {
		return 0
	}
	sort.Ints(s)
	if len(s)%2 == 1 {
		return float64(s[len(s)/2])
	}
	return float64(s[len(s)/2-1]+s[len(s)/2]) / 2
}

// Summary is the verdict in two lines.
func (r *SimReport) Summary() string {
	verdict := "FAIL"
	if r.Pass {
		verdict = "PASS"
	}
	return fmt.Sprintf("%s: held-out success %.0f%% → %.0f%% (%+.0f%%), median steps %.1f → %.1f (%+.0f%%), learned skills used in %.0f%% of tasks\n%d simulated requests, %d rated good; learned: %s",
		verdict, 100*r.SuccessBefore, 100*r.SuccessAfter, 100*r.SuccessGain, r.StepsBefore, r.StepsAfter, -100*r.StepsCut, 100*r.LearnedShare,
		r.Requests, r.Good, orStr(strings.Join(r.Learned, ", "), "none"))
}
