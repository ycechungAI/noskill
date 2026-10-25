package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/scheduler"
)

// cmdSchedule manages jobs (FR-J1). It only edits state.db; the daemon (`noskill serve`) picks the
// change up within a few seconds and is the only process that runs jobs.
func cmdSchedule(c *cmd, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	switch args[0] {
	case "add":
		return scheduleAdd(c, ctx, args[1:])
	case "list", "ls":
		return scheduleList(c, ctx)
	case "pause", "resume", "rm", "remove", "run":
		if len(args) != 2 {
			return c.usageErr("usage: noskill schedule %s <job>", args[0])
		}
		return scheduleOne(c, ctx, args[0], args[1])
	case "history":
		if len(args) != 2 {
			return c.usageErr("usage: noskill schedule history <job>")
		}
		return jobsHistory(c, ctx, args[1])
	}
	return c.usageErr("unknown schedule command %q (add, list, pause, resume, rm, run, history)", args[0])
}

func scheduleAdd(c *cmd, ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("schedule add", flag.ContinueOnError)
	cronExpr := fs.String("cron", "", `cron expression in local time, e.g. "0 3 * * *" (03:00 daily)`)
	every := fs.String("every", "", "interval, e.g. 30m, 6h, 1d")
	name := fs.String("name", "", "job name (default: the skill name, or one made from the prompt)")
	prompt := fs.String("prompt", "", "the task for a skill job (default: the skill's own task)")
	model := fs.String("model", "", "pin a model (default: route each run)")
	var allow, when multiFlag
	fs.Var(&allow, "allow", "grant file access, e.g. write:~/Downloads (repeatable)")
	fs.Var(&when, "when", "run only while this holds: volume:<label>, ac-power, unmetered, window:HH:MM-HH:MM (repeatable; FR-J5)")
	missed := fs.String("missed", "", "what to do after the machine was off or asleep at run time: once | skip (default: [scheduler] missed_policy)")
	overlap := fs.String("overlap", "skip", "when the previous run is still going: skip | queue")
	timeout := fs.String("timeout", "", "longest a run may take (default: [scheduler] default_timeout)")
	retries := fs.Int("retries", -1, "retries after a failure, with backoff (default: [scheduler] retries)")
	notifyTo := fs.String("notify", "os", "where to send results: os, ntfy, telegram, discord, email (comma-separated), or none")
	paused := fs.Bool("paused", false, "add it paused")
	pos, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return c.usageErr(`usage: noskill schedule add <skill|"prompt"> --cron "<expr>" | --every <dur> [--name n] [--prompt p] [--notify os,ntfy]`)
	}
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	spec := app.JobSpec{Target: pos[0], Cron: *cronExpr, Every: *every, Name: *name, Prompt: *prompt, Model: *model,
		Allow: allow, When: when, Missed: *missed, Overlap: *overlap, Timeout: *timeout, Paused: *paused}
	if *retries >= 0 {
		spec.Retries = retries
	}
	if *notifyTo != "none" {
		spec.Notify = splitComma(*notifyTo)
	}
	j, err := a.AddJob(ctx, spec)
	if err != nil {
		var ue *app.UserError
		if errors.As(err, &ue) {
			return c.usageErr("%v", err)
		}
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(j)
		return ExitOK
	}
	c.outf("✓ scheduled %s: %s · next %s\n", j.Name, describeSchedule(j.Schedule), when2(j.NextRunAt))
	if len(j.Notify) > 0 {
		c.outf("  results go to: %s\n", strings.Join(j.Notify, ", "))
	}
	if len(j.Preconditions) > 0 {
		c.outf("  runs only %s; otherwise it waits, and skips if the next run comes first\n", describeWhen(j.Preconditions))
	}
	for _, p := range j.Preconditions {
		label, ok := strings.CutPrefix(p, "volume:")
		if !ok {
			continue
		}
		// Bind the label to this drive now if it's attached (PLATFORMS.md "External drives").
		if v, err := a.Volumes().Mount(ctx, label, ""); err != nil {
			c.outf("  ! %v (the job waits for it)\n", err)
		} else if v.UUID != "" {
			c.outf("  ✓ %s mounted (%s, %s free) · bound to volume UUID\n", label, v.FSType, humanBytes(v.FreeBytes))
		}
	}
	c.daemonHint(ctx)
	return ExitOK
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// describeWhen says a job's preconditions in words: "while RED is mounted and on AC power".
func describeWhen(pre []string) string {
	var parts []string
	for _, p := range pre {
		kind, arg, _ := strings.Cut(p, ":")
		switch kind {
		case "volume":
			parts = append(parts, arg+" mounted")
		case "ac-power":
			parts = append(parts, "on AC power")
		case "unmetered":
			parts = append(parts, "unmetered network")
		case "window":
			parts = append(parts, "between "+strings.Replace(arg, "-", " and ", 1))
		default:
			parts = append(parts, p)
		}
	}
	return "if " + strings.Join(parts, " and ")
}

