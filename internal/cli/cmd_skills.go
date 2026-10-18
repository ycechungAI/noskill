package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"noskill/internal/app"
	"noskill/internal/learning"
	"noskill/internal/skills"
	"noskill/internal/tools"
)

func cmdSkills(c *cmd, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	rest := args[1:]
	switch args[0] {
	case "list":
		return skillsList(c, ctx, a, rest)
	case "show":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills show <name>")
		}
		return skillsShow(c, ctx, a, rest[0])
	case "enable":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills enable <name>")
		}
		return skillsEnable(c, ctx, a, rest[0])
	case "disable":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills disable <name>")
		}
		if _, ok := a.Skills.Get(rest[0]); !ok {
			return c.usageErr("no skill named %q", rest[0])
		}
		if err := a.DB.SetSkillEnabled(ctx, rest[0], false); err != nil {
			return c.fail(err)
		}
		c.outf("disabled %s\n", rest[0])
	case "remove":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills remove <name>")
		}
		if !c.ask(fmt.Sprintf("Move skill %s to the trash?", rest[0]), false) {
			c.errf("nothing removed (answer y in a terminal to confirm)\n")
			return ExitError
		}
		if err := a.RemoveSkill(ctx, rest[0]); err != nil {
			return c.fail(err)
		}
		c.outf("removed %s (its folder is in the trash)\n", rest[0])
	case "import":
		return skillsImport(c, ctx, a, rest)
	case "install":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills install <folder|git-url|registry/skill>")
		}
		return skillsInstall(c, ctx, a, rest[0])
	case "sign":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills sign <folder>")
		}
		sig, err := a.SignSkill(rest[0])
		if err != nil {
			return c.fail(err)
		}
		c.outf("signed %s %s with key %s (%d files)\n", sig.Name, orDefault(sig.Version, "(no version)"), sig.KeyID, len(sig.Files))
	case "verify":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills verify <name>")
		}
		sc, err := a.VerifySkill(rest[0])
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(sc)
			if sc.Err != "" {
				return ExitError
			}
			return ExitOK
		}
		switch {
		case !sc.Signed:
			c.outf("%s isn't signed, so there's nothing to check it against\n", rest[0])
		case sc.Err != "":
			c.errf("✗ %s: %s\n", rest[0], sc.Err)
			return ExitError
		default:
			c.outf("✓ %s matches its signature (%s)\n", rest[0], signedBy(sc))
		}
	case "prepare":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills prepare <name>")
		}
		return skillsPrepare(c, ctx, a, rest[0])
	case "versions":
		if len(rest) != 1 {
			return c.usageErr("usage: noskill skills versions <name>")
		}
		return skillsVersions(c, a, rest[0])
	case "diff":
		if len(rest) < 1 || len(rest) > 3 {
			return c.usageErr("usage: noskill skills diff <name> [from-version [to-version]]")
		}
		var from, to string
		if len(rest) > 1 {
			from = rest[1]
		}
		if len(rest) > 2 {
			to = rest[2]
		}
		d, err := a.DiffSkill(rest[0], from, to)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"diff": d})
		} else if d == "" {
			c.outf("no differences\n")
		} else {
			c.outf("%s", d)
		}
	case "optimise", "optimize":
		return skillsOptimise(c, ctx, a, rest)
	case "rollback":
		if len(rest) < 1 || len(rest) > 2 {
			return c.usageErr("usage: noskill skills rollback <name> [version]")
		}
		v := ""
		if len(rest) == 2 {
			v = rest[1]
		}
		got, err := a.RollbackSkill(ctx, rest[0], v)
		if err != nil {
			return c.fail(err)
		}
		c.outf("%s is back at version %s (the newer one is kept; `noskill skills versions %s` lists them)\n", rest[0], got, rest[0])
	default:
		return c.usageErr("unknown skills command %q (list, show, enable, disable, remove, install, sign, verify, prepare, versions, diff, rollback, optimise)", args[0])
	}
	return ExitOK
}

