package anime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"noskill/internal/platform"
)

func TestDetect(t *testing.T) {
	tr := httptest.NewServer(&fakeTransmission{})
	defer tr.Close()
	port := tr.Listener.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	ini := filepath.Join(dir, "qBittorrent.ini")
	os.WriteFile(ini, []byte("[Preferences]\nWebUI\\Enabled=false\nWebUI\\Port=1\n"), 0o644)
	trCfg := filepath.Join(dir, "settings.json")
	os.WriteFile(trCfg, []byte(`{"rpc-enabled": true, "rpc-port": 1}`), 0o644)

	d := Detector{
		Find: func(n platform.AppNames) []platform.Install {
			switch {
			case len(n.Mac) > 0 && n.Mac[0] == "Transmission.app":
				return []platform.Install{{Path: "/Applications/Transmission.app", Scope: "system"},
					{Path: "/Users/u/Applications/Transmission.app", Scope: "user"}}
			case len(n.Mac) > 0 && n.Mac[0] == "qBittorrent.app":
				return []platform.Install{{Path: "/Users/u/Applications/qBittorrent.app", Scope: "user"}}
			}
			return nil
		},
		Ports:  map[string][]int{"Transmission": {port}, "qBittorrent": {}, "Deluge": {}},
		Config: map[string]string{"qBittorrent": ini, "Transmission": trCfg},
	}
	got := d.Detect(context.Background())
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	use, ok := Usable(got)
	if !ok || use.Name != "Transmission" || !strings.HasSuffix(use.WebUI, ":"+itoa(port)) || len(use.Installs) != 2 {
		t.Fatalf("usable: %+v", use)
	}
	if got[1].Name != "qBittorrent" || !strings.Contains(got[1].Note, "remote access is off") {
		t.Errorf("qBittorrent: %+v", got[1])
	}
	desc := Describe(got)
	for _, want := range []string{"installed for everyone at /Applications/Transmission.app", "installed for you at /Users/u/Applications/Transmission.app", "ready"} {
		if !strings.Contains(desc, want) {
			t.Errorf("describe lacks %q:\n%s", want, desc)
		}
	}
	if Describe(nil) != NoClientNote {
		t.Error("no-client note")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Transmission is the user's torrent app; qBittorrent is used only when Transmission isn't answering.
func TestTransmissionIsPreferredOverQbittorrent(t *testing.T) {
	tr := httptest.NewServer(&fakeTransmission{})
	defer tr.Close()
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "v4.6.2") }))
	defer qb.Close()
	portOf := func(s *httptest.Server) int { return s.Listener.Addr().(*net.TCPAddr).Port }
	find := func(n platform.AppNames) []platform.Install {
		if len(n.Mac) > 0 && (n.Mac[0] == "Transmission.app" || n.Mac[0] == "qBittorrent.app") {
			return []platform.Install{{Path: "/Applications/" + n.Mac[0], Scope: "system"}}
		}
		return nil
	}
	// Config files of their own, so the apps on this machine aren't read.
	dir := t.TempDir()
	trCfg, ini := filepath.Join(dir, "settings.json"), filepath.Join(dir, "qBittorrent.ini")
	os.WriteFile(trCfg, []byte(`{"rpc-enabled": true, "rpc-port": 1}`), 0o644)
	os.WriteFile(ini, []byte("[Preferences]\nWebUI\\Enabled=true\nWebUI\\Port=1\n"), 0o644)
	d := Detector{Find: find, Ports: map[string][]int{"Transmission": {portOf(tr)}, "qBittorrent": {portOf(qb)}, "Deluge": {}},
		Config: map[string]string{"Transmission": trCfg, "qBittorrent": ini}}
	if use, ok := Usable(d.Detect(context.Background())); !ok || use.Name != "Transmission" {
		t.Errorf("both answering: %+v", use)
	}
	d.Ports["Transmission"] = []int{1} // Transmission is closed
	if use, ok := Usable(d.Detect(context.Background())); !ok || use.Name != "qBittorrent" {
		t.Errorf("Transmission closed: %+v", use)
	}
}
