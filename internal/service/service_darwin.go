package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"noskill/internal/platform"
)

type launchd struct {
	run   Runner
	trash platform.Trasher
	home  string
	uid   int
}

// NewManager returns the manager for this OS. run and trash may be nil (real command, OS trash).
func NewManager(run Runner, trash platform.Trasher) Manager {
	if run == nil {
		run = execRunner{}
	}
	if trash == nil {
		trash = platform.OSTrash{}
	}
	h, _ := os.UserHomeDir()
	return &launchd{run: run, trash: trash, home: h, uid: os.Getuid()}
}

func (l *launchd) plist() string {
	return filepath.Join(l.home, "Library", "LaunchAgents", Label+".plist")
}
func (l *launchd) domain() string { return "gui/" + strconv.Itoa(l.uid) }
func (l *launchd) target() string { return l.domain() + "/" + Label }

func (l *launchd) Notes() []string {
	return []string{"macOS may show a \"Background Items Added\" notice; if you switch noskill off in System Settings > Login Items, `noskill doctor` will say so."}
}

func (l *launchd) Install(ctx context.Context, s Spec) error {
	if err := os.MkdirAll(filepath.Dir(l.plist()), 0o755); err != nil {
		return err
	}
	if s.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o700); err != nil {
			return err
		}
	}
	// A loaded copy is replaced: bootout first (SIGTERM, a clean stop), then bootstrap the new plist.
	if st, _ := l.Status(ctx); st.Installed {
		l.run.Run(ctx, "launchctl", "bootout", l.target())
	}
	if err := os.WriteFile(l.plist(), []byte(LaunchdPlist(s)), 0o644); err != nil {
		return err
	}
	if _, err := l.run.Run(ctx, "launchctl", "bootstrap", l.domain(), l.plist()); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w", err)
	}
	return nil
}

func (l *launchd) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(l.plist()); err != nil {
		if os.IsNotExist(err) {
			return ErrNotInstalled
		}
		return err
	}
	// bootout first: it stops the process cleanly. An already-unloaded agent isn't an error.
	if _, err := l.run.Run(ctx, "launchctl", "bootout", l.target()); err != nil && ExitCode(err) != 3 && ExitCode(err) != 113 && ExitCode(err) != 5 {
		return fmt.Errorf("launchctl bootout failed: %w", err)
	}
	_, err := l.trash.Trash(l.plist())
	return err
}

func (l *launchd) Start(ctx context.Context) error {
	if _, err := os.Stat(l.plist()); err != nil {
		return ErrNotInstalled
	}
	if st, _ := l.Status(ctx); !st.Installed {
		if _, err := l.run.Run(ctx, "launchctl", "bootstrap", l.domain(), l.plist()); err != nil {
			return fmt.Errorf("launchctl bootstrap failed: %w", err)
		}
		return nil
	}
	_, err := l.run.Run(ctx, "launchctl", "kickstart", l.target())
	return err
}

func (l *launchd) Stop(ctx context.Context) error {
	if _, err := os.Stat(l.plist()); err != nil {
		return ErrNotInstalled
	}
	// bootout unloads it: with KeepAlive a plain kill would only restart it. Start loads it again.
	_, err := l.run.Run(ctx, "launchctl", "bootout", l.target())
	if err != nil && (ExitCode(err) == 3 || ExitCode(err) == 113 || ExitCode(err) == 5) {
		return nil
	}
	return err
}

var (
	rePID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	reState = regexp.MustCompile(`(?m)^\s*state = (\w+)`)
)

func (l *launchd) Status(ctx context.Context) (Status, error) {
	st := Status{Unit: l.plist()}
	if _, err := os.Stat(l.plist()); err == nil {
		st.Installed = true
	}
	out, err := l.run.Run(ctx, "launchctl", "print", l.target())
	if err != nil {
		var ce *CmdError
		if errors.As(err, &ce) {
			if st.Installed {
				st.Detail = "installed but not loaded (switched off in Login Items, or stopped): run `noskill daemon start`"
			}
			return st, nil
		}
		return st, err
	}
	st.Installed = true
	if m := reState.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
		st.Detail = "launchd state: " + m[1]
	}
	if m := rePID.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
	}
	return st, nil
}
