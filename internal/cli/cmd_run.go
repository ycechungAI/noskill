package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"noskill/internal/agent"
	"noskill/internal/app"
	"noskill/internal/approvals"
	"noskill/internal/backends"
)

// runOptions are the flags `run` and `chat` share.
type runOptions struct {
	model, preset  string
	skills, allow  multiFlag
	noLearn, think bool
	depth          string
	steps          int
}

func (o *runOptions) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.model, "model", "", "pin a model")
	fs.StringVar(&o.preset, "preset", "", "fast | balanced | best")
	fs.Var(&o.skills, "skill", "force a skill (repeatable)")
	fs.Var(&o.allow, "allow", "grant file access for this run, e.g. write:~/Downloads (repeatable)")
	fs.BoolVar(&o.noLearn, "no-learn", false, "keep no trace, memory or outcome signals")
	fs.BoolVar(&o.think, "think", false, "let the model think before answering")
	fs.StringVar(&o.depth, "depth", "", "quick | normal | deep")
	fs.IntVar(&o.steps, "budget-steps", 0, "most tool steps")
}

func (o runOptions) request(msgs []backends.Message) (app.RunRequest, error) {
	rr := app.RunRequest{Messages: msgs, Source: "cli", Model: o.model, Preset: o.preset, Skills: o.skills, Allow: o.allow, NoLearn: o.noLearn}
	switch o.preset {
	case "", "fast", "balanced", "best":
	default:
		return rr, fmt.Errorf("--preset must be fast, balanced or best")
	}
	switch o.depth {
	case "", "normal":
	case "quick":
		rr.MaxSteps = 6
	case "deep":
		rr.MaxSteps, rr.MaxTokens = 40, 150000
	default:
		return rr, fmt.Errorf("--depth must be quick, normal or deep")
	}
	if o.steps > 0 {
		rr.MaxSteps = o.steps
	}
	if o.think {
		t := true
		rr.Think = &t
	}
	return rr, nil
}

