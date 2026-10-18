// Package platform holds everything that differs per OS: folders, trash, memory, process groups,
// the script sandbox, and (from block 1b) the service manager, notifications and volumes.
// Code outside this package must not assume POSIX paths, signals or shells (AGENTS.md).
package platform

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// ErrUnsupported is returned by a mechanism that isn't available on this OS (yet).
var ErrUnsupported = errors.New("not supported on " + runtime.GOOS)

// Trasher moves files to the OS trash. noskill never hard-deletes user files (SECURITY.md
// "Deletion policy"); every "delete" goes through here.
type Trasher interface {
	// Trash moves path to the trash and returns where it went, or "" when the OS doesn't say
	// (Windows Recycle Bin).
	Trash(path string) (string, error)
}

// Service installs and controls the daemon as a per-user service (FR-J2): launchd, systemd --user,
// Task Scheduler. Implemented in block 1b after S7.
type Service interface {
	Install(ctx context.Context, exe string, args []string) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (ServiceStatus, error)
}

type ServiceStatus struct {
	Installed bool
	Running   bool
	PID       int
	Detail    string
}

// Notifier shows OS notifications, with action buttons where supported (FR-A6, FR-J4).
// Block 1b ships plain notifications on macOS and Linux (S7: action buttons on macOS need a
// helper .app and aren't proven yet); the tray app adds buttons.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
	SupportsActions() bool
}

type Notification struct {
	ID      string // approval or job ID; actions answer with it
	Title   string
	Body    string
	Sound   bool // for things that need a person (approvals)
	Actions []NotificationAction
}

type NotificationAction struct {
	ID    string // "approve", "skip"
	Label string
}

// Volumes resolves `volume:<label>` tokens to mount paths (PLATFORMS.md "External drives").
// Implemented in Phase 3 (FR-J5); the broker accepts any Volumes so tests can fake one.
type Volumes interface {
	// Resolve returns the current mount path of the volume with this label (and UUID, when known).
	Resolve(label, uuid string) (mount string, gotUUID string, err error)
}

// ErrVolumeNotMounted means the drive isn't attached; jobs wait or skip cleanly.
var ErrVolumeNotMounted = errors.New("volume not mounted")

// Memory describes this machine for the orchestrator's memory budget (ORCHESTRATOR.md).
type Memory struct {
	TotalBytes   uint64
	AppleSilicon bool   // unified memory, Metal working-set limit applies
	VRAMBytes    uint64 // discrete GPU memory, 0 if unknown or none (filled from nvidia-smi etc. in 1b)
	Pressure     string // "normal" | "warn" | "critical" | "" (unknown)
	// AvailableBytes is memory that can be used right now without swapping (free plus what the OS
	// can reclaim at once); 0 = unknown. It moves with whatever else runs on the machine, which is
	// what the burst budget reads (ORCHESTRATOR.md "Memory budget").
	AvailableBytes uint64
	MeasuredAt     time.Time
}
