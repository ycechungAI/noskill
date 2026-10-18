package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"noskill/internal/secrets"
)

func cmdSecrets(c *cmd, args []string) int {
	if len(args) == 0 {
		return c.usageErr("usage: noskill secrets set|rm|list [NAME]")
	}
	if err := c.load(false); err != nil {
		return c.fail(err)
	}
	m := c.secretStore()
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("secrets set", flag.ContinueOnError)
		fromStdin := fs.Bool("stdin", false, "read the value from standard input")
		pos, err := c.parse(fs, args[1:])
		if err != nil || len(pos) != 1 {
			return c.usageErr("usage: noskill secrets set NAME [--stdin]   (the value is never taken from the command line)")
		}
		if err := secrets.ValidName(pos[0]); err != nil {
			return c.usageErr("%v", err)
		}
		val, err := c.secretValue(pos[0], *fromStdin)
		if err != nil {
			return c.fail(err)
		}
		if err := m.Set(pos[0], val); err != nil {
			return c.fail(err)
		}
		c.outf("stored %s\n", pos[0])
	case "rm":
		if len(args) != 2 {
			return c.usageErr("usage: noskill secrets rm NAME")
		}
		if err := m.Delete(args[1]); err != nil {
			if errors.Is(err, secrets.ErrNotFound) {
				return c.fail(fmt.Errorf("no secret named %s", args[1]))
			}
			return c.fail(err)
		}
		c.outf("removed %s\n", args[1])
	case "list":
		names, err := m.List()
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(map[string]any{"secrets": names})
			return ExitOK
		}
		for _, n := range names {
			c.outf("%s\n", n)
		}
		if len(names) == 0 {
			c.infof("no secrets stored. Config files refer to them as \"secret:NAME\".\n")
		}
	default:
		return c.usageErr("unknown secrets command %q (set, rm, list)", args[0])
	}
	return ExitOK
}

// secretValue reads a secret without echoing it, or from standard input when piped. It never
// comes from an argument, where `ps` and shell history would keep it.
func (c *cmd) secretValue(name string, fromStdin bool) (string, error) {
	if c.env.StdinTTY && !fromStdin && c.env.StdinFD >= 0 {
		fmt.Fprintf(c.env.Stderr, "Value for %s (input is hidden): ", name)
		b, err := term.ReadPassword(c.env.StdinFD)
		fmt.Fprintln(c.env.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if c.env.StdinTTY && !fromStdin {
		return "", errors.New("can't hide input on this terminal; pipe the value in with --stdin")
	}
	b, err := io.ReadAll(io.LimitReader(c.in, 1<<20))
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", errors.New("no value on standard input")
	}
	return v, nil
}

var _ = os.Getenv
