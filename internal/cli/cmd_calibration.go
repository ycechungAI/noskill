package cli

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// cmdCalibration reports how well decision probabilities match outcomes, and tunes or reverts a
// decision's threshold (FR-B6c, MODEL_ADAPTERS.md "Calibration").
func cmdCalibration(c *cmd, args []string) int {
	usage := "usage: noskill calibration [show] | tune <decision> <model> [--apply] | revert <decision> <model>"
	if len(args) == 0 {
		args = []string{"show"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "show":
		rows, err := a.Calibration(ctx)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(rows)
			return ExitOK
		}
		if len(rows) == 0 {
			c.outf("No decisions recorded in the last 90 days.\n")
			return ExitOK
		}
		auto := "off: thresholds change only with `noskill calibration tune … --apply`"
		if a.Cfg.Orchestrator.AutoTune {
			auto = "on: thresholds move at most once a day, by up to 0.10"
		}
		c.outf("orchestrator.auto_tune is %s\n\n", auto)
		table := [][]string{{"DECISION", "MODEL", "MADE", "LABELLED", "ECE", "BRIER", "THRESHOLD", "SUGGESTED"}}
		for _, r := range rows {
			model := r.Model
			if r.Emulated {
				model += " (emulated)"
			}
			ece, brier, sug := "-", "-", "keep"
			if r.Labelled > 0 {
				ece, brier = fmt.Sprintf("%.2f", r.Calibration.ECE), fmt.Sprintf("%.2f", r.Calibration.Brier)
			}
			if r.Suggestion.Changed {
				sug = fmt.Sprintf("%.2f", r.Suggestion.Threshold)
			}
			th := fmt.Sprintf("%.2f", r.Threshold)
			if r.Threshold != r.Default {
				th += fmt.Sprintf(" (default %.2f)", r.Default)
			}
			table = append(table, []string{r.Name, model, fmt.Sprint(r.Decisions), fmt.Sprint(r.Labelled), ece, brier, th, sug})
		}
		c.table(table)
		for _, r := range rows {
			c.outf("\n%s, %s: %s\n", r.Name, r.Model, r.Suggestion.Why)
			if len(r.Calibration.Bins) > 0 {
				var bins []string
				for _, b := range r.Calibration.Bins {
					bins = append(bins, fmt.Sprintf("%.1f–%.1f: said %.2f, right %.2f (%d)", b.Lo, b.Hi, b.MeanP, b.Rate, b.N))
				}
				c.outf("  %s\n", strings.Join(bins, "\n  "))
			}
			for _, t := range r.Changes {
				c.outf("  %s  %s %.2f → %.2f  %s\n", t.At.Format(time.DateTime), t.Source, t.Previous, t.Threshold, t.Why)
			}
		}
		return ExitOK
	case "tune":
		fs := flag.NewFlagSet("calibration tune", flag.ContinueOnError)
		apply := fs.Bool("apply", false, "store the suggested threshold (otherwise only show it)")
		rest, err := c.parse(fs, args[1:])
		if err != nil {
			return ExitUsage
		}
		if len(rest) != 2 {
			return c.usageErr(usage)
		}
		s, err := a.TuneThreshold(ctx, rest[0], rest[1], *apply, "manual")
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(s)
			return ExitOK
		}
		switch {
		case !s.Changed:
			c.outf("%s stays at %.2f for %s: %s\n", rest[0], s.Current, rest[1], s.Why)
		case *apply:
			c.outf("✓ %s for %s: %.2f → %.2f (%s)\n", rest[0], rest[1], s.Current, s.Threshold, s.Why)
		default:
			c.outf("%s for %s would move %.2f → %.2f (%s). Add --apply to store it.\n", rest[0], rest[1], s.Current, s.Threshold, s.Why)
		}
		return ExitOK
	case "revert":
		if len(args) != 3 {
			return c.usageErr(usage)
		}
		t, err := a.RevertThreshold(ctx, args[1], args[2])
		if err != nil {
			return c.fail(err)
		}
		c.outf("✓ %s for %s: %.2f → %.2f\n", args[1], args[2], t.Previous, t.Threshold)
		return ExitOK
	}
	return c.usageErr(usage)
}