func skillsList(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("skills list", flag.ContinueOnError)
	learned := fs.Bool("learned", false, "only skills noskill learned")
	status := fs.String("status", "", "only skills with this status")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	usage, _ := a.DB.SkillUsage(ctx)
	enabled, _ := a.DB.EnabledSkills(ctx)
	type row struct {
		Name    string  `json:"name"`
		Origin  string  `json:"origin"`
		Status  string  `json:"status"`
		Version string  `json:"version"`
		From    string  `json:"from"`
		Enabled bool    `json:"enabled"`
		Uses    int     `json:"uses"`
		Success float64 `json:"success_rate,omitempty"`
	}
	var rows []row
	for _, s := range a.Skills.All() {
		if (*learned && s.Origin() != "learned") || (*status != "" && s.Status() != *status) {
			continue
		}
		u := usage[s.Name]
		r := row{s.Name, s.Origin(), s.Status(), s.Meta.Version, string(s.Source), enabled[s.Name], u.Uses, 0}
		if n := a.TunedVersion(s.Name); n > 0 && s.Source == skills.SourceBuiltin {
			r.From = fmt.Sprintf("builtin (tuned %d)", n) // ADR-0014
		}
		if u.Rated > 0 {
			r.Success = float64(u.Success) / float64(u.Rated)
		}
		rows = append(rows, r)
	}
	if c.g.json {
		c.printJSON(map[string]any{"skills": rows})
		return ExitOK
	}
	t := [][]string{{"SKILL", "ORIGIN", "STATUS", "VERSION", "FROM", "ENABLED", "USES", "SUCCESS"}}
	for _, r := range rows {
		en, succ := "yes", "-"
		if !r.Enabled {
			en = "no"
			if r.From == "project" {
				en = "no (needs your OK)"
			}
		}
		if r.Success > 0 || r.Uses > 0 {
			if u := usage[r.Name]; u.Rated > 0 {
				succ = fmt.Sprintf("%.0f%% of %d", 100*r.Success, u.Rated)
			}
		}
		t = append(t, []string{r.Name, r.Origin, r.Status, orDefault(r.Version, "-"), r.From, en, fmt.Sprint(r.Uses), succ})
	}
	c.table(t)
	for _, sh := range a.Skills.Shadows {
		c.infof("note: a %s skill named %s was ignored; the %s one is used\n", sh.Skipped, sh.Name, sh.Kept)
	}
	return ExitOK
}

// skillsVersions lists a learned skill's versions (FR-S9).
func skillsVersions(c *cmd, a *app.App, name string) int {
	vs, err := a.SkillVersions(name)
	if err != nil {
		return c.fail(err)
	}
	if c.g.json {
		type row struct {
			Version string `json:"version"`
			State   string `json:"state,omitempty"`
			Path    string `json:"path"`
		}
		var rows []row
		for _, v := range vs {
			rows = append(rows, row{v.Version, versionState(v), v.Dir})
		}
		c.printJSON(map[string]any{"versions": rows})
		return ExitOK
	}
	t := [][]string{{"VERSION", "", "DATE"}}
	for _, v := range vs {
		t = append(t, []string{v.Version, versionState(v), v.Time.Format("2006-01-02 15:04")})
	}
	c.table(t)
	return ExitOK
}

func versionState(v learning.Version) string {
	switch {
	case v.Current:
		return "current"
	case v.Next:
		return "on trial"
	}
	return ""
}

// permissionLines describes what a skill may do, in words. exec and wide network access are marked.
func permissionLines(s *skills.Skill) []string {
	p := s.Meta.Permissions
	var out []string
	switch {
	case len(p.Network) == 0:
		out = append(out, "network     none")
	default:
		line := "network     " + strings.Join(p.Network, ", ")
		for _, n := range p.Network {
			if n == "*" {
				line += "   ← any website"
			}
		}
		out = append(out, line)
	}
	if len(p.Filesystem) == 0 {
		out = append(out, "files       none declared (a skill that brings this one in grants folders; you can grant them with --allow)")
	} else {
		for i, f := range p.Filesystem {
			label := "            "
			if i == 0 {
				label = "files       "
			}
			out = append(out, label+f)
		}
	}
	out = append(out, "exec        "+yesNo(p.Exec, "yes   ← can run any program on this computer"))
	out = append(out, "subprocess  "+yesNo(p.Subprocess, "yes   ← scripts may start other programs"))
	var sec []string
	for _, x := range p.SecretList() {
		n := x.Name
		if x.Optional {
			n += " (optional)"
		}
		sec = append(sec, n)
	}
	if len(sec) > 0 {
		out = append(out, "secrets     "+strings.Join(sec, ", "))
	}
	return out
}

