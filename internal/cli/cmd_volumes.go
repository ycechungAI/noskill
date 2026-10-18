package cli

import (
	"noskill/internal/platform"
)

// cmdVolumes lists attached drives and the label → UUID bindings noskill keeps for volume:<label>
// paths and --when volume:<label> (PLATFORMS.md "External drives"). `forget` drops a binding after
// the user replaces a drive, so the next drive with that label is bound instead.
func cmdVolumes(c *cmd, args []string) int {
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	b := a.Volumes()
	if len(args) == 2 && args[0] == "forget" {
		n, err := b.Forget(ctx, args[1])
		if err != nil {
			return c.fail(err)
		}
		if n == 0 {
			return c.usageErr("no drive is bound to %q (see `noskill volumes`)", args[1])
		}
		c.outf("✓ forgot %s; the next drive called %s is bound when noskill first uses it\n", args[1], args[1])
		return ExitOK
	}
	if len(args) != 0 {
		return c.usageErr("usage: noskill volumes [forget <label>]")
	}
	mounted, _ := platform.ListVolumes()
	known, err := b.Known(ctx)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		c.printJSON(map[string]any{"mounted": mounted, "bound": known})
		return ExitOK
	}
	bound := map[string]bool{}
	for _, k := range known {
		bound[k.Label] = true
	}
	rows := [][]string{{"LABEL", "MOUNTED AT", "FILESYSTEM", "FREE", "BOUND"}}
	seen := map[string]bool{}
	for _, v := range mounted {
		seen[v.Label] = true
		state := "-"
		if bound[v.Label] {
			if _, err := b.Mount(ctx, v.Label, ""); err != nil {
				state = "another drive"
			} else {
				state = "yes"
			}
		}
		rows = append(rows, []string{v.Label, v.Mount, v.FSType, humanBytes(v.FreeBytes), state})
	}
	for _, k := range known {
		if !seen[k.Label] {
			rows = append(rows, []string{k.Label, "not mounted (last " + k.LastPath + ")", k.FSType, "-", "yes"})
		}
	}
	if len(rows) == 1 {
		c.outf("no drives attached and none bound yet\n")
		return ExitOK
	}
	c.table(rows)
	return ExitOK
}
