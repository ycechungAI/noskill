package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"noskill/internal/platform"
	"noskill/internal/skills"
)

// ErrNotPrepared: a skill declares dependencies that haven't been installed. Installing them
// downloads packages, so it only happens on `noskill skills prepare`, never silently mid-run.
type ErrNotPrepared struct{ Skill string }

func (e *ErrNotPrepared) Error() string {
	return fmt.Sprintf("skill %s needs its dependencies installed: run `noskill skills prepare %s`", e.Skill, e.Skill)
}

// Interpreters finds the program that runs each script runtime, as the real executable (shims such
// as pyenv's can't run inside the sandbox) plus the folders it must be allowed to read.
type Interpreters struct {
	RuntimesDir string // <data>/runtimes: managed runtimes and per-skill venvs

	mu    sync.Mutex
	cache map[string]found
}

type found struct {
	exe      string
	prefix   string   // python: sys.base_prefix
	readOnly []string // folders the sandbox must allow reading and executing
}

// Command is what to run for one script tool call.
type Command struct {
	Argv     []string
	ReadOnly []string
	Exec     []string // extra programs the interpreter itself starts
}

// Resolve builds the command for a script tool. script is the absolute path of the entry file.
func (in *Interpreters) Resolve(ctx context.Context, sk *skills.Skill, t skills.Tool, script string) (Command, error) {
	switch t.Runtime {
	case "python":
		return in.python(ctx, sk, t, script)
	case "node", "bun":
		f, err := in.lookup(ctx, t.Runtime, func() (found, error) { return findNative(ctx, t.Runtime) })
		if err != nil {
			return Command{}, err
		}
		argv := []string{f.exe, script}
		if t.Runtime == "bun" {
			argv = []string{f.exe, "run", script}
		}
		return Command{Argv: argv, ReadOnly: f.readOnly}, nil
	case "sh":
		if runtime.GOOS == "windows" {
			return Command{}, errors.New("sh tools need a POSIX shell; this tool has no pwsh version for Windows")
		}
		// On macOS /bin/sh is a wrapper that re-execs /bin/bash.
		exec := []string{"/bin/sh"}
		if runtime.GOOS == "darwin" {
			exec = append(exec, "/bin/bash")
		}
		return Command{Argv: []string{"/bin/sh", script}, Exec: exec}, nil
	case "pwsh":
		f, err := in.lookup(ctx, "pwsh", func() (found, error) {
			name := "pwsh"
			if _, err := exec.LookPath(name); err != nil && runtime.GOOS == "windows" {
				name = "powershell.exe"
			}
			return findNative(ctx, name)
		})
		if err != nil {
			return Command{}, err
		}
		return Command{Argv: []string{f.exe, "-NoProfile", "-NonInteractive", "-File", script}, ReadOnly: f.readOnly}, nil
	}
	return Command{}, fmt.Errorf("runtime %q isn't a script runtime", t.Runtime)
}

func (in *Interpreters) lookup(ctx context.Context, key string, f func() (found, error)) (found, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if v, ok := in.cache[key]; ok {
		return v, nil
	}
	v, err := f()
	if err != nil {
		return found{}, err
	}
	if in.cache == nil {
		in.cache = map[string]found{}
	}
	in.cache[key] = v
	return v, nil
}

// findNative resolves a program on PATH to its real file and the library folders it loads.
func findNative(ctx context.Context, name string) (found, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return found{}, fmt.Errorf("%s isn't installed (needed by this tool): %w", name, err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return found{}, err
	}
	ro := append([]string{filepath.Dir(filepath.Dir(abs))}, platform.LibraryDirs(abs)...)
	if filepath.Dir(filepath.Dir(abs)) == filepath.Dir(abs) { // program at the root of a drive
		ro = ro[1:]
	}
	return found{exe: abs, readOnly: ro}, nil
}

func (in *Interpreters) basePython(ctx context.Context) (found, error) {
	return in.lookup(ctx, "python", func() (found, error) {
		var cands []string
		if p := filepath.Join(in.RuntimesDir, "python", pyRel()); fileExists(p) {
			cands = append(cands, p)
		}
		if uv, err := exec.LookPath("uv"); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			out, err := exec.CommandContext(cctx, uv, "python", "find").Output()
			cancel()
			if p := strings.TrimSpace(string(out)); err == nil && p != "" {
				cands = append(cands, p)
			}
		}
		for _, n := range []string{"python3", "python"} {
			if p, err := exec.LookPath(n); err == nil {
				cands = append(cands, p)
			}
		}
		var last error
		for _, c := range cands {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			out, err := exec.CommandContext(cctx, c, "-c", "import sys; print(sys.executable); print(sys.base_prefix)").Output()
			cancel()
			if err != nil {
				last = err
				continue
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) != 2 {
				continue
			}
			return found{exe: lines[0], prefix: lines[1], readOnly: []string{lines[1]}}, nil
		}
		if last != nil {
			return found{}, fmt.Errorf("no working Python found: %w", last)
		}
		return found{}, errors.New("Python isn't installed (a skill tool needs it); install Python 3 or uv")
	})
}