func yesNo(b bool, yes string) string {
	if b {
		return yes
	}
	return "no"
}

func skillsShow(c *cmd, ctx context.Context, a *app.App, name string) int {
	s, ok := a.Skills.Get(name)
	if !ok {
		return c.usageErr("no skill named %q (see `noskill skills list`)", name)
	}
	enabled, _ := a.DB.EnabledSkills(ctx)
	usage, _ := a.DB.SkillUsage(ctx)
	if c.g.json {
		c.printJSON(map[string]any{"name": s.Name, "description": s.Description, "version": s.Meta.Version, "origin": s.Origin(), "status": s.Status(),
			"from": s.Source, "path": s.Dir, "enabled": enabled[s.Name], "permissions": s.Meta.Permissions, "confirm": s.Meta.Confirm,
			"depends_on": s.Meta.DependsOn, "triggers": s.Meta.Triggers, "hash": s.Hash})
		return ExitOK
	}
	state := "enabled"
	if !enabled[s.Name] {
		state = "disabled"
	}
	c.outf("%s %s   (%s · %s · %s)\n", s.Name, s.Meta.Version, s.Origin(), s.Status(), state)
	c.outf("  %s\n", s.Description)
	if s.Dir != "" {
		c.outf("  folder: %s\n", s.Dir)
	}
	if s.NeedsApproval() && !enabled[s.Name] {
		c.outf("\n! This skill comes from the folder you're in, not from you. It can't run until you review it\n  and run `noskill skills enable %s`. If its files change, that approval is withdrawn.\n", s.Name)
	}
	c.outf("\nPermissions\n")
	for _, l := range permissionLines(s) {
		c.outf("  %s\n", l)
	}
	if len(s.Tools) > 0 {
		c.outf("\nTools\n")
		for _, t := range s.Tools {
			conf := ""
			for _, cn := range s.Meta.Confirm {
				if cn == t.Name {
					conf = "   asks you first"
				}
			}
			c.outf("  %-16s %s%s\n", t.Name, t.Runtime+" "+t.Entry, conf)
		}
	}
	if len(s.Meta.DependsOn) > 0 {
		c.outf("\nUses skills: %s\n", strings.Join(s.Meta.DependsOn, ", "))
	}
	if u := usage[s.Name]; u.Uses > 0 {
		c.outf("\nUsed in %d run(s)", u.Uses)
		if u.Rated > 0 {
			c.outf(", %d%% of %d with a verdict went well", 100*u.Success/u.Rated, u.Rated)
		}
		c.outf("\n")
	}
	return ExitOK
}

func skillsEnable(c *cmd, ctx context.Context, a *app.App, name string) int {
	s, ok := a.Skills.Get(name)
	if !ok {
		return c.usageErr("no skill named %q", name)
	}
	if s.Source == skills.SourceLearned && (s.Status() == "stale" || s.Status() == "archived") {
		if err := a.RestoreLearned(ctx, name); err != nil {
			return c.fail(err)
		}
		c.outf("restored %s (it was %s)\n", name, s.Status())
	}
	enabled, _ := a.DB.EnabledSkills(ctx)
	if enabled[name] && s.Source == skills.SourceLearned && s.Status() != "active" {
		return ExitOK // restored above
	}
	if enabled[name] {
		c.outf("%s is already enabled\n", name)
		return ExitOK
	}
	if s.NeedsApproval() || s.Meta.Permissions.Exec {
		c.errf("Enabling %s lets it:\n", name)
		for _, l := range permissionLines(s) {
			c.errf("  %s\n", l)
		}
		if !c.ask("Enable it?", false) {
			c.errf("not enabled (a skill like this can only be enabled from a terminal, by you)\n")
			return ExitError
		}
	}
	if err := a.DB.SetSkillEnabled(ctx, name, true); err != nil {
		return c.fail(err)
	}
	c.outf("enabled %s\n", name)
	return ExitOK
}

func looksLikeURL(s string) bool { return app.IsGitURL(s) }

