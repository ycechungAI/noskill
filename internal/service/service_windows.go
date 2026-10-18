package service

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"noskill/internal/platform"
)

type taskSched struct {
	run   Runner
	trash platform.Trasher
}

const taskName = `noskill\daemon`

// NewManager returns the manager for this OS. run and trash may be nil (real command, OS trash).
// Untested on a real Windows machine so far (S7 Windows hasn't run); the schtasks calls are the
// documented ones.
func NewManager(run Runner, trash platform.Trasher) Manager {
	if run == nil {
		run = execRunner{}
	}
	if trash == nil {
		trash = platform.OSTrash{}
	}
	return &taskSched{run: run, trash: trash}
}

func (t *taskSched) Notes() []string {
	return []string{"Windows runs it when you log on. It has no window; `noskill daemon logs` shows its output."}
}

func (t *taskSched) Install(ctx context.Context, s Spec) error {
	if s.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o700); err != nil {
			return err
		}
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "noskill-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // our own scratch file in the temp folder
	if _, err := f.Write(WindowsTaskXML(s, u.Username)); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if _, err := t.run.Run(ctx, "schtasks", "/Create", "/TN", taskName, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("schtasks /Create failed: %w", err)
	}
	_, err = t.run.Run(ctx, "schtasks", "/Run", "/TN", taskName)
	return err
}

func (t *taskSched) Uninstall(ctx context.Context) error {
	if st, _ := t.Status(ctx); !st.Installed {
		return ErrNotInstalled
	}
	t.run.Run(ctx, "schtasks", "/End", "/TN", taskName)
	_, err := t.run.Run(ctx, "schtasks", "/Delete", "/TN", taskName, "/F")
	return err
}

func (t *taskSched) Start(ctx context.Context) error {
	if st, _ := t.Status(ctx); !st.Installed {
		return ErrNotInstalled
	}
	_, err := t.run.Run(ctx, "schtasks", "/Run", "/TN", taskName)
	return err
}

func (t *taskSched) Stop(ctx context.Context) error {
	if st, _ := t.Status(ctx); !st.Installed {
		return ErrNotInstalled
	}
	_, err := t.run.Run(ctx, "schtasks", "/End", "/TN", taskName)
	return err
}

var reStatus = regexp.MustCompile(`(?mi)^\s*Status:\s*(.+?)\s*$`)

func (t *taskSched) Status(ctx context.Context) (Status, error) {
	st := Status{Unit: `Task Scheduler: ` + taskName}
	out, err := t.run.Run(ctx, "schtasks", "/Query", "/TN", taskName, "/FO", "LIST", "/V")
	if err != nil {
		return st, nil // not found
	}
	st.Installed = true
	if m := reStatus.FindStringSubmatch(out); m != nil {
		st.Detail = "task state: " + m[1]
		st.Running = strings.EqualFold(m[1], "Running")
	}
	return st, nil
}
