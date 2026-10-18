package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"

	"noskill/internal/app"
	"noskill/internal/scheduler"
	"noskill/internal/server"
)

// schedulerTick is how often the scheduler looks for due jobs, answered approvals and stop
// requests (tests shorten it).
var schedulerTick = 5 * time.Second

func cmdServe(c *cmd, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 0, "port to listen on (default: server.port, 11435)")
	lan := fs.Bool("lan", false, "listen on all addresses; needs a [[server.tokens]] entry")
	bind := fs.String("bind", "", "address to listen on (default: server.bind, 127.0.0.1)")
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
	if *port > 0 {
		a.Cfg.Server.Port = *port
	}
	if *bind != "" {
		a.Cfg.Server.Bind = *bind
	}
	if *lan {
		a.Cfg.Server.Bind = "0.0.0.0"
	}
	srv, err := server.New(a, c.env.Version)
	if err != nil {
		// Typically: a bind beyond localhost without a token, or a token secret that isn't stored.
		return c.fail(err)
	}
	ln, err := net.Listen("tcp", srv.Addr())
	if err != nil {
		return c.fail(fmt.Errorf("can't listen on %s: %w (is noskill already running?)", srv.Addr(), err))
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	c.errf("noskill %s serving on http://%s\n  OpenAI API   http://%s/v1\n  Ollama API   http://%s\n", c.env.Version, ln.Addr(), ln.Addr(), ln.Addr())
	if a.Cfg.Server.Bind != "127.0.0.1" && a.Cfg.Server.Bind != "localhost" && a.Cfg.Server.Bind != "::1" {
		c.errf("  Listening beyond this computer: every request needs a token.\n")
	}

	// The scheduler runs here, so jobs run wherever the server does: the daemon, or a foreground
	// `noskill serve` (FR-J2). A lease in state.db keeps a second process from running them too.
	sch := &scheduler.Scheduler{Store: &scheduler.Store{DB: a.DB}, Exec: app.Jobs{A: a}, Notify: a.NotifyJob,
		MaxConcurrent: a.Cfg.Scheduler.MaxConcurrentJobs, Tick: schedulerTick, Check: &scheduler.PlatformChecker{Volumes: a.Volumes()},
		Log: func(format string, args ...any) {
			c.errf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
		}}
	schDone := make(chan struct{})
	go func() {
		defer close(schDone)
		sch.Run(ctx)
	}()
	defer func() { <-schDone }() // running jobs are cancelled and recorded before the database closes

	// The learning worker works only when noskill has been idle a while, on AC power (LEARNING.md).
	learnDone := make(chan struct{})
	go func() {
		defer close(learnDone)
		a.LearnLoop(ctx, func(format string, args ...any) {
			c.errf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
		})
	}()
	defer func() { <-learnDone }()

	// Sweep expired approvals while serving.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.Approvals.ExpireDue(ctx)
			}
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return c.fail(err)
		}
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutCtx)
		c.errf("noskill: stopped\n")
	}
	return ExitOK
}
