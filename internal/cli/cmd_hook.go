package cli

import (
	"flag"
	"strings"
)

// cmdHook is what a torrent app runs when a download finishes (SKILL.md "Filing"): it files that
// one torrent now instead of waiting for the next scheduled run.
//
//	qBittorrent: Options › Downloads › Run external program on torrent finished:
//	             noskill hook qbittorrent-finished "%I"
//	Transmission: settings.json "script-torrent-done-enabled": true and
//	             "script-torrent-done-filename": "<path to noskill>" with the argument-less form
//	             below (Transmission passes the hash in TR_TORRENT_HASH).
func cmdHook(c *cmd, args []string) int {
	usage := "usage: noskill hook qbittorrent-finished|transmission-finished|torrent-finished [--dry-run] [<torrent hash>]"
	if len(args) == 0 {
		return c.usageErr(usage)
	}
	switch args[0] {
	case "qbittorrent-finished", "transmission-finished", "torrent-finished":
	default:
		return c.usageErr(usage)
	}
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "say what would happen; change nothing")
	rest, err := c.parse(fs, args[1:])
	if err != nil {
		return ExitUsage
	}
	hash := ""
	if len(rest) > 0 {
		hash = rest[0]
	} else {
		for _, kv := range c.environ() { // Transmission passes the hash this way
			if v, ok := strings.CutPrefix(kv, "TR_TORRENT_HASH="); ok {
				hash = v
			}
		}
	}
	if strings.TrimSpace(hash) == "" {
		return c.usageErr("no torrent hash: pass it as an argument (qBittorrent's %%I) or set TR_TORRENT_HASH (Transmission)")
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	rep, err := a.AnimeFileFinished(ctx, hash, *dry)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(rep)
		return ExitOK
	}
	c.outf("%s\n", rep)
	return ExitOK
}
