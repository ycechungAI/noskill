package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noskill/internal/service"
	"noskill/internal/store"
	"noskill/internal/update"
)

func (c *cmd) executable() (string, error) {
	var exe string
	var err error
	if c.env.Executable != nil {
		exe, err = c.env.Executable()
	} else {
		exe, err = os.Executable()
	}
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

func cmdUpdate(c *cmd, args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only say whether a newer version exists")
	to := fs.String("to", "", "install this version (also how to roll back)")
	channel := fs.String("channel", "", "stable | beta")
	yes := fs.Bool("yes", false, "don't ask")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	ctx, stop := c.env.Context()
	defer stop()
	cfg := c.loaded.Config.Update
	if *channel == "" {
		*channel = cfg.Channel
	}
	cl := &update.Client{Repo: cfg.Repo}
	for _, e := range c.environ() {
		if v, ok := strings.CutPrefix(e, "NOSKILL_UPDATE_API="); ok {
			cl.API = v // a mirror, or a test server
		}
	}
	var rel update.Release
	var err error
	if *to != "" {
		rel, err = cl.Get(ctx, *to)
	} else {
		rel, err = cl.Latest(ctx, *channel)
	}
	if err != nil {
		return c.fail(err)
	}
	cur := c.env.Version
	newer := update.Compare(cur, rel.Version) < 0
	if *check {
		if newer {
			c.outf("noskill %s is available (you have %s). Run `noskill update`.\n", rel.Version, cur)
		} else {
			c.outf("noskill %s is up to date\n", cur)
		}
		return ExitOK
	}
	if *to == "" && !newer {
		c.outf("noskill %s is up to date\n", cur)
		return ExitOK
	}
	exe, err := c.executable()
	if err != nil {
		return c.fail(err)
	}
	if mgr, cmdline := update.InstalledVia(exe); mgr != "" {
		c.outf("noskill was installed with %s; update it there so the two don't disagree:\n  %s\n", mgr, cmdline)
		return ExitOK
	}
	if !*yes && !c.ask(fmt.Sprintf("Update noskill %s → %s?", cur, rel.Version), true) {
		c.errf("not updated (use --yes without a terminal)\n")
		return ExitError
	}
	c.infof("downloading %s and checking its signature…\n", rel.Tag)
	nb, err := cl.Fetch(ctx, rel, filepath.Dir(exe))
	if err != nil {
		return c.fail(err)
	}
	defer os.Remove(nb) // our own download, if the swap didn't consume it
	if err := update.SmokeTest(ctx, nb, rel.Version); err != nil {
		return c.fail(err)
	}
	svc := c.serviceManager()
	st, _ := svc.Status(ctx)
	if st.Running {
		c.infof("stopping the background service…\n")
		svc.Stop(ctx)
	}
	backup, err := update.Swap(exe, nb)
	if err != nil {
		if st.Running {
			svc.Start(ctx)
		}
		return c.fail(err)
	}
	if st.Running {
		svc.Start(ctx)
		if !waitForServer(ctx, c.loaded.Config.Server.Port, 30*time.Second) {
			c.errf("the new version didn't come up within 30 s; putting %s back\n", cur)
			svc.Stop(ctx)
			if err := update.Rollback(exe, backup); err != nil {
				return c.fail(fmt.Errorf("rollback failed: %w (the old binary is at %s)", err, backup))
			}
			if err := update.RestoreDB(c.dirs.Data, store.Latest(), func(p string) (string, error) { return c.trasher().Trash(p) }); err != nil {
				c.errf("note: %v\n", err)
			}
			svc.Start(ctx)
			return ExitError
		}
	}
	c.outf("✓ updated to %s (the previous version is kept as %s)\n", rel.Version, filepath.Base(backup))
	return ExitOK
}

func cmdUninstall(c *cmd, args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also move config, skills, traces and the database to the trash")
	yes := fs.Bool("yes", false, "don't ask")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	ctx, stop := c.env.Context()
	defer stop()
	exe, err := c.executable()
	if err != nil {
		return c.fail(err)
	}
	mgr, cmdline := update.InstalledVia(exe)
	if !*yes && !c.ask("Remove the background service and noskill's binary? Your data is kept unless you used --purge.", false) {
		c.errf("nothing removed (use --yes without a terminal)\n")
		return ExitError
	}
	if err := c.serviceManager().Uninstall(ctx); err != nil && !errors.Is(err, service.ErrNotInstalled) {
		return c.fail(err)
	}
	c.outf("✓ background service removed\n")
	if mgr != "" {
		c.outf("  noskill was installed with %s; remove the program with:\n    %s\n", mgr, uninstallCmd(cmdline))
	} else if _, err := c.trasher().Trash(exe); err != nil {
		c.errf("couldn't move %s to the trash: %v\n", exe, err)
	} else {
		c.outf("✓ moved %s to the trash\n", exe)
	}
	if *purge {
		size := dirSize(c.dirs.Data)
		if !*yes && !c.ask(fmt.Sprintf("Move %s (%.1f MB) to the trash?", c.dirs.Data, float64(size)/1e6), false) {
			c.errf("data kept\n")
			return ExitOK
		}
		for _, d := range c.dirs.Own() {
			if _, err := c.trasher().Trash(d); err != nil {
				c.errf("couldn't move %s to the trash: %v\n", d, err)
			} else {
				c.outf("✓ moved %s to the trash\n", d)
			}
		}
	} else {
		c.outf("  Your config, skills and history are kept in %s. `noskill uninstall --purge` moves them to the trash.\n", c.dirs.Data)
	}
	c.outf("  Ollama and its models were not touched.\n")
	return ExitOK
}

func uninstallCmd(update string) string {
	return strings.NewReplacer("upgrade", "uninstall", "update noskill", "uninstall noskill").Replace(update)
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

var _ = context.Background
