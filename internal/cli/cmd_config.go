package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"noskill/internal/config"
)

func cmdConfig(c *cmd, args []string) int {
	if len(args) == 0 {
		return c.usageErr("usage: noskill config check|get|set|edit|path")
	}
	switch args[0] {
	case "check":
		return configCheck(c, args[1:])
	case "get":
		return configGet(c, args[1:])
	case "set":
		return configSet(c, args[1:])
	case "edit":
		return configEdit(c)
	case "path":
		if err := c.load(false); err != nil {
			return c.fail(err)
		}
		c.outf("%s\n", c.dirs.ConfigFile())
		return ExitOK
	}
	return c.usageErr("unknown config command %q (check, get, set, edit, path)", args[0])
}

func configCheck(c *cmd, args []string) int {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	explain := fs.Bool("explain", false, "show every value and the layer that set it")
	if _, err := c.parse(fs, args); err != nil {
		return ExitUsage
	}
	if err := c.load(true); err != nil {
		return c.fail(err)
	}
	l := c.loaded
	issues := config.Check(l, secretChecker{c})
	type value struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		Source string `json:"source"`
	}
	var values []value
	if *explain {
		for _, k := range config.LeafKeys() {
			v, _ := config.Get(l.Config, k)
			src := l.Sources[k]
			if src == "" {
				src = config.LayerDefault
			}
			values = append(values, value{k, v, string(src)})
		}
	}
	ok := !config.HasErrors(issues)
	if c.g.json {
		c.printJSON(map[string]any{"files": l.Files, "issues": issues, "values": values, "ok": ok})
	} else {
		if len(l.Files) == 0 {
			c.outf("no config file; using defaults\n")
		} else {
			c.outf("read: %s\n", strings.Join(l.Files, ", "))
		}
		for _, v := range values {
			c.outf("%s = %s  (%s)\n", v.Key, v.Value, v.Source)
		}
		for _, i := range issues {
			c.outf("%s\n", i)
		}
		if ok {
			c.outf("ok\n")
		}
	}
	if !ok {
		return ExitError
	}
	return ExitOK
}

type secretChecker struct{ c *cmd }

func (s secretChecker) Has(name string) (bool, error) { return s.c.secretStore().Has(name) }

func configGet(c *cmd, args []string) int {
	if len(args) != 1 {
		return c.usageErr("usage: noskill config get <key>")
	}
	if err := c.load(true); err != nil {
		return c.fail(err)
	}
	key := args[0]
	if v, ok := config.Get(c.loaded.Config, key); ok {
		src := c.loaded.Sources[key]
		if src == "" {
			src = config.LayerDefault
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u // strings and lists print bare, so scripts can use them
		}
		if c.g.json {
			c.printJSON(map[string]string{"key": key, "value": v, "source": string(src)})
		} else {
			c.outf("%s\n", v)
			c.infof("(%s)\n", src)
		}
		return ExitOK
	}
	if rest, ok := strings.CutPrefix(key, "skills."); ok {
		if skill, k, found := strings.Cut(rest, "."); found {
			if v, ok := c.loaded.Config.Skills.Settings[skill][k]; ok {
				c.outf("%v\n", v)
				return ExitOK
			}
		}
	}
	var near []string
	for _, k := range config.LeafKeys() {
		if strings.HasPrefix(k, strings.SplitN(key, ".", 2)[0]+".") {
			near = append(near, k)
		}
	}
	msg := fmt.Sprintf("unknown key %q", key)
	if len(near) > 0 {
		msg += " (keys in that section: " + strings.Join(near, ", ") + ")"
	}
	return c.usageErr("%s", msg)
}

func configSet(c *cmd, args []string) int {
	if len(args) != 2 {
		return c.usageErr("usage: noskill config set <key> <value>   (lists: comma-separated)")
	}
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	key, raw := args[0], args[1]
	var literal string
	if rest, ok := strings.CutPrefix(key, "skills."); ok && strings.Contains(rest, ".") {
		literal = looseLiteral(raw) // [skills.<name>] settings are free-form
	} else {
		scratch := c.loaded.Config
		if err := config.Set(&scratch, key, raw); err != nil {
			return c.usageErr("%v", err)
		}
		var ok bool
		if literal, ok = config.Literal(scratch, key); !ok {
			return c.usageErr("%s can only be set by editing config.toml (`noskill config edit`)", key)
		}
	}
	section, k, err := config.SplitKey(key)
	if err != nil {
		return c.usageErr("%v", err)
	}
	path := c.dirs.ConfigFile()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return c.fail(err)
	}
	updated := config.EditText(string(old), section, k, literal)
	// Check the result before it replaces the user's file.
	tmp := path + ".new"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return c.fail(err)
	}
	if err := os.WriteFile(tmp, []byte(updated), 0o600); err != nil {
		return c.fail(err)
	}
	defer os.Remove(tmp) // our own scratch file
	l, err := config.Load(config.Options{File: tmp})
	if err == nil {
		if issues := config.Check(l, nil); config.HasErrors(issues) {
			var msgs []string
			for _, i := range issues {
				if i.Level == "error" {
					msgs = append(msgs, i.String())
				}
			}
			err = fmt.Errorf("%s\n  The file was not changed.", strings.Join(msgs, "\n  "))
		}
	}
	if err != nil {
		return c.fail(err)
	}
	if len(old) > 0 {
		os.WriteFile(path+".prev", old, 0o600) // one step of undo
	}
	if err := os.Rename(tmp, path); err != nil {
		return c.fail(err)
	}
	c.outf("%s = %s\n", key, literal)
	return ExitOK
}

// looseLiteral formats a free-form value: booleans and numbers stay bare, everything else is a string.
func looseLiteral(raw string) string {
	if raw == "true" || raw == "false" {
		return raw
	}
	if _, err := strconv.ParseFloat(raw, 64); err == nil {
		return raw
	}
	return strconv.Quote(raw)
}

func configEdit(c *cmd) int {
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	path := c.dirs.ConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return c.fail(err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
			return c.fail(err)
		}
	}
	editor := ""
	for _, v := range []string{"VISUAL", "EDITOR"} {
		for _, e := range c.environ() {
			if val, ok := strings.CutPrefix(e, v+"="); ok && val != "" {
				editor = val
			}
		}
		if editor != "" {
			break
		}
	}
	if editor == "" {
		editor = map[string]string{"windows": "notepad", "darwin": "nano"}[runtime.GOOS]
		if editor == "" {
			editor = "nano"
		}
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.env.Stdout, c.env.Stderr
	if err := cmd.Run(); err != nil {
		return c.fail(fmt.Errorf("couldn't run %q: %w (set $EDITOR, or edit %s yourself)", editor, err, path))
	}
	c.loaded = nil
	return configCheck(c, nil)
}

// starterConfig is what `config edit` and `init` create when there's no file.
const starterConfig = `# noskill config. Only list what you want to change; everything else keeps its default.
# Reference: docs/CONFIG.md

[[backends]]
kind = "ollama"
url = "http://127.0.0.1:11434"

# [orchestrator]
# preset = "balanced"        # fast | balanced | best

# [[orchestrator.rules]]
# when = { task_type = "code" }
# use = "qwen2.5-coder:7b"
`
