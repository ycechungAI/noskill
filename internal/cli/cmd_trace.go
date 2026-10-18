package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"noskill/internal/agent"
	"noskill/internal/app"
)

// showTrace prints a run's trace as a story: what was asked, how it was routed, each step, each
// tool call and result, and the answer.
func showTrace(c *cmd, ctx context.Context, a *app.App, id string) int {
	var run struct {
		ID, Path string
	}
	if id == "last" {
		r, err := a.DB.LatestRun(ctx, "")
		if err != nil {
			return c.fail(fmt.Errorf("no runs yet"))
		}
		run.ID, run.Path = r.ID, r.TracePath
	} else {
		items, _ := a.DB.ListRuns(ctx, 500)
		for _, r := range items {
			if r.ID == id || strings.HasSuffix(r.ID, id) {
				run.ID, run.Path = r.ID, r.TracePath
			}
		}
		if run.ID == "" {
			if r, err := a.DB.GetRun(ctx, id); err == nil {
				run.ID, run.Path = r.ID, r.TracePath
			}
		}
	}
	if run.ID == "" {
		return c.fail(fmt.Errorf("no run %q (see `noskill traces list`)", id))
	}
	if run.Path == "" {
		return c.fail(fmt.Errorf("run %s has no trace (it used --no-learn, or the trace was purged)", run.ID))
	}
	f, err := os.Open(run.Path)
	if err != nil {
		return c.fail(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		if c.g.json {
			c.outf("%s\n", sc.Text())
			continue
		}
		var ev agent.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Kind {
		case agent.EventRoute:
			c.outf("request: %s\n%s", ev.Content, indent(ev.Note, "  "))
		case agent.EventSelect:
			c.outf("skills: %s\n", orDefault(ev.Note, "none"))
		case agent.EventStep:
			c.outf("\nstep %d  %s  (%s%s)\n", ev.Step, ev.Model, ev.Note, map[bool]string{true: ", final", false: ""}[ev.Final])
		case agent.EventToolCall:
			c.outf("  → %s %s\n", ev.Tool, truncateStr(string(ev.Args), 200))
		case agent.EventToolResult:
			if ev.Err != "" {
				c.outf("  ← error: %s\n", truncateStr(ev.Err, 300))
			} else {
				c.outf("  ← %s  (%d bytes, %d ms)\n", truncateStr(strings.ReplaceAll(ev.Content, "\n", " "), 300), ev.Bytes, ev.Ms)
			}
		case agent.EventToolDenied, agent.EventRepeat, agent.EventRetry, agent.EventBudget, agent.EventEvicted:
			c.outf("  ! %s %s\n", ev.Kind, ev.Note)
		case agent.EventAnswer:
			c.outf("\nanswer:\n%s\n", indent(ev.Content, "  "))
		case agent.EventOutcome:
			c.outf("\noutcome: %s (%s) %s\n", ev.Content, ev.Tool, ev.Note)
		case agent.EventEnd:
			c.outf("\nend: %s · %d tokens · %d ms\n", ev.Note, ev.Tokens, ev.Ms)
		}
	}
	return ExitOK
}

func indent(s, p string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = p + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
