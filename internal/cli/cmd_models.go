package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/backends"
	"noskill/internal/decide"
)

func cmdModels(c *cmd, args []string) int {
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
	rest := args[1:]
	switch args[0] {
	case "list":
		return modelsList(c, ctx, a)
	case "probe":
		return modelsProbe(c, ctx, a, rest)
	case "bench":
		return modelsBench(c, ctx, a, rest)
	case "pull":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill models pull <name>")
		}
		if err := pullWithProgress(c, ctx, a, rest[0]); err != nil {
			return c.fail(err)
		}
		c.outf("✓ %s\n", rest[0])
	case "rm":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill models rm <name>")
		}
		if !c.ask(fmt.Sprintf("Remove %s? You'd have to download it again.", rest[0]), false) {
			c.errf("nothing removed (answer y in a terminal to confirm)\n")
			return ExitError
		}
		if err := a.RemoveModel(ctx, rest[0]); err != nil {
			return c.fail(err)
		}
		c.outf("✓ removed %s\n", rest[0])
	default:
		return c.usageErr("unknown models command %q (list, probe, bench, pull, rm)", args[0])
	}
	return ExitOK
}

func pullWithProgress(c *cmd, ctx context.Context, a *app.App, name string) error {
	last := time.Time{}
	return a.Pull(ctx, name, func(s string) {
		if time.Since(last) > 700*time.Millisecond || strings.HasPrefix(s, "success") || !strings.Contains(s, "%") {
			c.infof("  %s\n", s)
			last = time.Now()
		}
	})
}

func modelsList(c *cmd, ctx context.Context, a *app.App) int {
	ms, errs := a.Models(ctx)
	for _, e := range errs {
		c.errf("noskill: %v\n", e)
	}
	if len(ms) == 0 && len(errs) > 0 {
		return c.fail(&app.NoBackendError{Errs: errs})
	}
	cfg := a.Cfg.Orchestrator
	type row struct {
		Name         string   `json:"name"`
		Backend      string   `json:"backend"`
		SizeGB       float64  `json:"size_gb"`
		Params       string   `json:"params,omitempty"`
		Capabilities []string `json:"capabilities"`
		Loaded       bool     `json:"loaded"`
		Tools        string   `json:"tool_strategy,omitempty"`
		TokPerSec    float64  `json:"tok_s,omitempty"`
		MemGB        float64  `json:"memory_gb,omitempty"`
		Role         string   `json:"role,omitempty"`
	}
	var rows []row
	for _, m := range ms {
		r := row{Name: m.Name, Backend: m.NB.ID, SizeGB: float64(int(float64(m.SizeBytes)/1e8+0.5)) / 10, Loaded: m.Loaded, Tools: string(m.Strategy)}
		if m.ParamsB > 0 {
			r.Params = fmt.Sprintf("%.1fB", m.ParamsB)
		}
		for _, k := range m.Capabilities {
			r.Capabilities = append(r.Capabilities, string(k))
		}
		switch {
		case m.Name == cfg.DecisionModel:
			r.Role = "decision"
		case m.Name == cfg.EmbedModel || strings.HasPrefix(m.Name, cfg.EmbedModel+":"):
			r.Role = "embedding"
		}
		if p, ok, _ := a.DB.GetPerf(ctx, m.ID); ok {
			r.TokPerSec, r.MemGB = float64(int(p.TokPerSec*10+0.5))/10, float64(int(float64(p.MemBytes)/1e8+0.5))/10
		}
		rows = append(rows, r)
	}
	if c.g.json {
		c.printJSON(map[string]any{"models": rows})
		return ExitOK
	}
	t := [][]string{{"MODEL", "SIZE", "CAPABILITIES", "LOADED", "TOOLS", "SPEED", "MEMORY", "ROLE"}}
	for _, r := range rows {
		loaded, speed, mem := "-", "-", "-"
		if r.Loaded {
			loaded = "yes"
		}
		if r.TokPerSec > 0 {
			speed, mem = fmt.Sprintf("%.0f tok/s", r.TokPerSec), fmt.Sprintf("%.1f GB", r.MemGB)
		}
		tools := r.Tools
		if tools == "" {
			tools = "-"
		}
		t = append(t, []string{r.Name, fmt.Sprintf("%.1f GB", r.SizeGB), strings.Join(r.Capabilities, ","), loaded, tools, speed, mem, r.Role})
	}
	c.table(t)
	return ExitOK
}

func modelsProbe(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("models probe", flag.ContinueOnError)
	all := fs.Bool("all", false, "probe again even if already probed")
	names, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(names) == 0 {
		ms, errs := a.Models(ctx)
		if len(ms) == 0 && len(errs) > 0 {
			return c.fail(&app.NoBackendError{Errs: errs})
		}
		for _, m := range ms {
			if m.Has(backends.CapChat) && (*all || m.Strategy == "") {
				names = append(names, m.Name)
			}
		}
		if len(names) == 0 {
			c.outf("every chat model has been probed (use --all to probe again)\n")
			return ExitOK
		}
	}
	code := ExitOK
	for _, n := range names {
		c.infof("probing %s (10 short tasks, native then constrained)…\n", n)
		res, err := a.Probe(ctx, n)
		if err != nil {
			c.errf("noskill: %s: %v\n", n, err)
			code = ExitError
			continue
		}
		line := fmt.Sprintf("%-28s tools: %s", n, res.Strategy)
		if res.Native != nil {
			line += fmt.Sprintf("   native %d/%d", res.Native.Passed, res.Native.Total)
		}
		if res.Constrained != nil {
			line += fmt.Sprintf("   constrained %d/%d", res.Constrained.Passed, res.Constrained.Total)
		}
		c.outf("✓ %s\n    %s\n", line, res.Reason)
	}
	return code
}

