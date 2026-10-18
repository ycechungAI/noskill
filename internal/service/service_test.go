package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

var spec = Spec{
	Exe:     `/Users/me/My Apps/noskill & co/noskill`,
	Args:    []string{"--home", "/Users/me/My Data", "serve"},
	LogPath: "/Users/me/My Data/logs/daemon.log",
	WorkDir: "/Users/me/My Data",
}

func TestLaunchdPlistIsValidAndEscaped(t *testing.T) {
	p := LaunchdPlist(spec)
	var doc struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal([]byte(p), &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, p)
	}
	for _, want := range []string{"<key>RunAtLoad</key><true/>", "<key>KeepAlive</key><true/>", "<string>dev.noskill.daemon</string>",
		"My Apps/noskill &amp; co/noskill", "<string>--home</string>", "<string>serve</string>", "StandardOutPath", "daemon.log"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "& co") {
		t.Error("an unescaped & in the plist breaks launchd")
	}
}

func TestSystemdUnitQuoting(t *testing.T) {
	u := SystemdUnit(Spec{Exe: `/home/me/bin dir/noskill`, Args: []string{"--home", `/home/me/100%/"x"`, "serve"}, LogPath: "/home/me/d.log", WorkDir: "/home/me/data dir"})
	for _, want := range []string{`ExecStart="/home/me/bin dir/noskill" "--home" "/home/me/100%%/\"x\"" "serve"`, "Restart=on-failure", "WantedBy=default.target",
		"StandardOutput=append:/home/me/d.log", `WorkingDirectory=/home/me/data\x20dir`} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
}

func TestWindowsTaskXML(t *testing.T) {
	b := WindowsTaskXML(Spec{Exe: `C:\Program Files\noskill\noskill.exe`, Args: []string{"--home", `C:\Users\me\My Data`, "serve"},
		LogPath: `C:\Users\me\My Data\logs\daemon.log`, WorkDir: `C:\Users\me\My Data`}, `ME\me`)
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatal("schtasks needs UTF-16 with a BOM")
	}
	u16 := make([]uint16, (len(b)-2)/2)
	for i := range u16 {
		u16[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
	}
	doc := string(utf16.Decode(u16))
	// The text is UTF-16 on disk; once decoded it's ordinary XML, so parse it as that.
	dec := xml.NewDecoder(strings.NewReader(strings.Replace(doc, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("not valid XML: %v\n%s", err, doc)
			}
			break
		}
	}
	for _, want := range []string{"<LogonTrigger>", "<RunLevel>LeastPrivilege</RunLevel>", `ME\me`, "cmd.exe", "&gt;&gt;", "daemon.log", "<Hidden>true</Hidden>", "RestartOnFailure"} {
		if !strings.Contains(doc, want) {
			t.Errorf("task XML lacks %q:\n%s", want, doc)
		}
	}
	if !strings.Contains(html.UnescapeString(doc), `/c ""C:\Program Files\noskill\noskill.exe" --home "C:\Users\me\My Data" serve >> "C:\Users\me\My Data\logs\daemon.log" 2>&1"`) {
		t.Errorf("the command needs its own outer quotes for cmd /c, and the exe path must stay quoted:\n%s", doc)
	}
}

func TestXDGAutostart(t *testing.T) {
	d := XDGAutostart(Spec{Exe: "/opt/no skill/noskill", Args: []string{"serve"}})
	if !strings.Contains(d, `Exec="/opt/no skill/noskill" serve`) {
		t.Errorf("%s", d)
	}
}

func TestTailLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.log")
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		b.WriteString("line " + strings.Repeat("x", 100) + " " + string(rune('0'+i%10)) + "\n")
	}
	os.WriteFile(p, []byte(b.String()), 0o644)
	var out bytes.Buffer
	if err := TailLog(context.Background(), p, 3, false, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "\n"); got != 3 || !strings.HasPrefix(out.String(), "line ") {
		t.Errorf("tail = %q", out.String())
	}
	if err := TailLog(context.Background(), filepath.Join(t.TempDir(), "none"), 3, false, &out); err == nil || !strings.Contains(err.Error(), "no log yet") {
		t.Errorf("missing log: %v", err)
	}
	// Follow prints what's appended until the context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var follow safeBuf
	done := make(chan error, 1)
	go func() { done <- TailLog(ctx, p, 1, true, &follow) }()
	time.Sleep(700 * time.Millisecond)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("fresh line\n")
	f.Close()
	<-done
	if !strings.Contains(follow.String(), "fresh line") {
		t.Errorf("follow missed the new line: %q", follow.String())
	}
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// ---- manager on this OS, with a fake runner ----

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []call
	reply func(name string, args []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, call{name, args})
	if f.reply != nil {
		return f.reply(name, args)
	}
	return "", nil
}