func cmdRun(c *cmd, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o runOptions
	o.bind(fs)
	out := fs.String("out", "", "write the answer to this new file")
	resume := fs.String("resume", "", "continue a run that stopped for an approval you've now given (its approval id)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	if *resume != "" {
		if len(pos) > 0 {
			return c.usageErr("--resume continues an earlier run; give no task with it")
		}
		return runResume(c, *resume)
	}
	task := strings.TrimSpace(strings.Join(pos, " "))
	if task == "" || task == "-" {
		if c.env.StdinTTY && task == "" {
			return c.usageErr("usage: noskill run \"<task>\" [--model m] [--skill s] [--allow write:~/Downloads] [--out file]")
		}
		b, err := io.ReadAll(io.LimitReader(c.in, 1<<20))
		if err != nil {
			return c.fail(err)
		}
		task = strings.TrimSpace(string(b))
	}
	if task == "" {
		return c.usageErr("no task given")
	}
	if *out != "" {
		if _, err := os.Stat(*out); err == nil {
			return c.usageErr("%s already exists; noskill doesn't overwrite files. Pick another name.", *out)
		}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	rr, err := o.request([]backends.Message{{Role: backends.RoleUser, Content: task}})
	if err != nil {
		return c.usageErr("%v", err)
	}
	res, err := c.execute(ctx, a, rr)
	if res != nil && *out != "" && res.Answer != "" {
		if werr := writeNew(*out, res.Answer+"\n"); werr != nil {
			c.errf("noskill: couldn't write %s: %v\n", *out, werr)
		}
	}
	return c.finishRun(res, err)
}

// runResume continues a run parked on an approval (FR-A6) from the call it stopped on.
func runResume(c *cmd, id string) int {
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	it, err := resolveApproval(ctx, a.Approvals, id)
	if err != nil {
		return c.fail(err)
	}
	if it.Source == "job" {
		return c.fail(fmt.Errorf("that run belongs to a scheduled job; the daemon continues it once approved (see `noskill jobs`)"))
	}
	rr, err := a.ResumeRequest(ctx, it.ID, app.RunRequest{})
	if err != nil {
		return c.fail(err)
	}
	res, err := c.execute(ctx, a, rr)
	return c.finishRun(res, err)
}

func writeNew(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// execute runs one request, streaming the answer to stdout and progress to stderr.
func (c *cmd) execute(ctx context.Context, a *app.App, rr app.RunRequest) (*app.RunResult, error) {
	view := &termView{c: c}
	rr.OnEvent = view.onEvent
	if c.env.StdinTTY {
		rr.Prompt = c.promptApproval
	} else {
		rr.Headless = true // nobody to ask: a confirm tool parks the run (exit code 3)
	}
	res, err := a.Run(ctx, rr)
	view.end()
	return res, err
}

// promptApproval asks the person at the terminal about a confirm tool or a learned skill.
func (c *cmd) promptApproval(_ context.Context, it approvals.Item) (bool, error) {
	var d struct {
		Tool        string          `json:"tool"`
		Args        json.RawMessage `json:"args"`
		Description string          `json:"description"`
	}
	json.Unmarshal(it.Details, &d)
	if it.Kind == "learned_skill" { // a learned skill's first use (FR-L4)
		c.errf("\n%s\n  %s\n", it.Summary, d.Description)
		return c.ask("Use it?", false), nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, d.Args, "  ", "  ") != nil {
		pretty.Reset()
		pretty.Write(d.Args)
	}
	args := pretty.String()
	if r := []rune(args); len(r) > 1500 {
		args = string(r[:1500]) + "\n  …"
	}
	c.errf("\n%s\n  %s\n", it.Summary, args)
	return c.ask("Allow this?", false), nil
}

// termView prints a run as it happens.
type termView struct {
	c        *cmd
	mu       sync.Mutex
	final    bool
	streamed bool
	any      bool
	lastNL   bool
}

func (v *termView) write(s string) {
	if s == "" || v.c.g.json {
		return
	}
	io.WriteString(v.c.env.Stdout, s)
	v.any, v.lastNL = true, strings.HasSuffix(s, "\n")
}

func (v *termView) onEvent(ev agent.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch ev.Kind {
	case agent.EventStep:
		v.final, v.streamed = ev.Final, false
	case agent.EventDelta:
		if v.final && ev.Content != "" {
			v.write(ev.Content)
			v.streamed = true
		}
		if v.final && ev.Thinking != "" && v.c.g.verbose {
			fmt.Fprint(v.c.env.Stderr, ev.Thinking)
		}
	case agent.EventAnswer:
		if !v.streamed {
			v.write(ev.Content)
		}
	case agent.EventProgress:
		v.c.infof("→ %s\n", ev.Note)
	case agent.EventToolCall:
		v.c.infof("→ %s\n", toolLine(ev))
	case agent.EventToolDenied:
		v.c.infof("→ skipped %s: not available in this run\n", ev.Tool)
	case agent.EventRepeat:
		v.c.infof("→ %s was called twice with the same arguments; answering with what I have\n", ev.Tool)
	case agent.EventBudget:
		v.c.infof("→ %s\n", ev.Note)
	case agent.EventEvicted:
		if v.c.g.verbose {
			v.c.infof("→ %s\n", ev.Note)
		}
	}
}

func (v *termView) end() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.any && !v.lastNL {
		io.WriteString(v.c.env.Stdout, "\n")
	}
}

// toolLine is the one-line progress text for a tool call.
func toolLine(ev agent.Event) string {
	var args map[string]any
	json.Unmarshal(ev.Args, &args)
	detail := ""
	for _, k := range []string{"query", "url", "path", "name", "task", "goal", "from"} {
		if s, ok := args[k].(string); ok && s != "" {
			detail = s
			break
		}
	}
	if r := []rune(detail); len(r) > 80 {
		detail = string(r[:80]) + "…"
	}
	if detail != "" {
		return ev.Tool + ": " + detail
	}
	return ev.Tool + "…"
}

// finishRun prints the summary and picks the exit code.
func (c *cmd) finishRun(res *app.RunResult, err error) int {
	if c.g.json {
		out := map[string]any{"ok": err == nil}
		if res != nil {
			out["run_id"], out["answer"], out["model"], out["skills"] = res.RunID, res.Answer, res.Model, res.Skills
			out["stop"], out["steps"], out["tokens"], out["trace"] = res.Stop, res.Steps, res.Usage.Total(), res.TracePath
			if res.WaitingFor != "" {
				out["waiting_for_approval"] = res.WaitingFor
			}
		}
		if err != nil {
			out["error"] = err.Error()
		}
		c.printJSON(out)
	}
	if err != nil && !(res != nil && res.Stop == agent.StopCancelled && res.Answer != "") {
		if errors.Is(err, context.Canceled) {
			c.errf("\nnoskill: cancelled\n")
			return 130
		}
		return c.fail(err)
	}
	if res == nil {
		return ExitError
	}
	if !c.g.json {
		c.infof("(%s · %d step(s) · %d tokens · run %s)\n", res.Model, res.Steps, res.Usage.Total(), res.RunID)
	}
	switch res.Stop {
	case agent.StopWaiting:
		c.errf("noskill: waiting for your approval (%s). Run `noskill approvals approve %s`, then `noskill run --resume %s` to carry on.\n", res.WaitingFor, shortID(res.WaitingFor), shortID(res.WaitingFor))
		return ExitWaiting
	case agent.StopMaxSteps, agent.StopMaxTokens:
		if strings.TrimSpace(res.Answer) == "" && res.Stop == agent.StopMaxTokens {
			c.errf("noskill: %s thought until its output limit and gave no answer. Try again, or pin a model that doesn't always think with --model.\n", res.Model)
			return ExitBudget
		}
		c.errf("noskill: stopped at the %s limit; the answer above is partial. Raise it with --depth deep or --budget-steps.\n", strings.TrimPrefix(string(res.Stop), "max_"))
		return ExitBudget
	}
	return ExitOK
}
