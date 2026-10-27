package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"

	"noskill/internal/app"
	"noskill/internal/platform"
)

// Job backups (FR-J6): `schedule backup`, `schedule backups`, `schedule restore`.

func scheduleBackup(c *cmd, ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("schedule backup", flag.ContinueOnError)
	to := fs.String("to", "", "write the backup to this file (default: a new file in the backups folder)")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	path, err := platform.ExpandHome(*to)
	if err != nil {
		return c.fail(err)
	}
	info, err := a.BackupJobs(ctx, path)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(info)
		return ExitOK
	}
	if info.Unchanged {
		c.outf("✓ jobs unchanged since the last backup: %s\n", info.Path)
		return ExitOK
	}
	c.outf("✓ backed up %d jobs to %s\n", info.Jobs, info.Path)
	c.outf("  restore with: noskill schedule restore %s\n", restoreArg(*to, info))
	return ExitOK
}

func restoreArg(to string, info app.JobBackupInfo) string {
	if to != "" {
		return info.Path
	}
	return info.Name
}

func scheduleBackups(c *cmd, ctx context.Context) int {
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	list, err := a.ListJobBackups()
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(list)
		return ExitOK
	}
	if len(list) == 0 {
		c.outf("no job backups yet in %s: `noskill schedule backup` makes one\n", a.JobBackupsDir())
		return ExitOK
	}
	c.outf("%-28s %-5s %s\n", "NAME", "JOBS", "MADE")
	for _, b := range list {
		c.outf("%-28s %-5d %s\n", b.Name, b.Jobs, when2(b.CreatedAt))
	}
	c.outf("in %s\n", a.JobBackupsDir())
	return ExitOK
}

// scheduleRestore takes a backup's name in the backups folder, or the path of a backup file from
// anywhere (another account's, a copy on a drive).
func scheduleRestore(c *cmd, ctx context.Context, arg string) int {
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	var rep app.RestoreReport
	p, err := platform.ExpandHome(arg)
	if err != nil {
		return c.fail(err)
	}
	if strings.ContainsAny(arg, `/\`) || fileExists(p) {
		data, err := os.ReadFile(p)
		if err != nil {
			return c.fail(err)
		}
		b, err := app.ParseJobBackup(data)
		if err == nil {
			rep, err = a.RestoreJobs(ctx, b)
		}
		if err != nil {
			return restoreFail(c, err)
		}
	} else if rep, err = a.RestoreJobBackup(ctx, arg); err != nil {
		return restoreFail(c, err)
	}
	if c.g.json {
		c.printJSON(rep)
		return ExitOK
	}
	for _, n := range rep.Added {
		c.outf("✓ restored %s\n", n)
	}
	for _, s := range rep.Skipped {
		c.outf("- skipped %s: %s\n", s.Name, s.Why)
	}
	if len(rep.Added) == 0 && len(rep.Skipped) == 0 {
		c.outf("the backup has no jobs\n")
	}
	if len(rep.Added) > 0 {
		c.daemonHint(ctx)
	}
	return ExitOK
}

func restoreFail(c *cmd, err error) int {
	var ue *app.UserError
	if errors.As(err, &ue) {
		return c.usageErr("%v", err)
	}
	return c.fail(err)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