func modelsBench(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("models bench", flag.ContinueOnError)
	perfOnly := fs.Bool("perf-only", false, "measure speed and memory only; skip the quality micro-eval")
	names, err := c.parse(fs, args)
	if err != nil || len(names) != 1 {
		return c.usageErr("usage: noskill models bench <model> [--perf-only]   (unloads the model first to time a cold load)")
	}
	p, err := a.Bench(ctx, names[0], func(s string) { c.infof("  %s…\n", s) })
	if err != nil {
		return c.fail(err)
	}
	var scores map[string]float64
	if !*perfOnly {
		// FR-B4: a micro-eval per task type; the router starts from these scores.
		if scores, err = a.BenchScores(ctx, names[0], func(s string) { c.infof("  quality %s\n", s) }); err != nil {
			return c.fail(err)
		}
	}
	if c.g.json {
		c.printJSON(map[string]any{"perf": p, "scores": scores})
		return ExitOK
	}
	c.outf("✓ %s: %.1f tokens/s, first token in %.0f ms, %.1f GB in memory at %d context, cold load %.1f s\n",
		names[0], p.TokPerSec, p.TTFTMs, float64(p.MemBytes)/1e9, p.Ctx, p.LoadMsCold/1000)
	if len(scores) > 0 {
		var ts []string
		for t := range scores {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		var parts []string
		for _, t := range ts {
			parts = append(parts, fmt.Sprintf("%s %.2f", t, scores[t]))
		}
		c.outf("  quality: %s\n", strings.Join(parts, " · "))
	}
	return ExitOK
}

// ---- route explain ----

func cmdRoute(c *cmd, args []string) int {
	if len(args) == 0 || args[0] != "explain" {
		return c.usageErr("usage: noskill route explain \"<prompt>\" [--model m] [--preset p] [--skill s]")
	}
	fs := flag.NewFlagSet("route explain", flag.ContinueOnError)
	var o runOptions
	o.bind(fs)
	pos, err := c.parse(fs, args[1:])
	if err != nil {
		return ExitUsage
	}
	prompt := strings.TrimSpace(strings.Join(pos, " "))
	if prompt == "" {
		return c.usageErr("give the prompt to explain")
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	rr, err := o.request([]backends.Message{{Role: backends.RoleUser, Content: prompt}})
	if err != nil {
		return c.usageErr("%v", err)
	}
	pv, err := a.Explain(ctx, rr)
	if pv == nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(pv)
	} else {
		c.outf("%s", pv.Explanation.Text())
		if len(pv.Matches) > 0 {
			var parts []string
			for _, m := range pv.Matches {
				parts = append(parts, fmt.Sprintf("%s (%.2f, %s)", m.Name, m.Score, m.Method))
			}
			c.outf("skills: %s\n", strings.Join(parts, ", "))
		} else {
			c.outf("skills: none selected\n")
		}
		if len(pv.Skills) > 0 {
			c.outf("with: %s\ntools: %s\n", strings.Join(pv.Skills, ", "), strings.Join(pv.Tools, ", "))
		}
	}
	if err != nil {
		c.errf("noskill: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// ---- decide ----

func cmdDecide(c *cmd, args []string) int {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	model := fs.String("model", "", "decision model (default: orchestrator.decision_model)")
	state := fs.String("state", "{}", "JSON object: what the questions are about")
	questions := fs.String("questions", "", `JSON: {"name": {"type":"noul|choice|score","instructions":"…","choices":{…},"scale":[…]}}`)
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	if *questions == "" {
		return c.usageErr(`usage: noskill decide --state '{"request":"…"}' --questions '{"q":{"type":"noul","instructions":"Is this about code?"}}'`)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(*state), &st); err != nil {
		return c.usageErr("--state isn't a JSON object: %v", err)
	}
	var raw map[string]struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Choices      map[string]string `json:"choices"`
		Scale        []string          `json:"scale"`
	}
	if err := json.Unmarshal([]byte(*questions), &raw); err != nil {
		return c.usageErr("--questions isn't valid JSON: %v", err)
	}
	qs := map[string]decide.Question{}
	for n, q := range raw {
		qs[n] = decide.Question{Type: decide.Type(q.Type), Instructions: q.Instructions, Choices: q.Choices, Scale: q.Scale}
	}
	if err := decide.Validate(qs); err != nil {
		return c.usageErr("%v", err)
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	ans, err := a.Decide(ctx, *model, st, qs)
	if err != nil {
		return c.fail(err)
	}
	c.printJSON(ans)
	return ExitOK
}
