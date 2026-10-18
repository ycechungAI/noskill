package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"noskill/internal/platform"
)

type systemd struct {
	run   Runner
	trash platform.Trasher
	home  string
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
	return &systemd{run: run, trash: trash, home: h}
}

func (s *systemd) configHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return filepath.Join(s.home, ".config")
}
func (s *systemd) unit() string {
	return filepath.Join(s.configHome(), "systemd", "user", "noskill.service")
}
func (s *systemd) autostart() string {
	return filepath.Join(s.configHome(), "autostart", "noskill.desktop")
}

func (s *systemd) Notes() []string {
	return []string{"To keep it running when you're logged out (headless machines): sudo loginctl enable-linger $USER"}
}

func (s *systemd) hasSystemd(ctx context.Context) bool {
	_, err := s.run.Run(ctx, "systemctl", "--user", "show-environment")
	return err == nil
}

func (s *systemd) Install(ctx context.Context, spec Spec) error {
	if spec.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
			return err
		}
	}
	if !s.hasSystemd(ctx) {
		// No systemd --user (a minimal container, WSL1): XDG autostart starts it at the next login.
		if err := os.MkdirAll(filepath.Dir(s.autostart()), 0o755); err != nil {
			return err
		}
		return os.WriteFile(s.autostart(), []byte(XDGAutostart(spec)), 0o644)
	}
	if err := os.MkdirAll(filepath.Dir(s.unit()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.unit(), []byte(SystemdUnit(spec)), 0o644); err != nil {
		return err
	}
	if _, err := s.run.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := s.run.Run(ctx, "systemctl", "--user", "enable", "--now", "noskill.service"); err != nil {
		return fmt.Errorf("systemctl enable failed: %w", err)
	}
	return nil
}

func (s *systemd) Uninstall(ctx context.Context) error {
	_, uerr := os.Stat(s.unit())
	_, aerr := os.Stat(s.autostart())
	if uerr != nil && aerr != nil {
		return ErrNotInstalled
	}
	if uerr == nil {
		s.run.Run(ctx, "systemctl", "--user", "disable", "--now", "noskill.service") // stops it too; fine if already stopped
		if _, err := s.trash.Trash(s.unit()); err != nil {
			return err
		}
		s.run.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	if aerr == nil {
		if _, err := s.trash.Trash(s.autostart()); err != nil {
			return err
		}
	}
	return nil
}

func (s *systemd) Start(ctx context.Context) error {
	if _, err := os.Stat(s.unit()); err != nil {
		return ErrNotInstalled
	}
	_, err := s.run.Run(ctx, "systemctl", "--user", "start", "noskill.service")
	return err
}

func (s *systemd) Stop(ctx context.Context) error {
	if _, err := os.Stat(s.unit()); err != nil {
		return ErrNotInstalled
	}
	_, err := s.run.Run(ctx, "systemctl", "--user", "stop", "noskill.service")
	return err
}

func (s *systemd) Status(ctx context.Context) (Status, error) {
	st := Status{Unit: s.unit()}
	if _, err := os.Stat(s.unit()); err != nil {
		if _, aerr := os.Stat(s.autostart()); aerr == nil {
			st.Installed, st.Unit, st.Detail = true, s.autostart(), "XDG autostart entry (no systemd --user here): starts at next login"
		}
		return st, nil
	}
	st.Installed = true
	out, err := s.run.Run(ctx, "systemctl", "--user", "is-active", "noskill.service")
	state := strings.TrimSpace(out)
	if err != nil && state == "" {
		var ce *CmdError
		if !errors.As(err, &ce) {
			return st, err
		}
	}
	st.Running = state == "active"
	st.Detail = "systemd state: " + state
	if pid, err := s.run.Run(ctx, "systemctl", "--user", "show", "-p", "MainPID", "--value", "noskill.service"); err == nil {
		st.PID, _ = strconv.Atoi(strings.TrimSpace(pid))
	}
	return st, nil
}
