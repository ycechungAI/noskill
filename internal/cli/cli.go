// Package cli is the noskill command line (CLI.md, FR-I1). Main takes its streams and environment
// as arguments so commands can be tested without a terminal.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"noskill/internal/app"
	"noskill/internal/config"
	"noskill/internal/platform"
	"noskill/internal/secrets"
	"noskill/internal/service"
)

// Exit codes (CLI.md "Conventions").
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitWaiting     = 3 // waiting for an approval
	ExitBudget      = 4 // a step or token budget ran out
	ExitUnavailable = 5 // no backend answered
)

// Env is everything the CLI takes from its surroundings.
type Env struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	Environ   []string
	Getwd     func() (string, error)
	StdinTTY  bool // a person is typing: prompts are allowed
	StdinFD   int  // file descriptor of Stdin when it is a terminal (for no-echo input), else -1
	StdoutTTY bool
	Version   string
	// Context returns a context that ends on Ctrl-C / SIGTERM.
	Context func() (context.Context, context.CancelFunc)
	// Keychain overrides the OS keychain (tests).
	Keychain secrets.Store
	// Overrides for tests.
	Notifier platform.Notifier
	Sandbox  platform.Sandbox
	Trash    platform.Trasher
	Service  service.Manager // nil = the real service manager for this OS
	// Executable returns the running binary's path (tests point it at a scratch copy).
	Executable func() (string, error)
}

type globals struct {
	home    string
	json    bool
	verbose bool
	quiet   bool
}

type command struct {
	name    string
	summary string
	run     func(c *cmd, args []string) int
}

type cmd struct {
	env     Env
	g       globals
	dirs    platform.Dirs
	loaded  *config.Loaded
	secrets *secrets.Manager
	in      *bufio.Reader
}

var commands []command

func init() {
	commands = []command{
		{"init", "detect backends and models, write a starting config", cmdInit},
		{"doctor", "check folders, config, database, backends, memory, sandbox", cmdDoctor},
		{"run", "run one task", cmdRun},
		{"chat", "talk to noskill in the terminal", cmdChat},
		{"rate", "rate a run: good, partial or bad", cmdRate},
		{"learn", "draft a skill from a run (\"remember how to do this\"); learn queue", cmdLearn},
		{"undo", "revert the file changes of a run", cmdUndo},
		{"skills", "list, show, enable, disable, install, remove, diff and roll back skills", cmdSkills},
		{"approvals", "list, approve or skip things waiting for you", cmdApprovals},
		{"schedule", "add, list, pause, resume, remove or run scheduled jobs", cmdSchedule},
		{"remind", "set a reminder from a sentence (\"call Sam tomorrow at 5pm\"); list, rm", cmdRemind},
		{"brief", "what happened since yesterday: jobs, episodes filed, approvals, disk space", cmdBrief},
		{"jobs", "show running jobs, stop them, or list past runs", cmdJobs},
		{"models", "list, probe and benchmark models", cmdModels},
		{"route", "explain which model would handle a prompt", cmdRoute},
		{"eval", "run the checked tasks, compare runs, pin your own tasks", cmdEval},
		{"decide", "make a raw decision call (debugging)", cmdDecide},
		{"calibration", "how well decision confidences match outcomes; tune or revert thresholds", cmdCalibration},
		{"memory", "list, add or forget what noskill remembers", cmdMemory},
		{"volumes", "list external drives and the ones noskill is bound to", cmdVolumes},
		{"docs", "add folders noskill may read, index them, and search them", cmdDocs},
		{"anime", "find your torrent app, suggest new anime, download and file approved shows", cmdAnime},
		{"hook", "run by a torrent app when a download finishes: file that torrent now", cmdHook},
		{"registry", "sign skills, publish a registry, add and search registries you trust", cmdRegistry},
		{"traces", "list, show or purge run history", cmdTraces},
		{"train", "export good runs as a dataset, train a LoRA on it, adopt or revert the result", cmdTrain},
		{"config", "get, set, edit or check config.toml", cmdConfig},
		{"secrets", "store API keys and tokens in the OS keychain", cmdSecrets},
		{"serve", "run the OpenAI- and Ollama-compatible server", cmdServe},
		{"ui", "open the status page: approvals, downloads, history (starts a page-only server if none is running)", cmdUI},
		{"daemon", "install and control the background service", cmdDaemon},
		{"update", "check for a new version and install it", cmdUpdate},
		{"uninstall", "remove the service and the binary noskill installed", cmdUninstall},
		{"version", "print the version", cmdVersion},
	}
}

