package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noskill/internal/backends"
)

type found struct {
	kind, url, what string
}

// detectBackends looks for servers on their default ports (FR-B1). A short timeout keeps `init`
// quick on machines where nothing is listening.
func detectBackends(ctx context.Context) []found {
	probes := []struct {
		kind, url, path, what string
	}{
		{"ollama", "http://127.0.0.1:11434", "/api/version", "Ollama"},
		{"openai", "http://127.0.0.1:1234/v1", "/models", "LM Studio"},
		{"openai", "http://127.0.0.1:8080/v1", "/models", "llama.cpp server"},
	}
	hc := &http.Client{Timeout: 1500 * time.Millisecond}
	var out []found
	for _, p := range probes {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url+p.path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 300 {
			out = append(out, found{p.kind, p.url, p.what})
		}
	}
	return out
}

func cmdInit(c *cmd, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	probe := fs.Bool("probe", false, "probe tool calling on every chat model without asking")
	yes := fs.Bool("yes", false, "answer yes to the downloads offered (decision and embedding models)")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	ctx, stop := c.env.Context()
	defer stop()
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	if err := c.dirs.Ensure(); err != nil {
		return c.fail(err)
	}

	// 1. Backends.
	if _, err := os.Stat(c.dirs.ConfigFile()); os.IsNotExist(err) {
		det := detectBackends(ctx)
		if len(det) == 0 {
			c.outf("!       no model server found on the usual ports (Ollama :11434, LM Studio :1234, llama.cpp :8080)\n")
			c.outf("        Start one, then run `noskill init` again. Ollama: https://ollama.com\n")
			return ExitUnavailable
		}
		var b strings.Builder
		b.WriteString("# noskill config. Only list what you want to change; everything else keeps its default.\n# Reference: docs/CONFIG.md\n")
		for _, d := range det {
			fmt.Fprintf(&b, "\n[[backends]]\nkind = %q\nurl = %q\n", d.kind, d.url)
			c.outf("✓       found %s at %s\n", d.what, d.url)
		}
		if err := os.WriteFile(c.dirs.ConfigFile(), []byte(b.String()), 0o600); err != nil {
			return c.fail(err)
		}
		c.outf("✓       wrote %s\n", c.dirs.ConfigFile())
		c.loaded = nil
	} else {
		c.outf("✓       config: %s (left as it is)\n", c.dirs.ConfigFile())
	}

	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()

	// 2. Models.
	ms, errs := a.Models(ctx)
	for _, e := range errs {
		c.outf("!       %v\n", e)
	}
	if len(ms) == 0 {
		c.outf("!       no models installed. For example: ollama pull qwen3:4b\n")
		return ExitUnavailable
	}
	var chat []string
	for _, m := range ms {
		caps := make([]string, 0, len(m.Capabilities))
		for _, k := range m.Capabilities {
			caps = append(caps, string(k))
		}
		c.outf("✓       %-28s %s\n", m.Name, strings.Join(caps, " "))
		if m.Has(backends.CapChat) {
			chat = append(chat, m.Name)
		}
	}

	// 3. Models noskill relies on. A download is offered, never started on its own.
	cfg := a.Cfg.Orchestrator
	have := map[string]bool{}
	for _, m := range ms {
		have[m.Name] = true
	}
	var want []string
	if cfg.DecisionModel != "" && !have[cfg.DecisionModel] {
		want = append(want, cfg.DecisionModel)
	}
	if cfg.EmbedModel != "" && !have[cfg.EmbedModel] && !have[cfg.EmbedModel+":latest"] {
		want = append(want, cfg.EmbedModel)
	}
	for _, name := range want {
		c.outf("!       %s isn't installed (%s)\n", name, modelWhy(name, cfg.DecisionModel))
		if *yes || c.ask("        Download it now?", false) {
			if err := pullWithProgress(c, ctx, a, name); err != nil {
				c.errf("        download failed: %v\n", err)
			}
		} else {
			c.outf("        later: ollama pull %s\n", name)
		}
	}

	// 4. Tool-calling strategy: one probe per model, ten short tasks natively and constrained.
	var unprobed []string
	ms, _ = a.Models(ctx)
	for _, m := range ms {
		if m.Has(backends.CapChat) && m.Strategy == "" {
			unprobed = append(unprobed, m.Name)
		}
	}
	if len(unprobed) > 0 && (*probe || c.ask(fmt.Sprintf("Probe tool calling on %d model(s)? (about a minute each; loads the models)", len(unprobed)), false)) {
		for _, name := range unprobed {
			c.outf("…       probing %s\n", name)
			res, err := a.Probe(ctx, name)
			if err != nil {
				c.errf("!       %s: %v\n", name, err)
				continue
			}
			c.outf("✓       %-28s tools: %s (%s)\n", name, res.Strategy, res.Reason)
		}
	} else if len(unprobed) > 0 {
		c.outf("        later: noskill models probe\n")
	}

	// 5. The rest is a command away.
	c.outf("\nNext:\n")
	c.outf("  noskill run \"what changed in the latest Ollama release?\"\n")
	c.outf("  noskill serve                 # OpenAI + Ollama API on 127.0.0.1:%d for chat apps\n", a.Cfg.Server.Port)
	c.outf("  noskill daemon install        # keep it running in the background\n")
	c.outf("  noskill doctor                # check everything\n")
	_ = filepath.Join
	return ExitOK
}

func modelWhy(name, decision string) string {
	if name == decision {
		return "fast routing and judging decisions; without it the chat model answers them, about 2 s each"
	}
	return "matches skills by meaning; without it skills are matched by keywords"
}
