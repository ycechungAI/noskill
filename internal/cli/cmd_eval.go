package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/eval"
)

// cmdEval is `noskill eval`: the checked tasks (FR-B4, LEARNING.md "Evaluation").
func cmdEval(c *cmd, args []string) int {
	if len(args) == 0 {
		args = []string{"report"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	rest := args[1:]
	switch args[0] {
	case "list":
		return evalList(c, a, rest)
	case "run":
		return evalRun(c, ctx, a, rest, false)
	case "gate":
		return evalRun(c, ctx, a, rest, true)
	case "report":
		return evalReport(c, ctx, a, rest)
	case "history":
		return evalHistory(c, ctx, a, rest)
	case "pin":
		return evalPin(c, ctx, a, rest)
	case "simulate":
		return evalSimulate(c, ctx, a, rest)
	}
	return c.usageErr("unknown eval command %q (list, run, report, history, pin, gate, simulate)", args[0])
}

type evalFlags struct {
	model        string
	types, tasks multiFlag
	quick        bool
	noLearned    bool
}

func (f *evalFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.model, "model", "", "the model to test (default: the smallest installed chat model)")
	fs.Var(&f.types, "type", "only this task type (repeatable)")
	fs.Var(&f.tasks, "task", "only this task id (repeatable)")
	fs.BoolVar(&f.quick, "quick", false, "only the short micro-eval that `models bench` runs")
	fs.BoolVar(&f.noLearned, "no-learned", false, "keep learned skills out of agent tasks (to compare with and without them)")
}

func evalList(c *cmd, a *app.App, args []string) int {
	fs := flag.NewFlagSet("eval list", flag.ContinueOnError)
	var f evalFlags
	f.bind(fs)
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ts, err := a.EvalTasks(f.tasks, f.types, f.quick)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(ts)
		return ExitOK
	}
	rows := [][]string{{"ID", "TYPE", "KIND", "SOURCE", "PROMPT"}}
	for _, t := range ts {
		kind := "plain"
		if t.Agent() {
			kind = "agent"
		}
		p := strings.Join(strings.Fields(t.Prompt), " ")
		if len(p) > 60 {
			p = p[:59] + "…"
		}
		rows = append(rows, []string{t.ID, t.Type, kind, t.Source, p})
	}
	c.table(rows)
	return ExitOK
}

func evalRun(c *cmd, ctx context.Context, a *app.App, args []string, gate bool) int {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	var f evalFlags
	f.bind(fs)
	rollback := new(bool)
	if gate {
		rollback = fs.Bool("rollback", false, "when results fell, put back each learned skill that changed since the last run and took part in a task that fell")
	}
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ts, err := a.EvalTasks(f.tasks, f.types, f.quick)
	if err != nil {
		return c.fail(err)
	}
	model := f.model
	if model == "" {
		if model, err = a.CheapestChat(ctx); err != nil {
			return c.fail(err)
		}
		c.infof("testing %s (pick another with --model)\n", model)
	}
	suite := "all"
	switch {
	case f.quick:
		suite = "quick"
	case len(f.types) > 0 || len(f.tasks) > 0:
		suite = strings.Join(append(append([]string{}, f.types...), f.tasks...), ",")
	}
	if f.noLearned {
		suite += " (no learned)" // compared only with runs that also left them out
	}
	o := app.EvalOptions{Model: model, Tasks: ts, Suite: suite, NoLearned: f.noLearned, Progress: func(s string) { c.infof("  %s\n", s) }}
	var out *app.EvalOutcome
	var rolled map[string]string
	var kept []string
	if gate {
		g, err := a.EvalGate(ctx, o, *rollback)
		if g != nil {
			out, rolled, kept = g.Outcome, g.RolledBack, g.Kept
		}
		if err != nil {
			return c.fail(err)
		}
	} else if out, err = a.Eval(ctx, o); err != nil {
		return c.fail(err)
	}
	code := printEval(c, out, rolled, gate)
	if len(kept) > 0 && !c.g.json {
		c.outf("! new since the last run and used in a task that fell (no older version): %s\n", strings.Join(kept, ", "))
	}
	if gate && len(out.Regressions) > 0 && len(rolled) == 0 && len(kept) == 0 && !c.g.json {
		c.outf("no learned skill took part in the tasks that fell, so nothing was rolled back\n")
	}
	return code
}

