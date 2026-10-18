package cli

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"noskill/internal/app"
)

// cmdTrain is `noskill train` (FR-L8, ADR-0015): export good runs as a dataset, train a LoRA on it
// with MLX, and adopt the result once it beats its base on the checked tasks.
func cmdTrain(c *cmd, args []string) int {
	usage := "usage: noskill train export [--task t] [--since 30d] [--base m] | lora --data <id> --base <model> --reviewed [--iters n] [--dry-run] [--force] | list | adopt <n> | revert <n>"
	if len(args) == 0 {
		return c.usageErr(usage)
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "export":
		fs := flag.NewFlagSet("train export", flag.ContinueOnError)
		task := fs.String("task", "", "only runs of this task type (code, files, …)")
		since := fs.String("since", "30d", "only runs this recent (30d, 72h)")
		base := fs.String("base", "", "the model it will train (sets the small-model prompt)")
		if rest, err := c.parse(fs, args[1:]); err != nil || len(rest) != 0 {
			return c.usageErr("usage: noskill train export [--task t] [--since 30d] [--base m]")
		}
		age, err := parseAge(*since)
		if err != nil {
			return c.usageErr("--since: %v", err)
		}
		d, err := a.TrainExport(ctx, app.TrainExportOptions{Task: *task, Since: age, Base: *base})
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(d)
			return ExitOK
		}
		if d.Runs == 0 {
			c.outf("No run to train on.\n")
		} else {
			c.outf("Dataset %s: %d runs, %d examples (%d to train, %d to check).\n", d.ID, d.Runs, d.Examples, d.Train, d.Valid)
		}
		if len(d.Skipped) > 0 {
			c.outf("Left out:\n")
			for _, kv := range sortedCounts(d.Skipped) {
				c.outf("  %4d  %s\n", kv.n, kv.k)
			}
		}
		if d.Runs > 0 {
			c.outf("Read %s/REVIEW.md before training.", d.Dir)
			if need := a.Cfg.Learning.LoRA.MinRuns; d.Runs < need {
				c.outf(" Training needs %d good runs (learning.lora.min_runs); this has %d.", need, d.Runs)
			}
			c.outf("\n")
		}
		return ExitOK
	case "lora":
		fs := flag.NewFlagSet("train lora", flag.ContinueOnError)
		data := fs.String("data", "", "the dataset id from `train export`")
		base := fs.String("base", "", "the Ollama model to train")
		reviewed := fs.Bool("reviewed", false, "you read the dataset's REVIEW.md")
		iters := fs.Int("iters", 0, "training iterations (0 = two epochs, 100–1000)")
		dry := fs.Bool("dry-run", false, "check and print the commands; run nothing")
		force := fs.Bool("force", false, "run on battery too")
		if rest, err := c.parse(fs, args[1:]); err != nil || len(rest) != 0 || *data == "" || *base == "" {
			return c.usageErr("usage: noskill train lora --data <id> --base <model> --reviewed [--iters n] [--dry-run] [--force]")
		}
		p, err := a.TrainLoRA(ctx, app.TrainOptions{Data: *data, Base: *base, Iters: *iters, Reviewed: *reviewed, Force: *force, DryRun: *dry,
			Progress: func(s string) { c.infof("%s\n", s) }})
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(p)
			return ExitOK
		}
		r := p.Run
		if *dry {
			c.outf("Would train %s as %s on %s (%d iterations), then benchmark it on %d checked tasks:\n", r.Base, r.Tag, r.Data, r.Iters, len(r.Tasks))
			for _, cmd := range p.Commands {
				c.outf("  %s\n", strings.Join(cmd, " "))
			}
			return ExitOK
		}
		c.outf("Training run %d (%s): %s, %s.\n", r.N, r.Tag, r.Status, r.Why)
		return ExitOK
	case "list":
		runs := a.TrainRuns()
		if c.g.json {
			c.printJSON(map[string]any{"datasets": a.Datasets(), "runs": runs})
			return ExitOK
		}
		ds := a.Datasets()
		if len(ds) == 0 && len(runs) == 0 {
			c.outf("Nothing yet. `noskill train export` makes a dataset of your good runs.\n")
			return ExitOK
		}
		if len(ds) > 0 {
			rows := [][]string{{"DATASET", "RUNS", "EXAMPLES", "TASK"}}
			for _, d := range ds {
				rows = append(rows, []string{d.ID, strconv.Itoa(d.Runs), strconv.Itoa(d.Examples), orDash(d.Task)})
			}
			c.table(rows)
		}
		if len(runs) > 0 {
			if len(ds) > 0 {
				c.outf("\n")
			}
			rows := [][]string{{"RUN", "MODEL", "BASE", "TASK", "STATUS", "SCORE", "BASE SCORE"}}
			for _, r := range runs {
				rows = append(rows, []string{strconv.Itoa(r.N), r.Tag, r.Base, orDash(r.Task), r.Status, r.Score.String(), r.BaseScore.String()})
			}
			c.table(rows)
		}
		return ExitOK
	case "adopt", "revert":
		if len(args) != 2 {
			return c.usageErr("usage: noskill train %s <n>", args[0])
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return c.usageErr("usage: noskill train %s <n> (the run number from `noskill train list`)", args[0])
		}
		var r *app.TrainRun
		if args[0] == "adopt" {
			r, err = a.TrainAdopt(n)
		} else {
			r, err = a.TrainRevert(n)
		}
		if err != nil {
			return c.fail(err)
		}
		if args[0] == "adopt" {
			c.outf("%s now stands in for %s on %s requests. `noskill train revert %d` stops it.\n", r.Tag, r.Base, orAllTasks(r.Task), n)
		} else {
			c.outf("%s is no longer used; requests go to %s again.\n", r.Tag, r.Base)
		}
		return ExitOK
	}
	return c.usageErr("unknown train command %q (export, lora, list, adopt, revert)", args[0])
}

func orAllTasks(t string) string {
	if t == "" {
		return "all"
	}
	return t
}

// parseAge reads 30d, 2w or a Go duration (72h).
func parseAge(s string) (time.Duration, error) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%q isn't a number of days or weeks", s)
			}
			return time.Duration(n) * unit, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q isn't an age like 30d or 72h", s)
	}
	return d, nil
}

type kv struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []kv {
	var out []kv
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].n > out[i].n || (out[j].n == out[i].n && out[j].k < out[i].k) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