// humanBytes says a size in decimal units, like Finder and Explorer: "1.6 TB", "420 GB".
func humanBytes(b uint64) string {
	switch {
	case b >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(b)/1e12)
	case b >= 1e9:
		return fmt.Sprintf("%.0f GB", float64(b)/1e9)
	}
	return fmt.Sprintf("%.0f MB", float64(b)/1e6)
}

func describeSchedule(s string) string {
	kind, v, _ := strings.Cut(s, ":")
	if kind == "every" {
		return "every " + v
	}
	return "cron " + v
}

func when2(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	now := time.Now()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return "today " + t.Format("15:04")
	case t.Sub(now) < 7*24*time.Hour && t.After(now):
		return t.Format("Mon 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// daemonHint says whether anything will actually run the job.
func (c *cmd) daemonHint(ctx context.Context) {
	if c.g.json || waitForServer(ctx, c.loaded.Config.Server.Port, 1200*time.Millisecond) {
		return
	}
	c.outf("  Nothing runs jobs right now: start the background service with `noskill daemon install`\n  (or keep `noskill serve` open).\n")
}

func scheduleList(c *cmd, ctx context.Context) int {
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	st := &scheduler.Store{DB: a.DB}
	jobs, err := st.List(ctx)
	if err != nil {
		return c.fail(err)
	}
	last := map[string]scheduler.JobRun{}
	for _, j := range jobs {
		if runs, _ := st.Runs(ctx, j.ID, 1); len(runs) == 1 {
			last[j.ID] = runs[0]
		}
	}
	if c.g.json {
		type row struct {
			scheduler.Job
			Last *scheduler.JobRun `json:"last,omitempty"`
		}
		var rows []row
		for _, j := range jobs {
			r := row{Job: j}
			if l, ok := last[j.ID]; ok {
				r.Last = &l
			}
			rows = append(rows, r)
		}
		c.printJSON(map[string]any{"jobs": rows})
		return ExitOK
	}
	if len(jobs) == 0 {
		c.outf("no jobs yet: noskill schedule add <skill|\"prompt\"> --cron \"0 3 * * *\"\n")
		return ExitOK
	}
	rows := [][]string{{"NAME", "WHAT", "WHEN", "NEXT", "LAST"}}
	for _, j := range jobs {
		what := j.Target
		if j.TargetKind == "prompt" {
			what = `"` + truncateStr(j.Target, 40) + `"`
		}
		next := when2(j.NextRunAt)
		if !j.Enabled {
			next = "paused"
		} else if !j.RetryAt.IsZero() {
			next = "retry " + when2(j.RetryAt)
		}
		lastS := "-"
		if l, ok := last[j.ID]; ok {
			lastS = l.Status + " " + when2(l.ScheduledFor)
		}
		whenS := describeSchedule(j.Schedule)
		if len(j.Preconditions) > 0 {
			whenS += ", " + describeWhen(j.Preconditions)
		}
		rows = append(rows, []string{j.Name, what, whenS, next, lastS})
	}
	c.table(rows)
	c.daemonHint(ctx)
	return ExitOK
}

func scheduleOne(c *cmd, ctx context.Context, verb, name string) int {
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	st := &scheduler.Store{DB: a.DB}
	j, err := st.Get(ctx, name)
	if err != nil {
		return c.fail(err)
	}
	switch verb {
	case "pause":
		err = st.SetEnabled(ctx, j.ID, false, time.Time{})
		if err == nil {
			c.outf("✓ paused %s (a run already going carries on; `noskill jobs stop %s` stops it)\n", j.Name, j.Name)
		}
	case "resume":
		sched, perr := scheduler.ParseSchedule(j.Schedule)
		if perr != nil {
			return c.fail(perr)
		}
		next := sched.Next(time.Now())
		err = st.SetEnabled(ctx, j.ID, true, next)
		if err == nil {
			c.outf("✓ resumed %s · next %s\n", j.Name, when2(next))
		}
	case "rm", "remove":
		err = st.Remove(ctx, j.ID)
		if err == nil {
			c.outf("✓ removed %s (its runs and traces stay in `noskill traces`)\n", j.Name)
		}
	case "run":
		if !j.Enabled {
			return c.fail(fmt.Errorf("%s is paused; `noskill schedule resume %s` first", j.Name, j.Name))
		}
		err = st.RunNow(ctx, j.ID, time.Now())
		if err == nil {
			c.outf("✓ %s will start within a few seconds; follow it with `noskill jobs`\n", j.Name)
			c.daemonHint(ctx)
		}
	}
	if err != nil {
		return c.fail(err)
	}
	return ExitOK
}

// cmdJobs shows running and waiting jobs, and stops them (the kill switch, SECURITY.md T6).
func cmdJobs(c *cmd, args []string) int {
	ctx, stop := c.env.Context()
	defer stop()
	if len(args) > 0 && args[0] == "stop" {
		fs := flag.NewFlagSet("jobs stop", flag.ContinueOnError)
		all := fs.Bool("all", false, "stop every running and waiting job")
		pos, err := c.parse(fs, args[1:])
		if err != nil {
			return ExitUsage
		}
		target := ""
		switch {
		case *all && len(pos) == 0:
		case !*all && len(pos) == 1:
			target = pos[0]
		default:
			return c.usageErr("usage: noskill jobs stop <job|run-id> | --all")
		}
		a, err := c.open(ctx)
		if err != nil {
			return c.fail(err)
		}
		defer a.Close()
		n, err := (&scheduler.Store{DB: a.DB}).RequestStop(ctx, target, time.Now())
		if err != nil {
			return c.fail(err)
		}
		if n == 0 {
			c.outf("nothing to stop\n")
			return ExitOK
		}
		c.outf("✓ stopping %d run(s); running ones stop within a few seconds\n", n)
		return ExitOK
	}
	if len(args) > 0 && args[0] == "history" {
		job := ""
		if len(args) > 1 {
			job = args[1]
		}
		return jobsHistory(c, ctx, job)
	}
	if len(args) > 0 {
		return c.usageErr("usage: noskill jobs [stop <job|--all>] | jobs history [job]")
	}
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	active, err := (&scheduler.Store{DB: a.DB}).ActiveRuns(ctx)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(map[string]any{"active": active})
		return ExitOK
	}
	if len(active) == 0 {
		c.outf("no jobs running (`noskill jobs history` shows past runs)\n")
		return ExitOK
	}
	rows := [][]string{{"JOB", "STATUS", "SINCE", "RUN", "APPROVAL"}}
	for _, r := range active {
		rows = append(rows, []string{r.JobName, r.Status, when2(r.StartedAt), shortID(r.RunID), shortID(r.ApprovalID)})
	}
	c.table(rows)
	return ExitOK
}

func jobsHistory(c *cmd, ctx context.Context, job string) int {
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	st := &scheduler.Store{DB: a.DB}
	id := ""
	if job != "" {
		j, err := st.Get(ctx, job)
		if err != nil {
			return c.fail(err)
		}
		id = j.ID
	}
	runs, err := st.Runs(ctx, id, 30)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(map[string]any{"runs": runs})
		return ExitOK
	}
	if len(runs) == 0 {
		c.outf("no runs yet\n")
		return ExitOK
	}
	rows := [][]string{{"JOB", "FOR", "STATUS", "RUN", "RESULT"}}
	for _, r := range runs {
		res := r.Result
		if r.Error != "" && r.Status != scheduler.StatusDone {
			res = r.Error
		}
		if r.Status == scheduler.StatusWaiting {
			res = "waiting for approval " + shortID(r.ApprovalID)
		}
		rows = append(rows, []string{r.JobName, when2(r.ScheduledFor), r.Status, shortID(r.RunID), truncateStr(res, 60)})
	}
	c.table(rows)
	return ExitOK
}
