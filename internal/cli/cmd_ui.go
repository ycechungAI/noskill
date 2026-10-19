package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"noskill/internal/platform"
)

// openURL opens the status page; tests replace it.
var openURL = platform.OpenURL

var uiTabs = map[string]bool{"overview": true, "approvals": true, "downloads": true, "history": true}

// cmdUI opens the status page (Overview, Approvals, Downloads, History). When the daemon or a
// `noskill serve` is already answering, that is the page; otherwise this starts a page-and-API
// server with no scheduled jobs and keeps it up until Ctrl-C. It is the same on macOS, Windows and
// Linux, so nothing needs a shell script.
func cmdUI(c *cmd, args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	port := fs.Int("port", 0, "port for the page (default: server.port, 11435)")
	noOpen := fs.Bool("no-open", false, "don't open a browser; just print the address")
	pos, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	tab := "approvals"
	if len(pos) > 1 || (len(pos) == 1 && !uiTabs[pos[0]]) {
		c.errf("usage: noskill ui [overview|approvals|downloads|history] [--port N] [--no-open]\n")
		return ExitUsage
	}
	if len(pos) == 1 {
		tab = pos[0]
	}

	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	p := a.Cfg.Server.Port
	a.Close()
	if *port > 0 {
		p = *port
	}
	base := "http://127.0.0.1:" + strconv.Itoa(p)
	url := base + "/ui#" + tab

	if serving(ctx, base) {
		c.errf("noskill is already serving: %s\n", url)
		if !*noOpen {
			if err := openURL(ctx, url); err != nil {
				c.errf("couldn't open a browser (%v): open %s yourself\n", err, url)
			}
		}
		return ExitOK
	}
	if !*noOpen {
		go func() { // once our own server answers
			for i := 0; i < 100; i++ {
				if serving(ctx, base) {
					if err := openURL(ctx, url); err != nil {
						c.errf("couldn't open a browser (%v): open %s yourself\n", err, url)
					}
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(150 * time.Millisecond):
				}
			}
		}()
	}
	c.errf("status page: %s   (Ctrl-C stops it)\n", url)
	return cmdServe(c, []string{"--no-jobs", "--port", fmt.Sprint(p)})
}

// serving says whether something answers on base.
func serving(ctx context.Context, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