func printEval(c *cmd, out *app.EvalOutcome, rolled map[string]string, gate bool) int {
	if c.g.json {
		c.printJSON(map[string]any{"run": out.Run, "results": out.Results, "regressions": out.Regressions, "rolled_back": rolled})
	} else {
		c.outf("%s", eval.Text(out.Run, out.Results, out.PrevResults))
		for _, r := range out.Regressions {
			c.outf("▼ %s fell from %d/%d to %d/%d", r.Type, r.Before.Passed, r.Before.Total, r.After.Passed, r.After.Total)
			if len(r.Flipped) > 0 {
				c.outf(" (%s)", strings.Join(r.Flipped, ", "))
			}
			c.outf("\n")
		}
		for n, v := range rolled {
			c.outf("↩ %s is back at version %s\n", n, v)
		}
	}
	if gate && len(out.Regressions) > 0 {
		return ExitError
	}
	return ExitOK
}

func evalReport(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("eval report", flag.ContinueOnError)
	pos, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	id := "last"
	if len(pos) > 0 {
		id = pos[0]
	}
	out, err := a.EvalRun(ctx, id)
	if err != nil {
		return c.fail(err)
	}
	return printEval(c, out, nil, false)
}

func evalHistory(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("eval history", flag.ContinueOnError)
	model := fs.String("model", "", "only this model")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	runs, err := a.DB.EvalRuns(ctx, *model, 30)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(runs)
		return ExitOK
	}
	if len(runs) == 0 {
		c.outf("no eval runs yet: `noskill eval run`\n")
		return ExitOK
	}
	rows := [][]string{{"RUN", "WHEN", "MODEL", "SUITE", "PASSED"}}
	for _, r := range runs {
		rows = append(rows, []string{shortID(r.ID), r.StartedAt.Format("01-02 15:04"), r.Model, r.Suite, fmt.Sprintf("%d/%d", r.Passed, r.Tasks)})
	}
	c.table(rows)
	return ExitOK
}

func evalPin(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("eval pin", flag.ContinueOnError)
	id := fs.String("id", "", "a name for the task (default: pin-<end of the run id>)")
	var contains multiFlag
	fs.Var(&contains, "contains", "text a correct answer must contain (repeatable)")
	pos, err := c.parse(fs, args)
	if err != nil || len(pos) > 1 {
		return c.usageErr("usage: noskill eval pin [<run-id>|last] [--id name] [--contains text]")
	}
	run := "last"
	if len(pos) == 1 {
		run = pos[0]
	}
	t, err := a.PinEval(ctx, run, *id, contains)
	if err != nil {
		return c.fail(err)
	}
	c.outf("pinned %s (%s). `noskill eval run` includes it from now on.\n", t.ID, t.Type)
	return ExitOK
}

// evalSimulate is the learning exit check (ROADMAP Phase 2): weeks of simulated use, then the
// held-out multi-step tasks again. It refuses a home that already has learned skills or eval runs.
func evalSimulate(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("eval simulate", flag.ContinueOnError)
	model := fs.String("model", "", "the model the simulated user works with (default: the smallest installed chat model)")
	days := fs.Int("days", 28, "simulated days")
	perDay := fs.Int("per-day", 3, "requests a simulated day")
	seed := fs.Int64("seed", 1, "seed for picking the day's requests")
	progress := fs.String("progress-json", "", "also write progress to this JSON file (for a status page)")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	m := *model
	if m == "" {
		var err error
		if m, err = a.CheapestChat(ctx); err != nil {
			return c.fail(err)
		}
	}
	c.infof("simulating %d days × %d requests on %s in %s\n", *days, *perDay, m, a.Dirs.Data)
	started := time.Now()
	var notes []string
	write := func(p app.SimProgress, state string) {
		if *progress == "" {
			return
		}
		if p.Phase == "night" {
			notes = append(notes, p.Now)
			if len(notes) > 12 {
				notes = notes[len(notes)-12:]
			}
		}
		b, _ := json.Marshal(map[string]any{"name": "eval simulate (" + m + ")", "state": state, "done": p.Done, "total": p.Total,
			"now": p.Now, "notes": append(append([]string{}, notes...), "learned: "+strings.Join(p.Learned, ", ")), "started": started.Unix()})
		tmp := *progress + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, *progress)
		}
	}
	rep, err := a.Simulate(ctx, app.SimOptions{Model: m, Days: *days, PerDay: *perDay, Seed: *seed, Progress: func(p app.SimProgress) {
		c.infof("[%d/%d] %s\n", p.Done, p.Total, p.Now)
		state := "running"
		if p.Phase == "done" {
			state = "done"
		}
		write(p, state)
	}})
	if err != nil {
		write(app.SimProgress{Now: err.Error()}, "failed")
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(rep)
	} else {
		c.outf("before (no learned skills):\n%s\nafter %d days:\n%s\n", eval.Text(rep.Before.Run, rep.Before.Results, nil), *days,
			eval.Text(rep.After.Run, rep.After.Results, rep.Before.Results))
		for _, n := range rep.Nights {
			c.outf("  %s\n", n)
		}
		c.outf("%s\n", rep.Summary())
	}
	if !rep.Pass {
		return ExitError
	}
	return ExitOK
}
