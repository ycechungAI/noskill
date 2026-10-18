package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"noskill/internal/service"
)

func (c *cmd) serviceManager() service.Manager {
	if c.env.Service != nil {
		return c.env.Service
	}
	return service.NewManager(nil, c.env.Trash)
}

func cmdDaemon(c *cmd, args []string) int {
	if len(args) == 0 {
		return c.usageErr("usage: noskill daemon install|uninstall|start|stop|status|logs")
	}
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	ctx, stop := c.env.Context()
	defer stop()
	m := c.serviceManager()
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("daemon install", flag.ContinueOnError)
		tray := fs.Bool("tray", false, "also install the tray app (arrives in Phase 2)")
		if _, err := c.parse(fs, args[1:]); err != nil {
			return ExitUsage
		}
		if *tray {
			c.errf("note: the tray app arrives in Phase 2; installing the background service only\n")
		}
		spec, err := service.ResolveSpec(c.dirs, c.g.home)
		if err != nil {
			return c.fail(err)
		}
		c.errf("Installing a background service that runs `noskill serve` when you log in:\n  program  %s\n  log      %s\n  remove   noskill daemon uninstall\n", spec.Exe, spec.LogPath)
		if err := m.Install(ctx, spec); err != nil {
			return c.fail(err)
		}
		port := c.loaded.Config.Server.Port
		if waitForServer(ctx, port, 8*time.Second) {
			c.outf("✓ running; answering on http://127.0.0.1:%d\n", port)
		} else {
			c.outf("✓ installed. It hasn't answered on port %d yet; `noskill daemon status` and `noskill daemon logs` show what it's doing.\n", port)
		}
		for _, n := range m.Notes() {
			c.outf("  %s\n", n)
		}
	case "uninstall":
		if err := m.Uninstall(ctx); err != nil {
			return c.fail(err)
		}
		c.outf("✓ removed (the unit file is in the trash)\n")
	case "start":
		if err := m.Start(ctx); err != nil {
			return c.fail(err)
		}
		c.outf("✓ started\n")
	case "stop":
		if err := m.Stop(ctx); err != nil {
			return c.fail(err)
		}
		c.outf("✓ stopped (`noskill daemon start` to run it again)\n")
	case "status":
		return daemonStatus(c, ctx, m)
	case "logs":
		fs := flag.NewFlagSet("daemon logs", flag.ContinueOnError)
		follow := fs.Bool("f", false, "follow the log")
		n := fs.Int("n", 50, "how many lines")
		if _, err := c.parse(fs, args[1:]); err != nil {
			return ExitUsage
		}
		spec, err := service.ResolveSpec(c.dirs, c.g.home)
		path := c.dirs.Logs() + "/daemon.log"
		if err == nil {
			path = spec.LogPath
		}
		if err := service.TailLog(ctx, path, *n, *follow, c.env.Stdout); err != nil {
			return c.fail(err)
		}
	default:
		return c.usageErr("unknown daemon command %q (install, uninstall, start, stop, status, logs)", args[0])
	}
	return ExitOK
}

func daemonStatus(c *cmd, ctx context.Context, m service.Manager) int {
	st, err := m.Status(ctx)
	if err != nil {
		return c.fail(err)
	}
	port := c.loaded.Config.Server.Port
	up := waitForServer(ctx, port, 1500*time.Millisecond)
	if c.g.json {
		c.printJSON(map[string]any{"installed": st.Installed, "running": st.Running, "pid": st.PID, "unit": st.Unit, "detail": st.Detail, "responding": up, "port": port})
	} else {
		switch {
		case !st.Installed:
			c.outf("not installed (run `noskill daemon install`)\n")
		case st.Running:
			c.outf("running (pid %d) · %s\n", st.PID, st.Detail)
		default:
			c.outf("installed, not running · %s\n", st.Detail)
		}
		if st.Unit != "" {
			c.outf("unit: %s\n", st.Unit)
		}
		if up {
			c.outf("answering on http://127.0.0.1:%d\n", port)
		} else if st.Running {
			c.outf("running, but not answering on port %d yet\n", port)
		}
	}
	if st.Installed && !st.Running {
		return ExitError
	}
	return ExitOK
}

// waitForServer polls the liveness probe until it answers or the time is up.
func waitForServer(ctx context.Context, port int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	hc := &http.Client{Timeout: 700 * time.Millisecond}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			return resp.StatusCode == 200
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

var _ = errors.New
var _ = strings.TrimSpace
