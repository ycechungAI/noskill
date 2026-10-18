package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"noskill/internal/app"
	"noskill/internal/skills"
)

// skillsImport brings in skills written for Claude Code, Codex, Hermes or any agentskills.io
// runtime (FR-S8). The folder is copied as it is; before that, the user is told what noskill won't
// do with it and what permissions it would have.
func skillsImport(c *cmd, ctx context.Context, a *app.App, args []string) int {
	usage := "usage: noskill skills import [<name>|<folder>] [--from claude|codex|hermes|agents]"
	fs := flag.NewFlagSet("skills import", flag.ContinueOnError)
	from := fs.String("from", "", "only look in one runtime's folders: claude, codex, hermes or agents")
	rest, err := c.parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(rest) > 1 {
		return c.usageErr("%s", usage)
	}
	if *from != "" && !map[string]bool{"claude": true, "codex": true, "hermes": true, "agents": true}[*from] {
		return c.usageErr("--from: want claude, codex, hermes or agents (or give a folder)")
	}
	home, _ := os.UserHomeDir()
	cwd, _ := c.env.Getwd()
	found := skills.Discover(home, cwd, os.Getenv)
	if *from != "" {
		keep := found[:0]
		for _, f := range found {
			if f.Runtime == *from {
				keep = append(keep, f)
			}
		}
		found = keep
	}

	if len(rest) == 0 {
		if c.g.json {
			c.printJSON(found)
			return ExitOK
		}
		if len(found) == 0 {
			c.outf("No skills found in the usual folders of Claude Code, Codex, Hermes or .agents.\nGive a folder: noskill skills import ~/path/to/skill\n")
			return ExitOK
		}
		rows := [][]string{{"FROM", "SKILL", "STATUS", "FOLDER"}}
		for _, f := range found {
			status := "ready"
			switch {
			case !f.Compat.Valid:
				status = "can't load: " + f.Compat.Problem
			case func() bool { _, ok := a.Skills.Get(f.Name); return ok }():
				status = "already installed"
			case len(f.Compat.Notices) > 0:
				status = fmt.Sprintf("%d note(s)", len(f.Compat.Notices))
			}
			rows = append(rows, []string{f.Runtime, f.Name, status, f.Dir})
		}
		c.table(rows)
		c.outf("\nnoskill skills import <name> shows what carries over and asks before copying.\n")
		return ExitOK
	}

	var cand skills.Candidate
	target := rest[0]
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		abs, _ := filepath.Abs(target)
		cand = skills.CandidateAt("path", abs)
	} else {
		var matches []skills.Candidate
		for _, f := range found {
			if f.Name == target || filepath.Base(f.Dir) == target {
				matches = append(matches, f)
			}
		}
		switch len(matches) {
		case 0:
			return c.usageErr("no skill named %q in the usual folders (see `noskill skills import`); or give its folder", target)
		case 1:
			cand = matches[0]
		default:
			var where []string
			for _, m := range matches {
				where = append(where, m.Runtime+": "+m.Dir)
			}
			return c.usageErr("%q is in more than one place; use --from or give its folder:\n  %s", target, strings.Join(where, "\n  "))
		}
	}
	if !cand.Compat.Valid {
		return c.usageErr("%s can't be loaded: %s", cand.Dir, cand.Compat.Problem)
	}
	sk, _ := app.LoadSkillDir(cand.Dir)
	c.errf("Importing skill %s from %s\n  %s\n  %s\n", cand.Name, cand.Runtime, cand.Description, cand.Dir)
	if len(cand.Compat.Notices) > 0 {
		c.errf("\nWhat works differently here:\n")
		for _, n := range cand.Compat.Notices {
			c.errf("  - %s\n", n)
		}
	}
	c.errf("\nIt would be allowed to:\n")
	for _, l := range permissionLines(sk) {
		c.errf("  %s\n", l)
	}
	c.errf("\nThe folder is copied as it is; nothing in it is rewritten.\n")
	if !c.ask("\nImport it?", false) {
		c.errf("not imported (answer y in a terminal to import)\n")
		return ExitError
	}
	got, err := a.InstallSkill(ctx, cand.Dir, "imported from "+cand.Runtime+": "+cand.Dir)
	if err != nil {
		return c.fail(err)
	}
	c.outf("✓ imported %s (copied to your skills folder; `noskill skills show %s` to see it)\n", got.Name, got.Name)
	return ExitOK
}
