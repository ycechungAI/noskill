package platform

import (
	"context"
	"os/exec"
)

// NewNotifier posts plain notifications through osascript. They show under Script Editor's
// notification settings, need no helper app, and carry no buttons (S7: buttons need a signed
// helper .app with LaunchServices registration and the user's permission, still unproven).
func NewNotifier() Notifier { return osascriptNotifier{} }

type osascriptNotifier struct{}

func (osascriptNotifier) SupportsActions() bool { return false }

func (osascriptNotifier) Notify(ctx context.Context, n Notification) error {
	// The text goes in as arguments, never into the script, so nothing in it can run.
	stmt := `display notification (item 1 of argv) with title (item 2 of argv)`
	args := []string{n.Body, n.Title}
	if n.Sound {
		stmt += ` sound name (item 3 of argv)`
		args = append(args, "Glass")
	}
	cmd := append([]string{"-e", "on run argv", "-e", stmt, "-e", "end run"}, args...)
	return exec.CommandContext(ctx, "/usr/bin/osascript", cmd...).Run()
}
