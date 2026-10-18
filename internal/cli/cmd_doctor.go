package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/backends"
	"noskill/internal/config"
	"noskill/internal/orchestrator"
	"noskill/internal/platform"
	"noskill/internal/sandbox"
	"noskill/internal/store"
)

type check struct {
	Name   string `json:"check"`
	Status string `json:"status"` // ok | warn | failed | skipped
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type report struct {
	checks []check
	failed bool
}

func (r *report) add(name, status, detail, fix string) {
	r.checks = append(r.checks, check{name, status, detail, fix})
	if status == "failed" {
		r.failed = true
	}
}

func cmdDoctor(c *cmd, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	client := fs.String("client", "", "check a chat app's connection (openwebui)")
	files := fs.Bool("files", false, "also do one real move, copy, trash and undo in a folder of noskill's own (uses the OS trash)")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ctx, stop := c.env.Context()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	r := &report{}
	if err := c.load(true); err != nil {
		r.add("config", "failed", err.Error(), "fix config.toml, then run `noskill config check`")
		return c.printReport(r)
	}
	issues := config.Check(c.loaded, secretChecker{c})
	switch n := countErrors(issues); {
	case n > 0:
		r.add("config", "failed", fmt.Sprintf("%d error(s)", n), "run `noskill config check`")
	case len(issues) > 0:
		r.add("config", "warn", fmt.Sprintf("%d warning(s)", len(issues)), "run `noskill config check`")
	default:
		if len(c.loaded.Files) == 0 {
			r.add("config", "ok", "no config file; using defaults", "run `noskill init` to write a starting config")
		} else {
			r.add("config", "ok", "valid: "+strings.Join(c.loaded.Files, ", "), "")
		}
	}
	if err := c.dirs.Ensure(); err != nil {
		r.add("folders", "failed", err.Error(), "")
		return c.printReport(r)
	}
	r.add("folders", "ok", fmt.Sprintf("config %s, data %s", c.dirs.Config, c.dirs.Data), "")

	a, err := c.open(ctx)
	if err != nil {
		fix := ""
		if errors.Is(err, store.ErrSchemaNewer) {
			fix = "this database was written by a newer noskill: update noskill, or restore a state.db.bak-N backup"
		}
		r.add("database", "failed", err.Error(), fix)
		c.memoryAndSandbox(ctx, r, nil)
		return c.printReport(r)
	}
	defer a.Close()
	v, _ := a.DB.Version(ctx)
	r.add("database", "ok", fmt.Sprintf("%s, schema %d", c.dirs.StateDB(), v), "")

	c.checkBackends(ctx, r, a)
	c.memoryAndSandbox(ctx, r, a)
	c.checkNotifications(r, a)
	c.checkSkills(ctx, r, a)
	c.checkDaemon(ctx, r)
	if pending, _ := a.Approvals.List(ctx, false, 0); len(pending) > 0 {
		r.add("approvals", "warn", fmt.Sprintf("%d waiting for you", len(pending)), "run `noskill approvals`")
	} else {
		r.add("approvals", "ok", "nothing waiting", "")
	}
	if free := a.DiskFree(); free < 5<<30 {
		r.add("disk", "warn", fmt.Sprintf("%.1f GB free", float64(free)/1e9), "models and traces need room; free some space")
	} else {
		r.add("disk", "ok", fmt.Sprintf("%.0f GB free", float64(free)/1e9), "")
	}
	if *client != "" {
		c.checkClient(r, *client)
	}
	if *files {
		for _, st := range a.FilesSelfTest(ctx) {
			r.add("files: "+st.Name, st.Status, st.Detail, "")
		}
	}
	return c.printReport(r)
}

func countErrors(issues []config.Issue) int {
	n := 0
	for _, i := range issues {
		if i.Level == "error" {
			n++
		}
	}
	return n
}

func (c *cmd) checkBackends(ctx context.Context, r *report, a *app.App) {
	if len(a.Backends) == 0 {
		r.add("backends", "failed", "none configured", "add a [[backends]] entry, or run `noskill init`")
		return
	}
	ms, errs := a.Models(ctx)
	byBackend := map[string]int{}
	for _, m := range ms {
		byBackend[m.NB.ID]++
	}
	for _, nb := range a.Backends {
		var berr error
		for _, e := range errs {
			if strings.HasPrefix(e.Error(), nb.ID+": ") {
				berr = e
			}
		}
		if berr != nil {
			r.add("backend "+nb.ID, "failed", strings.TrimPrefix(berr.Error(), nb.ID+": "), "start the server (for Ollama: `ollama serve`)")
			continue
		}
		detail := fmt.Sprintf("%s · %d models", nb.URL, byBackend[nb.ID])
		status, fix := "ok", ""
		if vf, ok := nb.B.(interface {
			Version(context.Context) (string, error)
		}); ok {
			if ver, err := vf.Version(ctx); err == nil {
				detail = fmt.Sprintf("%s · Ollama %s · %d models", nb.URL, ver, byBackend[nb.ID])
				if olderThan(ver, 0, 35) {
					status, fix = "warn", "decision models need Ollama 0.35 or newer; noskill falls back to the chat model without it"
				}
			}
		}
		r.add("backend "+nb.ID, status, detail, fix)
	}
	var chat, probed int
	var haveDecide, haveEmbed bool
	cfg := a.Cfg.Orchestrator
	for _, m := range ms {
		if m.Has(backends.CapChat) {
			chat++
			if m.Strategy != "" {
				probed++
			}
		}
		haveDecide = haveDecide || (cfg.DecisionModel != "" && m.Name == cfg.DecisionModel && m.Has(backends.CapDecide))
		haveEmbed = haveEmbed || (m.Has(backends.CapEmbed) && (m.Name == cfg.EmbedModel || strings.HasPrefix(m.Name, cfg.EmbedModel+":")))
	}
	switch {
	case chat == 0 && len(errs) < len(a.Backends):
		r.add("models", "failed", "no chat model installed", "pull one, for example `ollama pull qwen3:4b`")
	case chat > 0:
		fix := ""
		st := "ok"
		if probed < chat {
			st, fix = "warn", fmt.Sprintf("%d of %d models haven't been probed for tool calling: `noskill models probe`", chat-probed, chat)
		}
		r.add("models", st, fmt.Sprintf("%d chat models, %d probed", chat, probed), fix)
	}
	switch {
	case cfg.DecisionModel == "":
		r.add("decision model", "ok", "none configured: the chat model answers routing questions (about 2 s)", "")
	case haveDecide:
		r.add("decision model", "ok", cfg.DecisionModel+" installed", "")
	default:
		r.add("decision model", "warn", cfg.DecisionModel+" isn't installed; the chat model answers routing questions instead (slower)", "ollama pull "+cfg.DecisionModel)
	}
	if haveEmbed {
		r.add("embed model", "ok", cfg.EmbedModel+" installed", "")
	} else {
		r.add("embed model", "warn", cfg.EmbedModel+" isn't installed; skills are matched by keywords instead of meaning", "ollama pull "+cfg.EmbedModel)
	}
}

// olderThan compares a version string like "0.40.3" with major.minor.
func olderThan(ver string, major, minor int) bool {
	var a, b int
	if _, err := fmt.Sscanf(strings.TrimPrefix(ver, "v"), "%d.%d", &a, &b); err != nil {
		return false
	}
	return a < major || (a == major && b < minor)
}

func (c *cmd) memoryAndSandbox(ctx context.Context, r *report, a *app.App) {
	cfg := config.Defaults()
	if c.loaded != nil {
		cfg = c.loaded.Config
	}
	if mem, err := platform.ReadMemory(); err != nil {
		r.add("memory", "warn", err.Error(), "set orchestrator.memory_budget_gb")
	} else {
		budget, why := orchestrator.MemoryBudget(cfg.Orchestrator.MemoryBudgetGB, mem)
		detail := fmt.Sprintf("%.0f GB RAM, model budget %.1f GB (%s)", float64(mem.TotalBytes)/(1<<30), float64(budget)/1e9, why)
		if ceil := orchestrator.BurstCeiling(cfg.Orchestrator.MemoryBurstGB, budget, mem); ceil > budget {
			detail += fmt.Sprintf(", grows to %.1f GB when memory is free (%.1f GB free now)", float64(ceil)/1e9, float64(mem.AvailableBytes)/1e9)
		}
		if mem.Pressure != "" && mem.Pressure != "normal" {
			r.add("memory", "warn", detail+", pressure "+mem.Pressure, "close apps or lower orchestrator.memory_budget_gb")
		} else {
			r.add("memory", "ok", detail, "")
		}
	}
	sb := c.env.Sandbox
	if sb == nil {
		sb = platform.NewSandbox()
	}
	st := sb.Strength()
	dir, err := os.MkdirTemp("", "noskill-doctor-")
	if err != nil {
		r.add("sandbox", "failed", err.Error(), "")
		return
	}
	defer os.RemoveAll(dir) // our own scratch folder in the temp directory
	run := &sandbox.Runner{Sandbox: sb, Mode: cfg.Sandbox.Mode, RunsDir: dir, Timeout: 10 * time.Second, MemBytes: 256 << 20}
	detail := fmt.Sprintf("%s: files %s, network %s, limits %s", st.Mechanism, st.Files, st.Network, st.Limits)
	switch err := sandbox.SelfTest(ctx, run); {
	case errors.Is(err, platform.ErrNoSandbox):
		r.add("sandbox", "warn", detail+"; script tools won't run", "no script sandbox on "+runtime.GOOS+" yet (S9 open); [sandbox] mode = \"warn\" runs them unconfined")
	case err != nil:
		r.add("sandbox", "failed", detail+": "+err.Error(), "")
	case cfg.Sandbox.Mode == "warn" && st.Files == platform.None:
		r.add("sandbox", "warn", detail+"; scripts run unconfined ([sandbox] mode = \"warn\")", "")
	default:
		r.add("sandbox", "ok", detail+"; self-test passed", "")
	}
}

func (c *cmd) checkNotifications(r *report, a *app.App) {
	n := a.Cfg.Notify
	var ch []string
	if n.OS {
		ch = append(ch, "os")
	}
	if n.Ntfy != nil {
		ch = append(ch, "ntfy")
	}
	if n.Telegram != nil {
		ch = append(ch, "telegram")
	}
	if n.Discord != nil {
		ch = append(ch, "discord")
	}
	if n.Email != nil {
		ch = append(ch, "email")
	}
	if len(a.Hub.Channels) < len(ch) {
		r.add("notifications", "warn", fmt.Sprintf("%d of %d channels couldn't be set up (%s)", len(ch)-len(a.Hub.Channels), len(ch), strings.Join(ch, ", ")), "see `noskill config check`")
		return
	}
	if runtime.GOOS == "windows" && n.OS {
		r.add("notifications", "warn", "OS notifications aren't available on Windows yet; approvals still work from `noskill approvals`", "add an ntfy, Telegram or Discord channel")
		return
	}
	r.add("notifications", "ok", strings.Join(ch, ", ")+" (plain messages; answer approvals with `noskill approvals approve <id>`)", "")
}

func (c *cmd) checkSkills(ctx context.Context, r *report, a *app.App) {
	all := a.Skills.All()
	var waiting []string
	enabled, _ := a.DB.EnabledSkills(ctx)
	for _, s := range all {
		if s.NeedsApproval() && !enabled[s.Name] {
			waiting = append(waiting, s.Name)
		}
	}
	errs := 0
	for _, i := range a.SkillIssues {
		if i.Level == "error" {
			errs++
		}
	}
	detail := fmt.Sprintf("%d skills", len(all))
	switch {
	case errs > 0:
		r.add("skills", "warn", fmt.Sprintf("%s, %d could not be loaded", detail, errs), "run with --verbose to see why")
	case len(waiting) > 0:
		r.add("skills", "warn", detail+"; from this folder and not enabled: "+strings.Join(waiting, ", "), "review with `noskill skills show <name>`")
	default:
		r.add("skills", "ok", detail, "")
	}
}

func (c *cmd) checkClient(r *report, name string) {
	port := c.loaded.Config.Server.Port
	switch name {
	case "openwebui":
		hosts := c.loaded.Config.Server.AllowedHosts
		r.add("openwebui", "ok", fmt.Sprintf("native install: Ollama or OpenAI connection at http://127.0.0.1:%d (OpenAI: add /v1); Docker Desktop: http://host.docker.internal:%d", port, port), "")
		hasDocker := false
		for _, h := range hosts {
			hasDocker = hasDocker || h == "host.docker.internal"
		}
		if !hasDocker {
			r.add("openwebui host", "warn", "host.docker.internal isn't in server.allowed_hosts; Open WebUI in Docker would get a 403", "noskill config set server.allowed_hosts host.docker.internal")
		}
	default:
		r.add("client", "failed", "unknown client "+name+" (openwebui)", "")
	}
}

func (c *cmd) printReport(r *report) int {
	if c.g.json {
		c.printJSON(map[string]any{"version": c.env.Version, "checks": r.checks, "ok": !r.failed})
	} else {
		for _, k := range r.checks {
			c.outf("%-7s %-16s %s\n", k.Status, k.Name, k.Detail)
			if k.Fix != "" {
				c.outf("        %-16s fix: %s\n", "", k.Fix)
			}
		}
	}
	if r.failed {
		return ExitError
	}
	return ExitOK
}

func (c *cmd) checkDaemon(ctx context.Context, r *report) {
	st, err := c.serviceManager().Status(ctx)
	switch {
	case err != nil:
		r.add("service", "warn", "couldn't check: "+err.Error(), "")
	case !st.Installed:
		r.add("service", "ok", "not installed (needed for chat apps that start with the computer)", "noskill daemon install")
	case st.Running:
		r.add("service", "ok", fmt.Sprintf("running (pid %d)", st.PID), "")
	default:
		r.add("service", "warn", "installed but not running: "+st.Detail, "noskill daemon start")
	}
}
