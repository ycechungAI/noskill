// S9 launcher: runs one command in a per-run workspace under the OS sandbox, with rlimits and a
// watchdog (memory, process count, timeout), and prints one JSON result line. Mechanism per OS is in
// sandbox_<os>.go. -mode none runs the same way without the OS sandbox (baseline for cost and attacks).
//
//	sbx -run DIR [-mode sandbox|none] [-ro a,b] [-proxy 127.0.0.1:18080] [-fork] [-mem-mb 512]
//	    [-nproc 64] [-fsize-mb 256] [-timeout 30s] [-env K=V ...] -- cmd args...
//
// DIR/work is the only writable folder; the script's cwd, HOME and TMPDIR are inside it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Opts struct {
	Run, Work string
	RO        []string // read-only runtime paths (uv Python, node, venv)
	Proxy     string   // host:port of the egress proxy ("" = no network)
	ProxyUnix string   // Linux: Unix socket of the egress proxy, bind-mounted into the sandbox
	Fork      bool     // allow child processes
	Exec      []string // extra executables the script may run (e.g. /usr/bin/curl)
	Mech      string   // Linux: landlock | bwrap | bwrap+landlock
	Cgroup    bool     // Linux: put the run in a systemd user scope with MemoryMax/TasksMax
	MemMB     int64
	NProc     int
}

type Result struct {
	Mode     string  `json:"mode"`
	Cmd      string  `json:"cmd"`
	Exit     int     `json:"exit"`
	Signal   string  `json:"signal,omitempty"`
	Killed   string  `json:"killed,omitempty"` // memory | procs | timeout
	WallMs   float64 `json:"wall_ms"`
	PeakRSS  int64   `json:"peak_rss_mb"`
	PeakProc int     `json:"peak_procs"`
	Err      string  `json:"err,omitempty"`
}

const childArg = "__sbx_child"

func main() {
	if len(os.Args) > 1 && os.Args[1] == childArg {
		child() // never returns
	}
	if len(os.Args) > 1 && os.Args[1] == innerArg {
		inner() // Linux bwrap: proxy forwarder + script, inside the namespaces
	}
	run := flag.String("run", "", "run directory (work/ is created inside)")
	mode := flag.String("mode", "sandbox", "sandbox | none")
	ro := flag.String("ro", "", "comma-separated read-only paths")
	proxy := flag.String("proxy", "", "egress proxy host:port")
	proxyUnix := flag.String("proxy-unix", "", "egress proxy Unix socket (Linux)")
	proxyCred := flag.String("proxy-cred", "", "per-run proxy credential (user:token)")
	fork := flag.Bool("fork", false, "allow child processes")
	execs := flag.String("exec", "", "comma-separated extra executables")
	memMB := flag.Int64("mem-mb", 512, "memory limit for the process tree")
	nproc := flag.Int("nproc", 64, "max processes in the tree")
	fsizeMB := flag.Int64("fsize-mb", 256, "max size of one written file")
	timeout := flag.Duration("timeout", 30*time.Second, "wall-clock limit")
	var extraEnv []string
	flag.Func("env", "extra env K=V (repeatable)", func(v string) error { extraEnv = append(extraEnv, v); return nil })
	printProfile := flag.Bool("print-profile", false, "print the generated profile and exit")
	mech := flag.String("mech", "landlock", "Linux mechanism: landlock | bwrap | bwrap+landlock")
	cgroup := flag.Bool("cgroup", false, "Linux: run inside systemd-run --user --scope with MemoryMax/TasksMax")
	flag.Parse()
	args := flag.Args()
	if *run == "" || (len(args) == 0 && !*printProfile) {
		fmt.Fprintln(os.Stderr, "usage: sbx -run DIR [flags] -- cmd args")
		os.Exit(2)
	}
	o := Opts{Run: abs(*run), Proxy: *proxy, ProxyUnix: *proxyUnix, Fork: *fork, Mech: *mech, Cgroup: *cgroup, MemMB: *memMB, NProc: *nproc}
	o.Work = filepath.Join(o.Run, "work")
	o.RO = split(*ro)
	o.Exec = split(*execs)
	for _, d := range []string{"in", "out", "tmp", "home"} {
		os.MkdirAll(filepath.Join(o.Work, d), 0o755)
	}
	if *printProfile {
		fmt.Print(profile(o))
		return
	}

	env := []string{
		"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8",
		"HOME=" + filepath.Join(o.Work, "home"),
		"TMPDIR=" + filepath.Join(o.Work, "tmp") + "/",
		"NOSKILL_WORK=" + o.Work,
	}
	if o.Proxy != "" {
		p := "http://" + o.Proxy
		if *proxyCred != "" {
			p = "http://" + *proxyCred + "@" + o.Proxy
		}
		env = append(env, "HTTPS_PROXY="+p, "HTTP_PROXY="+p, "https_proxy="+p, "http_proxy="+p, "NO_PROXY=", "no_proxy=")
	}
	env = append(env, extraEnv...)
	env = append(env,
		"SBX_NPROC="+strconv.Itoa(*nproc),
		"SBX_FSIZE="+strconv.FormatInt(*fsizeMB<<20, 10),
		"SBX_CPU="+strconv.Itoa(int(timeout.Seconds())+1),
		"SBX_MODE="+*mode,
		"SBX_MEM="+strconv.FormatInt(*memMB<<20, 10),
	)

	var argv []string
	if *mode == "sandbox" {
		argv, env = wrap(o, args, env)
	} else {
		argv = args
	}
	self, _ := os.Executable()
	full := outer(o, *mode, append([]string{self, childArg}, argv...))
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Env = env
	cmd.Dir = o.Work
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	res := Result{Mode: *mode, Cmd: strings.Join(args, " ")}
	beforeStart()
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		res.Err = err.Error()
		emit(res)
		os.Exit(1)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pgid := cmd.Process.Pid
	tick := time.NewTicker(50 * time.Millisecond)
	deadline := time.After(*timeout)
	kill := func(why string) {
		if res.Killed == "" {
			res.Killed = why
		}
		killTree(pgid)
	}
loop:
	for {
		select {
		case err := <-done:
			res.WallMs = float64(time.Since(t0).Microseconds()) / 1000
			if ee, ok := err.(*exec.ExitError); ok {
				ws := ee.Sys().(syscall.WaitStatus)
				res.Exit = ws.ExitStatus()
				if ws.Signaled() {
					res.Signal = ws.Signal().String()
				}
			}
			break loop
		case <-deadline:
			kill("timeout")
		case <-tick.C:
			rss, n := groupUsage(pgid)
			res.PeakRSS = max(res.PeakRSS, rss>>20)
			res.PeakProc = max(res.PeakProc, n)
			if rss > *memMB<<20 {
				kill("memory")
			} else if n > 2**nproc { // the rlimit acts first; this catches what it misses
				kill("procs")
			}
		}
	}
	killTree(pgid) // leftovers
	emit(res)
}

// child runs inside the new process group: apply rlimits, then exec the (wrapped) command.
func child() {
	setLimits()
	argv := os.Args[2:]
	path, err := exec.LookPath(argv[0])
	if err != nil {
		path = argv[0]
	}
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "SBX_") {
			env = append(env, e)
		}
	}
	err = syscall.Exec(path, argv, env)
	fmt.Fprintln(os.Stderr, "sbx exec:", err)
	os.Exit(127)
}

func emit(r Result) {
	b, _ := json.Marshal(r)
	fmt.Fprintln(os.Stderr, "SBX_RESULT "+string(b))
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func abs(p string) string {
	a, _ := filepath.Abs(p)
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r
	}
	return a
}
