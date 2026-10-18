// S7 spike (macOS part): a skeleton daemon that runs a job every 5 minutes and logs one JSON line per
// run, installed as a launchd user agent; plus a notification helper test. Throwaway, stdlib only.
// Requirements: FR-J2 (daemon as a user service), FR-J3 (missed-run policy), FR-A6 (approval notifications).
//
//	s7 daemon  [-policy once|skip] [-interval 5m] [-naive] [-ipc] [-log FILE]   resident scheduler
//	s7 oneshot [-interval 5m] [-log FILE]                                       run once (launchd calendar job)
//	s7 install   -variant once|skip|calendar [-log FILE] [-helper APP]
//	s7 uninstall -variant once|skip|calendar|all
//	s7 notify [-title T] [-body B] [-launch exec|open]                          ask the daemon to post a notification
//	s7 status
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	label    = "dev.noskill.s7."
	grace    = 60 * time.Second // a slot counts as "on time" if we handle it within this
	tickEach = time.Second
)

var home = func() string { h, _ := os.UserHomeDir(); return h }()
var appDir = filepath.Join(home, ".noskill-s7")
var helperApp = filepath.Join(home, "Applications", "NoskillNotify.app")

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

func rec(logPath string, m map[string]any) {
	m["ts"] = time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	m["pid"] = os.Getpid()
	b, _ := json.Marshal(m)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log:", err)
		return
	}
	defer f.Close()
	f.Write(append(b, '\n')) // single write: safe for the several agents appending to one file
}

func floor(t time.Time, iv time.Duration) time.Time {
	return time.Unix(0, t.UnixNano()/int64(iv)*int64(iv)).In(t.Location())
}

func boot() string {
	out, _ := exec.Command("sysctl", "-n", "kern.boottime").Output()
	s := string(out) // { sec = 1760000000, usec = 0 } ...
	if i := strings.Index(s, "sec = "); i >= 0 {
		j := strings.Index(s[i:], ",")
		return strings.TrimSpace(s[i+6 : i+j])
	}
	return ""
}

// ---- state: last handled slot, kept across restarts so a reboot/logout is a "missed run" too ----

func statePath(name string) string { return filepath.Join(appDir, "state-"+name+".json") }
func loadSlot(name string) (time.Time, bool) {
	b, err := os.ReadFile(statePath(name))
	if err != nil {
		return time.Time{}, false
	}
	var s struct{ Slot int64 }
	if json.Unmarshal(b, &s) != nil || s.Slot == 0 {
		return time.Time{}, false
	}
	return time.Unix(s.Slot, 0), true
}
func saveSlot(name string, t time.Time) {
	os.MkdirAll(appDir, 0o755)
	b, _ := json.Marshal(map[string]int64{"Slot": t.Unix()})
	tmp := statePath(name) + ".tmp"
	os.WriteFile(tmp, b, 0o644)
	os.Rename(tmp, statePath(name))
}

// ---- resident daemon ----

type daemon struct {
	log      string
	policy   string
	interval time.Duration
	name     string
}

func (d *daemon) run(slot time.Time, now time.Time, kind string, collapsed int) {
	rec(d.log, map[string]any{
		"event": "run", "mode": "wallclock", "variant": d.name, "policy": d.policy, "kind": kind,
		"scheduled": slot.Format("15:04:05"), "scheduled_epoch": slot.Unix(),
		"actual_epoch_ms": now.UnixMilli(), "drift_ms": now.Sub(slot).Milliseconds(), "collapsed": collapsed,
	})
}

