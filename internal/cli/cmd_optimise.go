package cli

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"noskill/internal/app"
)

// skillsOptimise is `noskill skills optimise` (FR-L9, ADR-0014): rewrite a built-in or learned
// skill's instructions and keep the rewrite only if the checked tasks say it's better.
func skillsOptimise(c *cmd, ctx context.Context, a *app.App, args []string) int {
	usage := "usage: noskill skills optimise <name> [--budget 4] [--model m] [--tasks n] [--adopt] [--dry-run] [--force] | adopt <name> | revert <name> [--to <version>] | show <name>"
	if len(args) == 0 {
		return c.usageErr(usage)
	}
	switch args[0] {
	case "adopt":
		if len(args) != 2 {
			return c.usageErr("usage: noskill skills optimise adopt <name>")
		}
		n, err := a.OptimiseAdopt(ctx, args[1])
		if err != nil {
			return c.fail(err)
		}
		c.outf("%s now uses tuned version %d. `noskill skills optimise revert %s` goes back to the built-in instructions.\n", args[1], n, args[1])
		return ExitOK
	case "revert":
		fs := flag.NewFlagSet("skills optimise revert", flag.ContinueOnError)
		to := fs.Int("to", 0, "an earlier tuned version to use instead (0 = the built-in instructions)")
		rest, err := c.parse(fs, args[1:])
		if err != nil || len(rest) != 1 {
			return c.usageErr("usage: noskill skills optimise revert <name> [--to <version>]")
		}
		if err := a.OptimiseRevert(ctx, rest[0], *to, "reverted by hand"); err != nil {
			return c.fail(err)
		}
		if *to > 0 {
			c.outf("%s uses tuned version %d.\n", rest[0], *to)
		} else {
			c.outf("%s uses its built-in instructions again (the tuned versions are kept).\n", rest[0])
		}
		return ExitOK
	case "show":
		if len(args) != 2 {
			return c.usageErr("usage: noskill skills optimise show <name>")
		}
		st, err := a.OptimiseStatus(args[1])
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(st)
			return ExitOK
		}
		if len(st.Info.Versions) == 0 {
			c.outf("%s has never been optimised. `noskill skills optimise %s` tries.\n", st.Skill, st.Skill)
			return ExitOK
		}
		inUse := "the built-in instructions"
		if st.InUse > 0 {
			inUse = "tuned version " + strconv.Itoa(st.InUse)
		}
		c.outf("%s uses %s. Current tuned version: %d (%s)", st.Skill, inUse, st.Info.Current, st.Info.Status)
		if st.Stale {
			c.outf(", made from an older built-in: not used")
		}
		c.outf("\n")
		rows := [][]string{{"VERSION", "MADE", "TRAINING", "HELD-OUT", "BEFORE"}}
		for _, v := range st.Info.Versions {
			rows = append(rows, []string{strconv.Itoa(v.N), v.At.Format("2006-01-02 15:04"), v.Train.String(), v.Held.String(), v.Base.String()})
		}
		c.table(rows)
		if st.Diff != "" {
			c.outf("\n%s", st.Diff)
		}
		return ExitOK
	}
	fs := flag.NewFlagSet("skills optimise", flag.ContinueOnError)
	budget := fs.Int("budget", 4, "rewrites to try")
	model := fs.String("model", "", "the model the tasks run on (default: the smallest chat model)")
	maxTasks := fs.Int("tasks", 0, "use at most this many training tasks (at least 5), spread evenly; for a quick run")
	adopt := fs.Bool("adopt", false, "a built-in's accepted rewrite goes straight into use (revert undoes it)")
	dry := fs.Bool("dry-run", false, "a quick trial (8 tasks, 2 rewrites unless you say otherwise): the rewrite isn't saved as a version")
	force := fs.Bool("force", false, "run on battery too")
	rest, err := c.parse(fs, args)
	if err != nil || len(rest) != 1 {
		return c.usageErr(usage)
	}
	if *dry { // a trial that takes hours isn't one: keep it short unless asked
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["tasks"] {
			*maxTasks = 8
		}
		if !set["budget"] {
			*budget = 2
		}
	}
	out, err := a.OptimiseSkill(ctx, app.OptimiseOptions{Skill: rest[0], Budget: *budget, Model: *model, MaxTasks: *maxTasks, Adopt: *adopt, DryRun: *dry, Force: *force,
		Progress: func(s string) { c.infof("%s\n", s) }})
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(out)
		return ExitOK
	}
	r := out.Result
	c.outf("Optimised %s on %s (rewrites by %s): %d training tasks, %d held out.\n", out.Skill, out.Model, out.Author, len(out.Train), len(out.Held))
	for i, cand := range r.Candidates {
		label := "current"
		if i > 0 {
			label = fmt.Sprintf("rewrite %d", i)
		}
		if cand.Dropped != "" {
			c.outf("  %-10s dropped: %s\n", label, cand.Dropped)
		} else {
			c.outf("  %-10s %s\n", label, cand.Score)
		}
	}
	switch {
	case !r.Accepted:
		c.outf("Kept the current instructions: %s.\n", r.Why)
	case *dry:
		c.outf("Would keep rewrite %d (%s). Dry run: not saved as a version; run it without --dry-run to keep it.\n", r.Best, r.Why)
	case out.Problem != "":
		c.outf("Not kept: %s.\n", out.Problem)
	default:
		c.outf("Kept rewrite %d: %s.\n%s.\n", r.Best, r.Why, strings.TrimSuffix(out.Saved, "."))
	}
	if out.Report != "" {
		c.outf("Every rewrite and score is in %s\n", out.Report)
	}
	return ExitOK
}