func pyRel() string {
	if runtime.GOOS == "windows" {
		return "python.exe"
	}
	return filepath.Join("bin", "python3")
}

func fileExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode().IsRegular() }

// VenvDir is where a skill's Python dependencies live. The name includes a hash of the
// requirements and the Python they were built for, so changing either gives a fresh venv.
func (in *Interpreters) VenvDir(skill string, deps []string, pyPrefix string) string {
	d := append([]string{}, deps...)
	sort.Strings(d)
	h := sha256.Sum256([]byte(strings.Join(d, "\n") + "\x00" + pyPrefix))
	return filepath.Join(in.RuntimesDir, "venvs", skill+"-"+hex.EncodeToString(h[:4]))
}

func venvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "Scripts", "python.exe")
	}
	return filepath.Join(dir, "bin", "python")
}

func (in *Interpreters) python(ctx context.Context, sk *skills.Skill, t skills.Tool, script string) (Command, error) {
	base, err := in.basePython(ctx)
	if err != nil {
		return Command{}, err
	}
	exe, ro := base.exe, base.readOnly
	if len(t.Deps) > 0 {
		dir := in.VenvDir(sk.Name, t.Deps, base.prefix)
		if !fileExists(venvPython(dir)) {
			return Command{}, &ErrNotPrepared{Skill: sk.Name}
		}
		exe, ro = venvPython(dir), append(append([]string{}, ro...), dir)
	}
	// -B: no .pyc writes (read-only folders); -E -s: ignore PYTHON* variables and the user's
	// site-packages. Not -I, which would drop the script's own folder from sys.path.
	return Command{Argv: []string{exe, "-B", "-E", "-s", script}, ReadOnly: ro}, nil
}

// CodeDeps is the analysis set the code-runner's run_python gets once `skills prepare code-runner`
// has installed it. Compatible-release ranges rather than exact pins, so a newer Python still
// finds wheels.
var CodeDeps = []string{"numpy>=2.1,<3", "pandas>=2.2,<3", "openpyxl>=3.1,<4", "matplotlib>=3.9,<4"}

// CodeSkill is the skill whose venv holds CodeDeps.
const CodeSkill = "code-runner"

// CodePython is the command for run_python: the program comes on stdin. prepared says whether the
// CodeDeps venv exists; without it the code gets the standard library only.
func (in *Interpreters) CodePython(ctx context.Context) (cmd Command, prepared bool, err error) {
	base, err := in.basePython(ctx)
	if err != nil {
		return Command{}, false, err
	}
	exe, ro := base.exe, base.readOnly
	if dir := in.VenvDir(CodeSkill, CodeDeps, base.prefix); fileExists(venvPython(dir)) {
		exe, ro, prepared = venvPython(dir), append(append([]string{}, ro...), dir), true
	}
	// -X utf8: stdout is UTF-8 on every OS, even with -E.
	return Command{Argv: []string{exe, "-B", "-E", "-s", "-X", "utf8", "-"}, ReadOnly: ro}, prepared, nil
}

// usesRunPython: the skill offers the run_python builtin, so preparing it installs CodeDeps.
func usesRunPython(sk *skills.Skill) bool {
	for _, t := range sk.Tools {
		if t.Runtime == "builtin" && t.Entry == "run_python" {
			return true
		}
	}
	return false
}

// Prepare installs a skill's Python dependencies into its venv. It needs uv and the network, and
// runs unsandboxed on purpose: the user approved the skill's permissions, and `skills prepare` is
// the one place packages are downloaded.
func (in *Interpreters) Prepare(ctx context.Context, sk *skills.Skill) ([]string, error) {
	var created []string
	type venv struct {
		skill string
		deps  []string
	}
	var want []venv
	for _, t := range sk.Tools {
		if t.Runtime == "python" && len(t.Deps) > 0 {
			want = append(want, venv{sk.Name, t.Deps})
		}
	}
	if usesRunPython(sk) {
		want = append(want, venv{CodeSkill, CodeDeps})
	}
	for _, v := range want {
		base, err := in.basePython(ctx)
		if err != nil {
			return created, err
		}
		dir := in.VenvDir(v.skill, v.deps, base.prefix)
		if fileExists(venvPython(dir)) {
			continue
		}
		uv, err := exec.LookPath("uv")
		if err != nil {
			return created, errors.New("installing Python dependencies needs uv (https://docs.astral.sh/uv/)")
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return created, err
		}
		for _, args := range [][]string{
			{"venv", "--python", base.exe, dir},
			append([]string{"pip", "install", "--python", venvPython(dir)}, v.deps...),
		} {
			cmd := exec.CommandContext(ctx, uv, args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return created, fmt.Errorf("uv %s: %v\n%s", args[0], err, tail(string(out), 800))
			}
		}
		created = append(created, dir)
	}
	return created, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
