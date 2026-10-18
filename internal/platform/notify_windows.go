package platform

import "context"

// NewNotifier: Windows toast notifications are not implemented yet (S7 Windows hasn't run).
// `doctor` reports it; approvals still work through the CLI and the other channels.
func NewNotifier() Notifier { return noNotifier{} }

type noNotifier struct{}

func (noNotifier) SupportsActions() bool { return false }

func (noNotifier) Notify(context.Context, Notification) error { return ErrUnsupported }
