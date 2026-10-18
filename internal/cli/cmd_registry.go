package cli

import (
	"flag"
	"strconv"
	"strings"
	"time"

	"noskill/internal/registry"
)

// cmdRegistry is `noskill registry` (FR-S10, ADR-0016): your signing key, publishing a registry
// folder, and the registries you trust.
func cmdRegistry(c *cmd, args []string) int {
	usage := "usage: noskill registry key new [--replace] | key show | publish <folder> [--name n] | add <name> <folder|git-url> --key <public key> | list | remove <name> | update [name] | search <words>"
	if len(args) == 0 {
		return c.usageErr(usage)
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "key":
		if len(args) < 2 {
			return c.usageErr("usage: noskill registry key new [--replace] | key show")
		}
		switch args[1] {
		case "new":
			fs := flag.NewFlagSet("registry key new", flag.ContinueOnError)
			replace := fs.Bool("replace", false, "replace the key you have (everyone must add your registry again)")
			if rest, err := c.parse(fs, args[2:]); err != nil || len(rest) != 0 {
				return c.usageErr("usage: noskill registry key new [--replace]")
			}
			if *replace && !c.ask("Replace your registry key? Skills and registries signed with the old one will show as signed by an unknown key.", false) {
				c.errf("key kept (answer y in a terminal to replace it)\n")
				return ExitError
			}
			pub, err := a.RegistryKeyNew(*replace)
			if err != nil {
				return c.fail(err)
			}
			c.outf("Made a signing key %s; its private half is in the OS keychain.\nShare this public key with people who add your registry:\n  %s\n", registry.KeyID(pub), registry.EncodePublicKey(pub))
		case "show":
			pub, err := a.RegistryPublicKey()
			if err != nil {
				return c.fail(err)
			}
			if c.g.json {
				c.printJSON(map[string]string{"key": registry.EncodePublicKey(pub), "key_id": registry.KeyID(pub)})
				return ExitOK
			}
			c.outf("key %s\n  %s\n", registry.KeyID(pub), registry.EncodePublicKey(pub))
		default:
			return c.usageErr("usage: noskill registry key new [--replace] | key show")
		}
	case "publish":
		fs := flag.NewFlagSet("registry publish", flag.ContinueOnError)
		name := fs.String("name", "", "the registry's name (default: the one already there, or the folder's name)")
		rest, err := c.parse(fs, args[1:])
		if err != nil || len(rest) != 1 {
			return c.usageErr("usage: noskill registry publish <folder> [--name n]")
		}
		p, err := a.RegistryPublish(rest[0], *name)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(p)
			return ExitOK
		}
		for _, s := range p.Skipped {
			c.infof("skipped %s\n", s)
		}
		rows := [][]string{{"SKILL", "VERSION", "PATH"}}
		for _, e := range p.Index.Entries {
			rows = append(rows, []string{e.Name, orDash(e.Version), e.Path})
		}
		c.table(rows)
		c.outf("Signed %d skills and wrote %s for registry %s (key %s). Share the folder, for example by pushing it to a git host.\n",
			len(p.Index.Entries), registry.IndexFile, p.Index.Name, p.Index.KeyID)
	case "add":
		fs := flag.NewFlagSet("registry add", flag.ContinueOnError)
		key := fs.String("key", "", "the registry's public key, from its publisher (`noskill registry key show`)")
		rest, err := c.parse(fs, args[1:])
		if err != nil || len(rest) != 2 || *key == "" {
			return c.usageErr("usage: noskill registry add <name> <folder|git-url> --key <public key>")
		}
		c.infof("fetching %s …\n", rest[1])
		info, err := a.RegistryAdd(ctx, rest[0], rest[1], *key)
		if err != nil {
			return c.fail(err)
		}
		c.outf("Added registry %s: %d skills, signed by key %s. `noskill registry search <words>` searches it.\n", info.Name, len(info.Index.Entries), info.KeyID)
	case "list":
		infos, err := a.RegistryInfos()
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"registries": infos})
			return ExitOK
		}
		if len(infos) == 0 {
			c.outf("No registries. `noskill registry add <name> <folder|git-url> --key <key>` adds one.\n")
			return ExitOK
		}
		rows := [][]string{{"NAME", "KEY", "SKILLS", "UPDATED", "FROM"}}
		for _, r := range infos {
			n, at := "-", "-"
			if r.Index != nil {
				n, at = strconv.Itoa(len(r.Index.Entries)), r.Fetched.Local().Format(time.DateTime)
			}
			rows = append(rows, []string{r.Name, r.KeyID, n, at, r.Where})
		}
		c.table(rows)
	case "remove":
		if len(args) != 2 {
			return c.usageErr("usage: noskill registry remove <name>")
		}
		if err := a.RegistryRemove(args[1]); err != nil {
			return c.fail(err)
		}
		c.outf("Removed registry %s; skills installed from it stay installed. Their signatures now show as an unknown key.\n", args[1])
	case "update":
		if len(args) > 2 {
			return c.usageErr("usage: noskill registry update [name]")
		}
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		us, err := a.RegistryUpdate(ctx, name)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"registries": us})
			return ExitOK
		}
		failed := false
		for _, u := range us {
			if u.Err != "" {
				failed = true
				c.errf("✗ %s\n", u.Err)
				continue
			}
			c.outf("✓ %s: %d skills (published %s)\n", u.Name, u.Skills, u.Made.Local().Format(time.DateTime))
		}
		if len(us) == 0 {
			c.outf("No registries to update.\n")
		}
		if failed {
			return ExitError
		}
	case "search":
		hits, err := a.RegistrySearch(args[1:])
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"skills": hits})
			return ExitOK
		}
		if len(hits) == 0 {
			c.outf("Nothing found. `noskill registry update` fetches the latest indexes.\n")
			return ExitOK
		}
		rows := [][]string{{"SKILL", "VERSION", "DESCRIPTION", "PERMISSIONS"}}
		for _, h := range hits {
			rows = append(rows, []string{h.Registry + "/" + h.Name, orDash(h.Version), clipCell(h.Description, 60), strings.Join(h.Permissions, "; ")})
		}
		c.table(rows)
		c.outf("`noskill skills install <registry>/<skill>` installs one.\n")
	default:
		return c.usageErr("unknown registry command %q (key, publish, add, list, remove, update, search)", args[0])
	}
	return ExitOK
}
