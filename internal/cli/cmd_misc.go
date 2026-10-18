package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/approvals"
	"noskill/internal/memory"
)

// ---- rate ----

func cmdRate(c *cmd, args []string) int {
	fs := flag.NewFlagSet("rate", flag.ContinueOnError)
	pos, err := c.parse(fs, args)
	if err != nil || len(pos) < 2 {
		return c.usageErr("usage: noskill rate <run-id|last> good|partial|bad [\"note\"]")
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	run, err := a.Rate(ctx, pos[0], pos[1], strings.Join(pos[2:], " "))
	if err != nil {
		return c.fail(err)
	}
	c.outf("run %s: %s (you)\n", run.ID, run.Outcome)
	return ExitOK
}

// ---- undo ----

func cmdUndo(c *cmd, args []string) int {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show what would be undone")
	pos, err := c.parse(fs, args)
	if err != nil || len(pos) != 1 {
		return c.usageErr("usage: noskill undo <run-id|last> [--dry-run]")
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	id := pos[0]
	if id == "last" {
		run, err := a.DB.LatestRun(ctx, "")
		if err != nil {
			return c.fail(errors.New("no runs yet"))
		}
		id = run.ID
	}
	steps, err := a.Undo(ctx, id, *dry)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(map[string]any{"run_id": id, "steps": steps, "dry_run": *dry})
	} else {
		if len(steps) == 0 {
			c.outf("nothing to undo for run %s\n", id)
		}
		for _, s := range steps {
			mark := "✓"
			switch {
			case s.Note != "":
				mark = "!"
			case *dry:
				mark = "·"
			}
			c.outf("%s %s", mark, s.Action)
			if s.Note != "" {
				c.outf(" — %s", s.Note)
			}
			c.outf("\n")
		}
	}
	for _, s := range steps {
		if s.Note != "" {
			return ExitError
		}
	}
	return ExitOK
}

// ---- approvals ----

func cmdApprovals(c *cmd, args []string) int {
	ctx, stop := c.env.Context()
	defer stop()
	if len(args) > 0 && (args[0] == "approve" || args[0] == "skip") {
		return approvalsAnswer(c, ctx, args[0] == "approve", args[1:])
	}
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	all := fs.Bool("all", false, "include answered and expired items")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	items, err := a.Approvals.List(ctx, *all, 50)
	if err != nil {
		return c.fail(err)
	}
	for i := range items {
		items[i].ResumeToken = ""
	}
	if c.g.json {
		c.printJSON(map[string]any{"approvals": items})
		return ExitOK
	}
	if len(items) == 0 {
		c.outf("nothing is waiting for you\n")
		return ExitOK
	}
	rows := [][]string{{"ID", "SOURCE", "WHAT", "STATUS", "EXPIRES"}}
	for _, it := range items {
		exp := "in " + humanDur(time.Until(it.ExpiresAt))
		if it.Status != approvals.Pending {
			exp = "-"
		}
		rows = append(rows, []string{shortID(it.ID), it.Source, truncateStr(it.Summary, 70), string(it.Status), exp})
	}
	c.table(rows)
	return ExitOK
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

func truncateStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func humanDur(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// resolveApproval finds an item by full id, or by the short id `approvals` prints (the end of the id).
func resolveApproval(ctx context.Context, q *approvals.Queue, want string) (approvals.Item, error) {
	items, err := q.List(ctx, true, 500)
	if err != nil {
		return approvals.Item{}, err
	}
	var hits []approvals.Item
	for _, it := range items {
		if it.ID == want {
			return it, nil
		}
		if strings.HasSuffix(it.ID, want) || strings.HasPrefix(it.ID, want) {
			hits = append(hits, it)
		}
	}
	switch len(hits) {
	case 0:
		return approvals.Item{}, fmt.Errorf("no approval %q (see `noskill approvals`)", want)
	case 1:
		return hits[0], nil
	}
	return approvals.Item{}, fmt.Errorf("%q matches %d approvals; use more of the id", want, len(hits))
}

func approvalsAnswer(c *cmd, ctx context.Context, approve bool, args []string) int {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	var set multiFlag
	fs.Var(&set, "set", "change an editable field: --set name=value (repeatable)")
	every := fs.Bool("all", false, "answer every pending item")
	pos, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	fields := map[string]string{}
	for _, kv := range set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return c.usageErr("--set wants name=value, got %q", kv)
		}
		fields[k] = v
	}
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	var targets []approvals.Item
	var note []string
	switch {
	case *every:
		if targets, err = a.Approvals.List(ctx, false, 0); err != nil {
			return c.fail(err)
		}
		note = pos
		if approve && len(fields) > 0 {
			return c.usageErr("--set applies to one item; name it instead of using --all")
		}
	case len(pos) >= 1:
		it, err := resolveApproval(ctx, a.Approvals, pos[0])
		if err != nil {
			return c.fail(err)
		}
		targets, note = []approvals.Item{it}, pos[1:]
	default:
		return c.usageErr("usage: noskill approvals approve|skip <id|--all> [--set name=value] [\"note\"]")
	}
	code := ExitOK
	for _, it := range targets {
		// A confirmation shows what it's about before it's answered.
		done, err := a.Approvals.Resolve(ctx, it.ID, approvals.Answer{Approve: approve, Set: fields, Note: strings.Join(note, " "), Via: "cli"})
		if err != nil {
			c.errf("noskill: %s: %v\n", shortID(it.ID), err)
			code = ExitError
			continue
		}
		verb := map[bool]string{true: "approved", false: "skipped"}[approve]
		c.outf("✓ %s %s: %s\n", verb, shortID(done.ID), done.Summary)
		switch {
		case !approve || done.RunID == "":
		case done.Source == "job":
			c.outf("  The job carries on from where it stopped within a few seconds (`noskill jobs`).\n")
			c.daemonHint(ctx)
		case done.Source == "skill" && app.Resumable(done):
			c.outf("  Carry on with `noskill run --resume %s`.\n", shortID(done.ID))
		case done.Source == "skill":
			c.outf("  Run the task again; this call will go through without asking.\n")
		}
		if approve {
			vals := done.FieldValues()
			for _, k := range sortedKeys(vals) {
				c.outf("  %s = %s\n", k, vals[k])
			}
		}
	}
	return code
}

// ---- memory ----

func cmdMemory(c *cmd, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "list":
		items, err := a.Memory.List(ctx)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"memory": items})
			return ExitOK
		}
		if len(items) == 0 {
			c.outf("nothing remembered yet. Add something with `noskill memory add <key> \"<fact>\"`,\nor tell noskill \"remember that …\" in a chat.\n")
			return ExitOK
		}
		rows := [][]string{{"KEY", "FACT", "SOURCE"}}
		for _, it := range items {
			rows = append(rows, []string{it.Key, truncateStr(it.Value, 80), it.Source})
		}
		c.table(rows)
	case "add":
		if len(args) < 3 {
			return c.usageErr("usage: noskill memory add <key> \"<fact>\"")
		}
		it, err := a.Memory.Add(ctx, args[1], strings.Join(args[2:], " "), "explicit", 1)
		if errors.Is(err, memory.ErrLooksSecret) {
			return c.usageErr("%v", err)
		}
		if err != nil {
			return c.fail(err)
		}
		c.outf("remembered %s\n", it.Key)
	case "forget":
		if len(args) != 2 {
			return c.usageErr("usage: noskill memory forget <key|id|--all>")
		}
		var n int
		if args[1] == "--all" {
			if !c.ask("Forget everything noskill remembers about you?", false) {
				c.errf("nothing forgotten (run in a terminal and answer y to confirm)\n")
				return ExitError
			}
			n, err = a.Memory.ForgetAll(ctx)
		} else {
			n, err = a.Memory.Forget(ctx, args[1])
		}
		if err != nil {
			return c.fail(err)
		}
		if n == 0 {
			return c.fail(fmt.Errorf("nothing remembered under %q", args[1]))
		}
		c.outf("forgot %d item(s)\n", n)
	default:
		return c.usageErr("unknown memory command %q (list, add, forget)", args[0])
	}
	return ExitOK
}