func (d *daemon) loop() {
	last, ok := loadSlot(d.name)
	now := time.Now()
	if !ok {
		last = floor(now, d.interval)
	}
	prev := now
	for {
		time.Sleep(tickEach)
		now = time.Now()
		if gap := now.Sub(prev); gap > 5*time.Second { // time.Sleep uses the monotonic clock, which pauses during sleep
			rec(d.log, map[string]any{"event": "wake", "variant": d.name, "gap_s": gap.Seconds(), "from": prev.Format("15:04:05"), "to": now.Format("15:04:05")})
		}
		prev = now
		cur := floor(now, d.interval)
		n := int(cur.Sub(last) / d.interval)
		if n <= 0 {
			continue
		}
		onTime := now.Sub(cur) <= grace
		switch {
		case n == 1 && onTime:
			d.run(cur, now, "scheduled", 0)
		case d.policy == "once":
			kind := "catchup"
			if onTime {
				kind = "scheduled" // the on-time slot covers the missed ones
			}
			d.run(cur, now, kind, n-1)
		default: // skip
			missed := n
			if onTime {
				d.run(cur, now, "scheduled", 0)
				missed = n - 1
			}
			if missed > 0 {
				rec(d.log, map[string]any{"event": "skipped", "variant": d.name, "policy": "skip", "missed": missed, "latest_slot": cur.Format("15:04:05"), "late_ms": now.Sub(cur).Milliseconds()})
			}
		}
		last = cur
		saveSlot(d.name, last)
	}
}

// A plain time.Ticker, as a naive implementation would use: shows how the monotonic clock drifts over sleep.
func (d *daemon) naive() {
	start := time.Now()
	t := time.NewTicker(d.interval)
	for k := 1; ; k++ {
		<-t.C
		now := time.Now()
		nominal := start.Add(time.Duration(k) * d.interval)
		rec(d.log, map[string]any{"event": "run", "mode": "naive-ticker", "variant": d.name, "kind": "scheduled",
			"scheduled": nominal.Format("15:04:05"), "scheduled_epoch": nominal.Unix(), "actual_epoch_ms": now.UnixMilli(), "drift_ms": now.Sub(nominal).Milliseconds()})
	}
}

// ---- IPC (Unix domain socket): CLI -> daemon, notification helper -> daemon ----

var sockPath = filepath.Join(appDir, "run", "s7.sock")

func (d *daemon) serve() {
	os.MkdirAll(filepath.Dir(sockPath), 0o755)
	os.Remove(sockPath) // our own stale socket, not a user file
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		rec(d.log, map[string]any{"event": "ipc_error", "err": err.Error()})
		return
	}
	posted := map[string]time.Time{}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var m map[string]any
				if json.Unmarshal(sc.Bytes(), &m) != nil {
					continue
				}
				d.handle(m, posted)
				c.Write([]byte("ok\n"))
			}
		}()
	}
}

func (d *daemon) handle(m map[string]any, posted map[string]time.Time) {
	switch m["type"] {
	case "notify":
		id := fmt.Sprint(m["id"])
		helper := helperApp
		if h, _ := m["helper"].(string); h != "" {
			helper = h
		}
		args := []string{"--id", id, "--title", fmt.Sprint(m["title"]), "--body", fmt.Sprint(m["body"]), "--socket", sockPath, "--timeout", "240"}
		var cmd *exec.Cmd
		if m["launch"] == "open" { // via LaunchServices
			cmd = exec.Command("open", append([]string{"-n", "-W", "-a", helper, "--args"}, args...)...)
		} else { // straight exec of the bundle's binary, as a daemon would
			cmd = exec.Command(filepath.Join(helper, "Contents", "MacOS", "noskill-notify"), args...)
		}
		os.MkdirAll(filepath.Join(appDir, "logs"), 0o755)
		lf, _ := os.Create(filepath.Join(appDir, "logs", "notify-"+id+".log"))
		cmd.Stderr, cmd.Stdout = lf, lf
		posted[id] = time.Now()
		err := cmd.Start()
		rec(d.log, map[string]any{"event": "notify_requested", "id": id, "launch": m["launch"], "err": fmt.Sprint(err), "helper_pid": func() int {
			if cmd.Process != nil {
				return cmd.Process.Pid
			}
			return 0
		}()})
		if err == nil {
			go func() {
				err := cmd.Wait()
				lf.Close()
				rec(d.log, map[string]any{"event": "notify_helper_exit", "id": id, "err": fmt.Sprint(err)})
			}()
		}
	default: // events from the helper: auth, posted, response, timeout, error
		id := fmt.Sprint(m["id"])
		m["event"] = "notify_" + fmt.Sprint(m["type"])
		if t, ok := posted[id]; ok {
			m["since_request_ms"] = time.Since(t).Milliseconds()
		}
		delete(m, "type")
		rec(d.log, m)
	}
}

