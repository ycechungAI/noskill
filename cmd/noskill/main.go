// Command noskill is the CLI (CLI.md, FR-I1). The commands live in internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"noskill/internal/cli"
	"noskill/internal/platform"
)

// Version is set at build time: -ldflags "-X main.Version=v0.1.0".
var Version = "dev"

func main() {
	platform.MaybeRunSandboxChild() // must run first: the sandbox re-execs this binary (ADR-0008)
	fd := int(os.Stdin.Fd())
	env := cli.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Environ: os.Environ(), Version: Version,
		StdinTTY: term.IsTerminal(fd), StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())), StdinFD: fd,
		Context: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
	}
	os.Exit(cli.Main(os.Args[1:], env))
}