// Main runs the CLI and returns its exit code.
func Main(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	if env.Getwd == nil {
		env.Getwd = os.Getwd
	}
	if env.Context == nil {
		env.Context = func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }
	}
	c := &cmd{env: env, in: bufio.NewReader(env.Stdin)}
	fs := flag.NewFlagSet("noskill", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.g.home, "home", "", "")
	fs.BoolVar(&c.g.json, "json", false, "")
	fs.BoolVar(&c.g.verbose, "verbose", false, "")
	fs.BoolVar(&c.g.quiet, "quiet", false, "")
	fs.Bool("no-color", false, "")
	// Global flags come before the command; the rest of the line belongs to the command.
	rest := args
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "-" {
		n := 1
		if (rest[0] == "--home" || rest[0] == "-home") && len(rest) > 1 {
			n = 2
		}
		if err := fs.Parse(rest[:n]); err != nil {
			c.errf("noskill: %v\n", err)
			c.usage(env.Stderr)
			return ExitUsage
		}
		rest = rest[n:]
	}
	if len(rest) == 0 {
		c.usage(env.Stderr)
		return ExitUsage
	}
	name := rest[0]
	if name == "help" || name == "-h" || name == "--help" {
		c.usage(env.Stdout)
		return ExitOK
	}
	for _, cm := range commands {
		if cm.name == name {
			return cm.run(c, rest[1:])
		}
	}
	c.errf("noskill: unknown command %q\n", name)
	c.usage(env.Stderr)
	return ExitUsage
}

func (c *cmd) usage(w io.Writer) {
	fmt.Fprintln(w, "usage: noskill [--home DIR] [--json] [--verbose] [--quiet] <command> [args]\n\ncommands:")
	for _, cm := range commands {
		fmt.Fprintf(w, "  %-10s %s\n", cm.name, cm.summary)
	}
	fmt.Fprintln(w, "\nRun a command with --help for its flags.")
}

// ---- output ----

func (c *cmd) outf(format string, a ...any) { fmt.Fprintf(c.env.Stdout, format, a...) }
func (c *cmd) errf(format string, a ...any) { fmt.Fprintf(c.env.Stderr, format, a...) }

// infof is progress and chatter: stderr, silenced by --quiet.
func (c *cmd) infof(format string, a ...any) {
	if !c.g.quiet {
		fmt.Fprintf(c.env.Stderr, format, a...)
	}
}