// ---- traces ----

func cmdTraces(c *cmd, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "list":
		runs, err := a.DB.ListRuns(ctx, 20)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"runs": runs})
			return ExitOK
		}
		rows := [][]string{{"RUN", "WHEN", "STATUS", "MODEL", "STEPS", "OUTCOME", "SOURCE"}}
		for _, r := range runs {
			out := r.Outcome
			if out == "" {
				out = "-"
			} else {
				out += " (" + r.OutcomeSource + ")"
			}
			rows = append(rows, []string{shortID(r.ID), r.StartedAt.Format("Jan 2 15:04"), r.Status, r.DriverModel, fmt.Sprint(r.Steps), out, r.Source})
		}
		c.table(rows)
	case "show":
		if len(args) != 2 {
			return c.usageErr("usage: noskill traces show <run-id|last>")
		}
		return showTrace(c, ctx, a, args[1])
	case "purge":
		return purgeTraces(c, ctx, a, args[1:])
	default:
		return c.usageErr("unknown traces command %q (list, show, purge)", args[0])
	}
	return ExitOK
}

func purgeTraces(c *cmd, ctx context.Context, a interface {
	PurgeTraces(context.Context, time.Time) (int, error)
}, args []string) int {
	fs := flag.NewFlagSet("traces purge", flag.ContinueOnError)
	before := fs.String("before", "", "move traces older than this date (YYYY-MM-DD) to the trash")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	if *before == "" {
		return c.usageErr("usage: noskill traces purge --before YYYY-MM-DD")
	}
	t, err := time.ParseInLocation("2006-01-02", *before, time.Local)
	if err != nil {
		return c.usageErr("--before wants a date like 2026-09-01")
	}
	n, err := a.PurgeTraces(ctx, t)
	if err != nil {
		return c.fail(err)
	}
	c.outf("moved %d trace file(s) to the trash\n", n)
	return ExitOK
}

var _ = sort.Strings
var _ = os.Stat
