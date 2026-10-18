package cli

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// cmdLearn is `noskill learn <run-id|last>` ("remember how to do this", FR-L2) and
// `noskill learn queue`.
func cmdLearn(c *cmd, args []string) int {
	fs := flag.NewFlagSet("learn", flag.ContinueOnError)
	later := fs.Bool("later", false, "only queue it; the background worker drafts it when noskill is idle")
	pos, err := c.parse(fs, args)
	if err != nil || len(pos) != 1 {
		return c.usageErr("usage: noskill learn <run-id|last> [--later]\n       noskill learn queue")
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()

	if pos[0] == "queue" {
		items, err := a.DB.ListLearning(ctx, 30)
		if err != nil {
			return c.fail(err)
		}
		why := a.LearningBlocked(ctx)
		if c.g.json {
			c.printJSON(map[string]any{"items": items, "blocked": why})
			return ExitOK
		}
		if why == "" {
			c.infof("background learning: can run now\n")
		} else {
			c.infof("background learning: waiting (%s)\n", why)
		}
		if len(items) == 0 {
			c.outf("nothing queued\n")
			return ExitOK
		}
		t := [][]string{{"ID", "KIND", "WHY", "STATUS", "RUN/SKILL", "RESULT", "QUEUED"}}
		for _, it := range items {
			target := it.Skill
			if target == "" {
				target = strings.Join(it.RunIDs, ",")
			}
			res := it.Result
			if it.LastError != "" {
				res = strings.TrimSpace(res + " " + it.LastError)
			}
			t = append(t, []string{it.ID, it.Kind, it.Reason, it.Status, target, clipCell(res, 60), it.CreatedAt.Format("01-02 15:04")})
		}
		c.table(t)
		return ExitOK
	}

	id := pos[0]
	if id == "last" {
		run, err := a.DB.LatestRun(ctx, "")
		if err != nil {
			return c.usageErr("no runs yet")
		}
		id = run.ID
	}
	it, err := a.Learn(ctx, id)
	if err != nil {
		return c.fail(err)
	}
	if *later || it.Status == "running" {
		c.outf("queued %s: run %s will be drafted into a skill when noskill is idle (`noskill learn queue`)\n", it.ID, id)
		return ExitOK
	}
	c.infof("drafting a skill from run %s; this takes a minute or two on a small model…\n", id)
	start := time.Now()
	it, err = a.LearnNow(ctx, it.ID)
	if c.g.json {
		c.printJSON(map[string]any{"item": it, "error": errString(err)})
		if err != nil {
			return ExitError
		}
		return ExitOK
	}
	if err != nil {
		return c.fail(err)
	}
	switch it.Status {
	case "done":
		c.outf("%s (%s)\n", it.Result, time.Since(start).Round(time.Second))
	case "pending":
		c.outf("interrupted; %s is back in the queue\n", it.ID)
	default:
		c.outf("%s: %s %s\n", it.Status, it.Result, it.LastError)
	}
	return ExitOK
}

func clipCell(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
