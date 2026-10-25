package cli

import (
	"errors"
	"flag"
	"strings"

	"noskill/internal/app"
)

// noskill remind and noskill brief: the daily-brief skill from the terminal, with no model. The
// logic is in internal/chores; reminders are scheduled jobs (FR-J1) the daemon fires.

func cmdRemind(c *cmd, args []string) int {
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	if len(args) == 0 {
		return c.usageErr(`usage: noskill remind "<what and when>" | list | rm <name>   e.g. noskill remind "call Sam tomorrow at 5pm"`)
	}
	switch args[0] {
	case "list", "ls":
		jobs, err := a.Reminders(ctx)
		if err != nil {
			return c.fail(err)
		}
		if len(jobs) == 0 {
			c.outf("no reminders yet: noskill remind \"call Sam tomorrow at 5pm\"\n")
			return ExitOK
		}
		rows := [][]string{{"NAME", "REMINDER", "WHEN", "NEXT"}}
		for _, j := range jobs {
			next := when2(j.NextRunAt)
			if !j.Enabled {
				next = "done"
			}
			rows = append(rows, []string{j.Name, j.Target, describeSchedule(j.Schedule), next})
		}
		c.table(rows)
		return ExitOK
	case "rm", "remove":
		if len(args) != 2 {
			return c.usageErr("usage: noskill remind rm <name>")
		}
		return scheduleOne(c, ctx, "rm", args[1])
	}
	j, r, err := a.AddReminder(ctx, strings.Join(args, " "))
	if err != nil {
		var ue *app.UserError
		if errors.As(err, &ue) {
			return c.usageErr("%v", err)
		}
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(map[string]any{"job": j, "reminder": r})
		return ExitOK
	}
	c.outf("✓ %q: %s · next %s\n", r.Message, r.When, when2(j.NextRunAt))
	c.daemonHint(ctx)
	return ExitOK
}

func cmdBrief(c *cmd, args []string) int {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	hours := fs.Int("hours", 24, "how far back to look")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	text := a.Brief(ctx, *hours)
	if c.g.json {
		c.printJSON(map[string]any{"hours": *hours, "brief": text})
		return ExitOK
	}
	c.outf("%s\n", text)
	return ExitOK
}