func send(m map[string]any) error {
	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	b, _ := json.Marshal(m)
	c.Write(append(b, '\n'))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = bufio.NewReader(c).ReadString('\n')
	return err
}

// ---- install / uninstall (launchd user agent, no admin) ----

func plist(variant, bin, log string) string {
	args := []string{bin}
	extra := ""
	switch variant {
	case "calendar":
		args = append(args, "oneshot", "-log", log)
		extra = "\t<key>StartCalendarInterval</key>\n\t<array>\n"
		for m := 0; m < 60; m += 5 {
			extra += fmt.Sprintf("\t\t<dict><key>Minute</key><integer>%d</integer></dict>\n", m)
		}
		extra += "\t</array>\n"
	default:
		args = append(args, "daemon", "-policy", variant, "-log", log, "-name", variant)
		if variant == "once" {
			args = append(args, "-naive", "-ipc")
		}
		extra = "\t<key>RunAtLoad</key><true/>\n\t<key>KeepAlive</key><true/>\n"
	}
	var a strings.Builder
	for _, s := range args {
		a.WriteString("\t\t<string>" + s + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + label + variant + `</string>
	<key>ProgramArguments</key>
	<array>
` + a.String() + `	</array>
` + extra + `	<key>StandardOutPath</key><string>` + filepath.Join(appDir, "logs", variant+".out") + `</string>
	<key>StandardErrorPath</key><string>` + filepath.Join(appDir, "logs", variant+".err") + `</string>
</dict>
</plist>
`
}

func agentPath(variant string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+variant+".plist")
}

func launchctl(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// trash moves a file or folder to the user's Trash; it never deletes. Tries a plain rename into ~/.Trash,
// then Finder via osascript (which also records "Put Back" info).
func trash(p string) (how string, err error) {
	dst := filepath.Join(home, ".Trash", filepath.Base(p))
	if _, e := os.Lstat(dst); e == nil {
		dst += "-" + time.Now().Format("150405")
	}
	if err = os.Rename(p, dst); err == nil {
		return "rename", nil
	}
	script := fmt.Sprintf(`tell application "Finder" to delete POSIX file %q`, p)
	if out, e := exec.Command("osascript", "-e", script).CombinedOutput(); e != nil {
		return "failed", fmt.Errorf("rename: %v; finder: %v %s", err, e, out)
	}
	return "finder", nil
}

func install(variant, log, helperSrc string) {
	exe, _ := os.Executable()
	bin := filepath.Join(appDir, "bin", "s7")
	must(copyFile(exe, bin))
	os.MkdirAll(filepath.Join(appDir, "logs"), 0o755)
	if helperSrc != "" {
		// The helper must live in a normal app folder and be registered with LaunchServices, or usernoted
		// answers "Notifications are not allowed" without ever showing the permission prompt (S7 finding).
		must(os.MkdirAll(filepath.Dir(helperApp), 0o755))
		if out, err := exec.Command("ditto", helperSrc, helperApp).CombinedOutput(); err != nil {
			fmt.Println("helper copy:", err, string(out))
		}
		if out, err := exec.Command(lsregister, "-f", helperApp).CombinedOutput(); err != nil {
			fmt.Println("lsregister:", err, string(out))
		}
	}
	pp := agentPath(variant)
	must(os.MkdirAll(filepath.Dir(pp), 0o755))
	must(os.WriteFile(pp, []byte(plist(variant, bin, log)), 0o644))
	out, err := launchctl("bootstrap", "gui/"+strconv.Itoa(os.Getuid()), pp)
	fmt.Printf("installed %s (bootstrap: %q err=%v)\n", label+variant, out, err)
}

func uninstall(variant string) {
	vs := []string{variant}
	if variant == "all" {
		vs = []string{"once", "skip", "calendar"}
	}
	for _, v := range vs {
		pp := agentPath(v)
		if _, err := os.Stat(pp); err != nil {
			continue
		}
		out, err := launchctl("bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+label+v)
		fmt.Printf("bootout %s: %q err=%v\n", v, out, err)
		how, err := trash(pp)
		fmt.Printf("plist -> Trash via %s err=%v\n", how, err)
	}
	if _, err := os.Stat(helperApp); err == nil && variant == "all" {
		exec.Command(lsregister, "-u", helperApp).Run()
		how, err := trash(helperApp)
		fmt.Printf("%s -> Trash via %s err=%v\n", helperApp, how, err)
	}
	// last agent gone: the whole app folder (binary, helper, logs, state) goes to the Trash as one item
	left := 0
	for _, v := range []string{"once", "skip", "calendar"} {
		if _, err := os.Stat(agentPath(v)); err == nil {
			left++
		}
	}
	if left == 0 {
		if _, err := os.Stat(appDir); err == nil {
			how, err := trash(appDir)
			fmt.Printf("%s -> Trash via %s err=%v\n", appDir, how, err)
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: s7 daemon|oneshot|install|uninstall|notify|status")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	logp := fs.String("log", "/Users/neolinux/dev/noskill/spikes/results/s7-macos.jsonl", "results file")
	policy := fs.String("policy", "once", "missed-run policy: once|skip")
	iv := fs.Duration("interval", 5*time.Minute, "job interval")
	naive := fs.Bool("naive", false, "also run a plain time.Ticker for comparison")
	ipc := fs.Bool("ipc", false, "serve the Unix socket")
	name := fs.String("name", "", "variant name for state/log")
	variant := fs.String("variant", "once", "install/uninstall: once|skip|calendar|all")
	helper := fs.String("helper", "", "install: path of NoskillNotify.app to copy")
	title := fs.String("title", "noskill approval", "")
	body := fs.String("body", "Move 3 files to ~/Documents/Invoices?", "")
	launch := fs.String("launch", "exec", "notify: exec|open")
	fs.Parse(os.Args[2:])

	switch os.Args[1] {
	case "daemon":
		if *name == "" {
			*name = *policy
		}
		d := &daemon{log: *logp, policy: *policy, interval: *iv, name: *name}
		rec(*logp, map[string]any{"event": "start", "variant": d.name, "policy": d.policy, "interval_s": iv.Seconds(), "boot": boot(),
			"launchd_label": os.Getenv("XPC_SERVICE_NAME"), "ppid": os.Getppid(), "last_slot_saved": func() string {
				t, ok := loadSlot(d.name)
				if ok {
					return t.Format("15:04:05")
				}
				return ""
			}()})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			s := <-sig
			rec(*logp, map[string]any{"event": "stop", "variant": d.name, "signal": s.String()})
			if *ipc {
				os.Remove(sockPath)
			}
			os.Exit(0)
		}()
		if *naive {
			go d.naive()
		}
		if *ipc {
			go d.serve()
		}
		d.loop()
	case "oneshot":
		now := time.Now()
		slot := floor(now, *iv)
		prev, ok := loadSlot("calendar")
		gap := 0
		if ok {
			gap = int(slot.Sub(prev) / *iv) - 1
		}
		rec(*logp, map[string]any{"event": "run", "mode": "launchd-calendar", "variant": "calendar", "kind": "scheduled",
			"scheduled": slot.Format("15:04:05"), "scheduled_epoch": slot.Unix(), "actual_epoch_ms": now.UnixMilli(), "drift_ms": now.Sub(slot).Milliseconds(), "slots_since_last_run": gap, "boot": boot()})
		saveSlot("calendar", slot)
	case "install":
		install(*variant, *logp, *helper)
	case "uninstall":
		uninstall(*variant)
	case "notify":
		id := strconv.FormatInt(time.Now().Unix(), 10)
		err := send(map[string]any{"type": "notify", "id": id, "title": *title, "body": *body, "launch": *launch, "helper": *helper})
		fmt.Println("requested", id, "err=", err)
	case "status":
		out, _ := launchctl("print", "gui/"+strconv.Itoa(os.Getuid())+"/"+label+*variant)
		fmt.Println(out)
	}
}
