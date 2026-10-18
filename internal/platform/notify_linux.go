package platform

import (
	"context"
	"errors"
	"os/exec"
)

// NewNotifier posts plain notifications with notify-send (libnotify). Buttons come with the tray app.
func NewNotifier() Notifier { return notifySend{} }

type notifySend struct{}

func (notifySend) SupportsActions() bool { return false }

func (notifySend) Notify(ctx context.Context, n Notification) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return errors.New("notify-send isn't installed (libnotify-bin)")
	}
	urgency := "normal"
	if n.Sound {
		urgency = "critical"
	}
	return exec.CommandContext(ctx, path, "-a", "noskill", "-u", urgency, "--", n.Title, n.Body).Run()
}