// signedBy says whose a valid signature is (FR-S10).
func signedBy(sc app.SigCheck) string {
	if sc.Trusted {
		return fmt.Sprintf("signed by %s, key %s", sc.By, sc.KeyID)
	}
	return fmt.Sprintf("signed by an unknown key %s, so treated as unsigned; `noskill registry add` trusts a registry's key", sc.KeyID)
}

func skillsInstall(c *cmd, ctx context.Context, a *app.App, src string) int {
	dir, from, signed := src, "", ""
	if reg, name, ok := a.RegistryRef(src); ok {
		c.infof("fetching %s …\n", src)
		p, err := a.RegistryFetchSkill(ctx, reg, name)
		if err != nil {
			return c.fail(err)
		}
		defer p.Close()
		dir, from = p.Dir, p.From
		signed = fmt.Sprintf("signed by registry %s (key %s)", reg, p.Registry.KeyID)
	} else if !looksLikeURL(src) {
		abs, err := filepath.Abs(src)
		if err != nil {
			return c.fail(err)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return c.usageErr("%s isn't a folder, a git URL or <registry>/<skill> for a registry you added", src)
		}
		dir = abs
	} else {
		c.infof("downloading %s …\n", src)
		d, commit, done, err := app.GitFetch(ctx, src)
		if err != nil {
			return c.fail(err)
		}
		defer done()
		dir = d
		from = src + "@" + commit // pinned to the commit that was reviewed
	}
	sk, issues := app.LoadSkillDir(dir)
	if sk == nil {
		msg := "not a valid skill"
		if len(issues) > 0 {
			msg = issues[len(issues)-1].String()
		}
		return c.usageErr("%s", msg)
	}
	if signed == "" {
		switch sc := a.CheckSignature(dir, sk); {
		case sc.Err != "":
			return c.fail(&app.UserError{Msg: fmt.Sprintf("%s has a signature that doesn't check out, so it isn't installed: %s", sk.Name, sc.Err)})
		case sc.Signed:
			signed = signedBy(sc)
		default:
			signed = "not signed"
		}
	}
	c.errf("Installing skill %s %s\n  %s\n  %s\n\nIt would be allowed to:\n", sk.Name, sk.Meta.Version, sk.Description, signed)
	for _, l := range permissionLines(sk) {
		c.errf("  %s\n", l)
	}
	if len(sk.Tools) > 0 {
		c.errf("\nTools: ")
		var names []string
		for _, t := range sk.Tools {
			names = append(names, t.Name+" ("+t.Runtime+")")
		}
		c.errf("%s\n", strings.Join(names, ", "))
	}
	if !c.ask("\nInstall it?", false) {
		c.errf("not installed (answer y in a terminal to install)\n")
		return ExitError
	}
	got, err := a.InstallSkill(ctx, dir, from)
	if err != nil {
		return c.fail(err)
	}
	c.outf("✓ installed %s%s\n", got.Name, map[bool]string{true: " from " + from, false: ""}[from != ""])
	if hasDeps(got) {
		c.outf("  It needs Python packages: run `noskill skills prepare %s`\n", got.Name)
	}
	return ExitOK
}

func hasDeps(s *skills.Skill) bool {
	for _, t := range s.Tools {
		if len(t.Deps) > 0 {
			return true
		}
	}
	return false
}

func skillsPrepare(c *cmd, ctx context.Context, a *app.App, name string) int {
	s, ok := a.Skills.Get(name)
	if !ok {
		return c.usageErr("no skill named %q", name)
	}
	var deps []string
	for _, t := range s.Tools {
		deps = append(deps, t.Deps...)
	}
	if len(deps) == 0 {
		c.outf("%s needs no extra packages\n", name)
		return ExitOK
	}
	c.errf("This downloads %s from the Python package index into noskill's own folder:\n  %s\n", name, strings.Join(deps, "\n  "))
	if !c.ask("Go ahead?", false) {
		c.errf("nothing downloaded (answer y in a terminal)\n")
		return ExitError
	}
	made, err := a.Interp.Prepare(ctx, s)
	if err != nil {
		return c.fail(err)
	}
	if len(made) == 0 {
		c.outf("already prepared\n")
	} else {
		c.outf("✓ prepared %s\n", name)
	}
	return ExitOK
}

var _ = tools.ErrDeclined