func (f *fakeRunner) joined() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.name+" "+strings.Join(c.args, " "))
	}
	return out
}

type trashTo struct{ dir string }

func (t trashTo) Trash(p string) (string, error) {
	to := filepath.Join(t.dir, filepath.Base(p))
	return to, os.Rename(p, to)
}

// exitErr builds a CmdError with a real exit code.
func exitErr(code int) error {
	cmd := exec.Command("sh", "-c", "exit "+string(rune('0'+code%10)))
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit "+string(rune('0'+code%10)))
	}
	err := cmd.Run()
	return &CmdError{Name: "x", Err: err, Stderr: "boom"}
}

func TestManagerLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	trash := t.TempDir()
	run := &fakeRunner{}
	m := NewManager(run, trashTo{trash})
	ctx := context.Background()
	s := Spec{Exe: "/usr/local/bin/noskill", Args: []string{"serve"}, LogPath: filepath.Join(home, "logs", "daemon.log"), WorkDir: home}

	if _, err := m.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("start before install: %v", err)
	}
	if err := m.Uninstall(ctx); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("uninstall before install: %v", err)
	}

	switch runtime.GOOS {
	case "darwin":
		run.reply = func(name string, args []string) (string, error) {
			if args[0] == "print" {
				return "dev.noskill.daemon = {\n\tstate = running\n\tpid = 4242\n}", nil
			}
			return "", nil
		}
	case "linux":
		run.reply = func(name string, args []string) (string, error) {
			switch {
			case args[1] == "is-active":
				return "active\n", nil
			case args[1] == "show" && len(args) > 2 && args[2] == "-p":
				return "4242\n", nil
			}
			return "", nil
		}
	case "windows":
		run.reply = func(name string, args []string) (string, error) {
			if args[0] == "/Query" {
				return "Folder: \\noskill\nStatus:                                Running\n", nil
			}
			return "", nil
		}
	}
	if err := m.Install(ctx, s); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(run.joined(), "\n")
	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", "dev.noskill.daemon.plist")
		if b, err := os.ReadFile(plist); err != nil || !strings.Contains(string(b), "RunAtLoad") {
			t.Fatalf("plist: %v", err)
		}
		if !strings.Contains(calls, "launchctl bootstrap gui/") {
			t.Errorf("calls = %s", calls)
		}
	case "linux":
		if !strings.Contains(calls, "systemctl --user daemon-reload") || !strings.Contains(calls, "systemctl --user enable --now noskill.service") {
			t.Errorf("calls = %s", calls)
		}
	case "windows":
		if !strings.Contains(calls, "schtasks /Create /TN noskill\\daemon /XML") {
			t.Errorf("calls = %s", calls)
		}
	}
	st, err := m.Status(ctx)
	if err != nil || !st.Installed || !st.Running || st.PID != 4242 && runtime.GOOS != "windows" {
		t.Errorf("status = %+v %v", st, err)
	}
	if len(m.Notes()) == 0 {
		t.Error("no notes")
	}
	run.calls = nil
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	run.calls = nil
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	calls = strings.Join(run.joined(), "\n")
	switch runtime.GOOS {
	case "darwin":
		// bootout (a clean stop) comes before the plist goes to the trash; nothing is deleted.
		if !strings.Contains(calls, "launchctl bootout gui/") {
			t.Errorf("no bootout: %s", calls)
		}
		if _, err := os.Stat(filepath.Join(trash, "dev.noskill.daemon.plist")); err != nil {
			t.Errorf("plist should be in the trash: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "dev.noskill.daemon.plist")); err == nil {
			t.Error("plist still installed")
		}
	case "linux":
		if !strings.Contains(calls, "disable --now") {
			t.Errorf("calls = %s", calls)
		}
		if _, err := os.Stat(filepath.Join(trash, "noskill.service")); err != nil {
			t.Errorf("unit should be in the trash: %v", err)
		}
	}
}

func TestDarwinStatusWhenSwitchedOff(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	run := &fakeRunner{reply: func(name string, args []string) (string, error) { return "", exitErr(3) }}
	m := NewManager(run, trashTo{t.TempDir()})
	m.Install(context.Background(), Spec{Exe: "/x/noskill", Args: []string{"serve"}})
	// The plist is there but launchd doesn't know it (the user switched it off in Login Items).
	st, err := m.Status(context.Background())
	if err != nil || !st.Installed || st.Running || !strings.Contains(st.Detail, "not loaded") {
		t.Errorf("%+v %v", st, err)
	}
}
