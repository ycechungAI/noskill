// Package service installs noskill's server as a per-user background service (FR-J2): a launchd
// LaunchAgent on macOS, a systemd --user unit on Linux, a Task Scheduler "at logon" task on Windows.
// None of them needs admin rights. The generators here compile and are tested on every OS; the
// Manager for the current OS runs the real commands through a Runner so tests never touch a
// machine's login items.
package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"noskill/internal/platform"
)

// Label is the service's identity on every OS: launchd label, systemd unit stem, task name.
const Label = "dev.noskill.daemon"

// Spec says what the service runs.
type Spec struct {
	Exe     string   // absolute path of the noskill binary
	Args    []string // usually ["--home", dir, "serve"]
	LogPath string   // stdout and stderr go here
	WorkDir string
}

type Status struct {
	Installed bool
	Running   bool
	PID       int
	Unit      string // path of the unit file (or task name)
	Detail    string
}

// Manager controls the service on this OS.
type Manager interface {
	Install(ctx context.Context, s Spec) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
	// Notes are things the user should know after installing (Linux: loginctl enable-linger).
	Notes() []string
}

// Runner runs an external command and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), &CmdError{Name: name, Args: args, Err: err, Stderr: strings.TrimSpace(errb.String() + out.String())}
	}
	return out.String(), nil
}

// CmdError is a failed command with what it printed.
type CmdError struct {
	Name   string
	Args   []string
	Err    error
	Stderr string
}

func (e *CmdError) Error() string {
	return fmt.Sprintf("%s %s: %v: %s", e.Name, strings.Join(e.Args, " "), e.Err, e.Stderr)
}
func (e *CmdError) Unwrap() error { return e.Err }

// ExitCode returns the exit status of a failed command, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ErrNotInstalled: the service isn't installed.
var ErrNotInstalled = errors.New("the noskill service isn't installed (run `noskill daemon install`)")

// ResolveSpec fills in the running binary and the log path. The binary path must stay valid after
// the install command exits, so a temporary `go run` build is refused.
func ResolveSpec(dirs platform.Dirs, home string) (Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return Spec{}, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return Spec{}, errors.New("this is a temporary `go run` binary; build noskill first (go build ./cmd/noskill)")
	}
	args := []string{}
	if home != "" {
		args = append(args, "--home", home)
	}
	args = append(args, "serve")
	return Spec{Exe: exe, Args: args, LogPath: filepath.Join(dirs.Logs(), "daemon.log"), WorkDir: dirs.Data}, nil
}

// ---- logs ----

// TailLog writes the last n lines of the log to w; with follow it keeps printing new lines until
// ctx ends.
func TailLog(ctx context.Context, path string, n int, follow bool, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no log yet at %s", path)
		}
		return err
	}
	defer f.Close()
	lines, err := lastLines(f, n)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	if !follow {
		return nil
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			fmt.Fprint(w, line)
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// lastLines reads the end of a file without loading all of it, leaving the file position at the end.
func lastLines(f *os.File, n int) ([]string, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const window = 256 << 10
	start := fi.Size() - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	f.Seek(0, io.SeekEnd)
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line is probably cut
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