func (c *cmd) printJSON(v any) {
	enc := json.NewEncoder(c.env.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (c *cmd) table(rows [][]string) {
	tw := tabwriter.NewWriter(c.env.Stdout, 0, 2, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// usageErr reports a mistake in how the command was called.
func (c *cmd) usageErr(format string, a ...any) int {
	c.errf("noskill: "+format+"\n", a...)
	return ExitUsage
}

// fail prints an error and picks its exit code.
func (c *cmd) fail(err error) int {
	var (
		nb *app.NoBackendError
		nm *app.NoModelError
		ue *app.UserError
	)
	switch {
	case errors.Is(err, context.Canceled):
		c.errf("noskill: cancelled\n")
		return 130
	case errors.As(err, &nb):
		c.errf("noskill: %v\n", err)
		c.errf("  Is Ollama running? Try `ollama serve`, or run `noskill doctor`.\n")
		return ExitUnavailable
	case errors.As(err, &nm), errors.As(err, &ue):
		c.errf("noskill: %v\n", err)
		return ExitUsage
	}
	c.errf("noskill: %v\n", err)
	return ExitError
}

// ---- flags ----

// parse parses flags that may come before, between or after positional arguments, so
// `noskill run "task" --model qwen3:4b` works. "--" ends flag parsing.
func (c *cmd) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(c.env.Stderr)
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos, nil
}

// multiFlag collects a repeatable flag (--allow a --allow b).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// ---- config and app ----

func (c *cmd) environ() []string {
	if c.env.Environ != nil {
		return c.env.Environ
	}
	return os.Environ()
}

// load reads the config (not opening any database).
func (c *cmd) load(withProject bool) error {
	if c.loaded != nil {
		return nil
	}
	home := c.g.home
	if home == "" {
		for _, e := range c.environ() {
			if v, ok := strings.CutPrefix(e, "NOSKILL_HOME="); ok {
				home = v
			}
		}
	}
	dirs, err := platform.DefaultDirs(home)
	if err != nil {
		return err
	}
	c.dirs = dirs
	cwd, _ := c.env.Getwd()
	proj := ""
	if withProject {
		proj = cwd
	}
	l, err := config.Load(config.Options{File: dirs.ConfigFile(), ProjectDir: proj, Env: c.environ()})
	if err != nil {
		return err
	}
	c.loaded = l
	return nil
}

func (c *cmd) secretStore() *secrets.Manager {
	if c.secrets == nil {
		k := c.env.Keychain
		if k == nil {
			k = secrets.OSKeychain()
		}
		c.secrets = secrets.NewManager(k, filepath.Join(c.dirs.Data, "secrets.names"))
		c.secrets.Environ = func(n string) (string, bool) {
			for _, e := range c.environ() {
				if v, ok := strings.CutPrefix(e, n+"="); ok {
					return v, true
				}
			}
			return "", false
		}
	}
	return c.secrets
}

// open builds the engine. Errors are already user-facing.
func (c *cmd) open(ctx context.Context) (*app.App, error) {
	if err := c.load(true); err != nil {
		return nil, err
	}
	if err := c.dirs.Ensure(); err != nil {
		return nil, err
	}
	if issues := config.Check(c.loaded, nil); config.HasErrors(issues) {
		var msgs []string
		for _, i := range issues {
			if i.Level == "error" {
				msgs = append(msgs, i.String())
			}
		}
		return nil, fmt.Errorf("config has errors (run `noskill config check`):\n  %s", strings.Join(msgs, "\n  "))
	}
	cwd, _ := c.env.Getwd()
	a, err := app.Open(ctx, app.Options{Dirs: c.dirs, Loaded: c.loaded, Secrets: c.secretStore(), Project: cwd,
		Notifier: c.env.Notifier, Sandbox: c.env.Sandbox, Trash: c.env.Trash, Stderr: c.env.Stderr})
	if err != nil {
		return nil, err
	}
	if c.g.verbose {
		for _, i := range a.SkillIssues {
			c.errf("skills: %s\n", i)
		}
	}
	return a, nil
}

// ---- prompts ----

// ask asks a yes/no question on the terminal. Without a terminal the answer is no: nothing is ever
// approved by a script that nobody is watching.
func (c *cmd) ask(question string, def bool) bool {
	if !c.env.StdinTTY {
		return false
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(c.env.Stderr, "%s %s ", question, hint)
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

func (c *cmd) readLine(prompt string) (string, error) {
	if prompt != "" {
		fmt.Fprint(c.env.Stderr, prompt)
	}
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdVersion(c *cmd, args []string) int {
	if c.g.json {
		c.printJSON(map[string]string{"version": c.env.Version})
		return ExitOK
	}
	c.outf("noskill %s\n", c.env.Version)
	return ExitOK
}

// trasher is the trash used for anything noskill moves out of the way.
func (c *cmd) trasher() platform.Trasher {
	if c.env.Trash != nil {
		return c.env.Trash
	}
	return platform.OSTrash{}
}
