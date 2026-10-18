package anime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"noskill/internal/platform"
)

// ClientInfo is one torrent app found on this machine.
type ClientInfo struct {
	Name      string             `json:"name"`
	Installs  []platform.Install `json:"installs,omitempty"` // per user and system wide; empty when only its Web UI answered
	WebUI     string             `json:"web_ui,omitempty"`   // a local Web UI that answered, e.g. http://127.0.0.1:8080
	Login     bool               `json:"needs_login,omitempty"`
	Version   string             `json:"version,omitempty"`
	Supported bool               `json:"supported"` // the tracker can drive it
	Note      string             `json:"note,omitempty"`
}

// client is how to find one torrent app.
type client struct {
	name   string
	app    platform.AppNames
	ports  []int
	probe  func(ctx context.Context, c *http.Client, base string) (version string, login, ok bool)
	conf   func(path string) appConfig // its remote-access settings
	ok     bool                        // supported
	turnOn string                      // where the user turns remote access on
}

// Transmission comes first: it is the user's torrent app. qBittorrent is a backup, used only when
// Transmission isn't there or isn't answering (the order here is the order of preference).
var clients = []client{
	{name: "Transmission", ok: true, ports: []int{9091},
		app: platform.AppNames{Mac: []string{"Transmission.app"}, MacID: []string{"org.m0k.transmission"},
			Exe: []string{"transmission-gtk", "transmission-qt", "transmission-daemon", "com.transmissionbt.Transmission"},
			Win: []string{`Transmission\transmission-qt.exe`}, Display: []string{"Transmission"}},
		probe: probeTransmission, conf: readTransmissionConfig,
		turnOn: "Transmission › Settings › Remote › \"Enable remote access\" (port 9091, allow only 127.0.0.1)"},
	{name: "qBittorrent", ok: true, ports: []int{8080},
		app: platform.AppNames{Mac: []string{"qBittorrent.app"}, MacID: []string{"org.qbittorrent.qBittorrent"}, Display: []string{"qBittorrent"}, Exe: []string{"qbittorrent", "qbittorrent-nox", "org.qbittorrent.qBittorrent"},
			Win: []string{`qBittorrent\qbittorrent.exe`}},
		probe: probeQbit, conf: readQbitConfig, turnOn: "qBittorrent › Preferences › Web UI (listen on 127.0.0.1)"},
	{name: "Deluge", ports: []int{8112},
		app: platform.AppNames{Mac: []string{"Deluge.app"}, Exe: []string{"deluge", "deluged", "deluge-gtk", "org.deluge_torrent.deluge"},
			Win: []string{`Deluge\deluge.exe`}},
		probe: probeDeluge},
	{name: "BiglyBT", app: platform.AppNames{Mac: []string{"BiglyBT.app"}, Exe: []string{"biglybt", "com.biglybt.BiglyBT"}, Win: []string{`BiglyBT\BiglyBT.exe`}}},
	{name: "Vuze", app: platform.AppNames{Mac: []string{"Vuze.app"}, Exe: []string{"vuze"}, Win: []string{`Vuze\Azureus.exe`}}},
	{name: "µTorrent", app: platform.AppNames{Mac: []string{"uTorrent.app", "uTorrent Web.app"}, Win: []string{`uTorrent\uTorrent.exe`, `uTorrent Web\utweb.exe`}}},
	{name: "BitTorrent", app: platform.AppNames{Mac: []string{"BitTorrent.app", "BitTorrent Web.app"}, Win: []string{`BitTorrent\BitTorrent.exe`, `BitTorrent Web\btweb.exe`}}},
	{name: "Tixati", app: platform.AppNames{Exe: []string{"tixati"}, Win: []string{`Tixati\tixati.exe`}}},
	{name: "Motrix", app: platform.AppNames{Mac: []string{"Motrix.app"}, Exe: []string{"motrix", "net.agalwood.Motrix"}, Win: []string{`Motrix\Motrix.exe`}}},
	{name: "Folx", app: platform.AppNames{Mac: []string{"Folx.app"}}},
	{name: "WebTorrent", app: platform.AppNames{Mac: []string{"WebTorrent.app"}, Exe: []string{"webtorrent-desktop", "io.webtorrent.WebTorrent"}, Win: []string{`WebTorrent\WebTorrent.exe`}}},
}

// Detector finds the user's torrent apps: what's installed, and which local Web UIs answer.
// It only reads files and talks to 127.0.0.1; it never starts or reconfigures an app.
type Detector struct {
	HTTP *http.Client
	Find func(platform.AppNames) []platform.Install // default platform.FindApp
	// Ports overrides the Web UI ports to try per app name (tests); the app's own settings add its port.
	Ports map[string][]int
	// Config overrides where an app's settings file is, per app name (tests).
	Config map[string]string
}

// Detect lists every torrent app it finds, the supported one with a working Web UI first.
func (d Detector) Detect(ctx context.Context) []ClientInfo {
	find := d.Find
	if find == nil {
		find = platform.FindApp
	}
	hc := d.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Second}
	}
	var out []ClientInfo
	for _, c := range clients {
		info := ClientInfo{Name: c.name, Installs: find(c.app), Supported: c.ok}
		ports := c.ports
		if p, ok := d.Ports[c.name]; ok {
			ports = p
		}
		var cfg appConfig
		if c.conf != nil {
			cfg = c.conf(d.Config[c.name])
			if cfg.port > 0 && !containsInt(ports, cfg.port) {
				ports = append([]int{cfg.port}, ports...)
			}
		}
		if c.probe != nil {
			for _, p := range ports {
				base := "http://127.0.0.1:" + strconv.Itoa(p)
				if v, login, ok := c.probe(ctx, hc, base); ok {
					info.WebUI, info.Version, info.Login = base, v, login
					break
				}
			}
		}
		if len(info.Installs) == 0 && info.WebUI == "" {
			continue
		}
		key := strings.ToUpper(c.name)
		switch {
		case !c.ok:
			info.Note = c.name + " isn't supported yet; the tracker works with qBittorrent or Transmission."
		case info.WebUI == "" && cfg.found && !cfg.enabled:
			info.Note = c.name + "'s remote access is off. Turn it on in " + c.turnOn + ", then run the tracker again."
		case info.WebUI == "":
			info.Note = c.name + " didn't answer. Start it and check " + c.turnOn + "."
		case info.Login:
			info.Note = c.name + " needs a login: save it with `noskill secrets set " + key + "_USER` and `" + key + "_PASS`."
		}
		out = append(out, info)
	}
	// The usable one first; among usable ones, the order of `clients` (Transmission before qBittorrent).
	for i, c := range out {
		if c.Supported && c.WebUI != "" {
			first := out[i]
			copy(out[1:i+1], out[:i])
			out[0] = first
			break
		}
	}
	return out
}

// NoClientNote is what the tracker says when no torrent app was found.
const NoClientNote = "No torrent app found. Install Transmission (https://transmissionbt.com) or qBittorrent (https://www.qbittorrent.org), turn on its remote access for 127.0.0.1, then run the tracker again."

// Usable returns the first detected app the tracker can drive now.
func Usable(cs []ClientInfo) (ClientInfo, bool) {
	for _, c := range cs {
		if c.Supported && c.WebUI != "" {
			return c, true
		}
	}
	return ClientInfo{}, false
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

type appConfig struct {
	found, enabled bool
	port           int
}

// readQbitConfig reads the Web UI section of qBittorrent.ini, which qBittorrent keeps in
// ~/.config/qBittorrent on macOS and Linux (or the Flatpak's folder) and %APPDATA%\qBittorrent on Windows.
func readQbitConfig(path string) appConfig {
	var paths []string
	if path != "" {
		paths = []string{path}
	} else {
		if d, err := os.UserConfigDir(); err == nil {
			paths = append(paths, filepath.Join(d, "qBittorrent", "qBittorrent.ini"))
		}
		if h, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(h, ".config", "qBittorrent", "qBittorrent.ini"),
				filepath.Join(h, ".var", "app", "org.qbittorrent.qBittorrent", "config", "qBittorrent", "qBittorrent.ini"))
		}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		cfg := appConfig{found: true}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if !ok {
				continue
			}
			switch strings.ReplaceAll(k, `\\`, `\`) {
			case `WebUI\Enabled`:
				cfg.enabled = strings.EqualFold(v, "true")
			case `WebUI\Port`:
				cfg.port, _ = strconv.Atoi(v)
			}
		}
		f.Close()
		return cfg
	}
	return appConfig{}
}

// readTransmissionConfig reads remote access settings: the Mac app's preferences, or settings.json
// of transmission-gtk/qt (~/.config/transmission), transmission-daemon (~/.config/transmission-daemon)
// and the Windows app (%LOCALAPPDATA%\transmission).
func readTransmissionConfig(path string) appConfig {
	if path == "" {
		if v, ok := platform.AppPref("org.m0k.transmission", "RPC"); ok {
			cfg := appConfig{found: true, enabled: v == "1", port: 9091}
			if p, ok := platform.AppPref("org.m0k.transmission", "RPCPort"); ok {
				if n, err := strconv.Atoi(p); err == nil {
					cfg.port = n
				}
			}
			return cfg
		}
	}
	var paths []string
	if path != "" {
		paths = []string{path}
	} else {
		var dirs []string
		if d, err := os.UserConfigDir(); err == nil {
			dirs = append(dirs, d)
		}
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			dirs = append(dirs, d)
		}
		if h, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(h, ".config"))
		}
		for _, d := range dirs {
			for _, n := range []string{"transmission", "transmission-daemon"} {
				paths = append(paths, filepath.Join(d, n, "settings.json"))
			}
		}
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s struct {
			Enabled *bool `json:"rpc-enabled"`
			Port    int   `json:"rpc-port"`
		}
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		return appConfig{found: true, enabled: s.Enabled == nil || *s.Enabled, port: s.Port}
	}
	return appConfig{}
}

func get(ctx context.Context, c *http.Client, method, u string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, b, nil
}

// probeQbit: /api/v2/app/version answers "v4.6.2", or 403 "Forbidden" when a login is needed.
func probeQbit(ctx context.Context, c *http.Client, base string) (string, bool, bool) {
	resp, b, err := get(ctx, c, http.MethodGet, base+"/api/v2/app/version", nil, map[string]string{"Referer": base})
	if err != nil {
		return "", false, false
	}
	s := strings.TrimSpace(string(b))
	switch {
	case resp.StatusCode == 200 && strings.HasPrefix(s, "v") && len(s) < 32:
		return s, false, true
	case resp.StatusCode == 403 && s == "Forbidden":
		return "", true, true
	}
	return "", false, false
}

// probeTransmission: the RPC answers 409 with a session id header before any call.
func probeTransmission(ctx context.Context, c *http.Client, base string) (string, bool, bool) {
	resp, _, err := get(ctx, c, http.MethodPost, base+"/transmission/rpc", []byte(`{"method":"session-get"}`), nil)
	if err != nil {
		return "", false, false
	}
	switch {
	case resp.StatusCode == 409 && resp.Header.Get("X-Transmission-Session-Id") != "":
		return "", false, true
	case resp.StatusCode == 401 && strings.Contains(resp.Header.Get("WWW-Authenticate"), "Transmission"):
		return "", true, true
	}
	return "", false, false
}

// probeDeluge: the Web UI's JSON-RPC answers auth.check_session with the request's id.
func probeDeluge(ctx context.Context, c *http.Client, base string) (string, bool, bool) {
	resp, b, err := get(ctx, c, http.MethodPost, base+"/json", []byte(`{"method":"auth.check_session","params":[],"id":7}`),
		map[string]string{"Content-Type": "application/json"})
	if err != nil || resp.StatusCode != 200 {
		return "", false, false
	}
	var r struct {
		ID     int  `json:"id"`
		Result bool `json:"result"`
	}
	if json.Unmarshal(b, &r) != nil || r.ID != 7 {
		return "", false, false
	}
	return "", !r.Result, true
}

// Describe is one line per app for reports and `noskill anime detect`.
func Describe(cs []ClientInfo) string {
	if len(cs) == 0 {
		return NoClientNote
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "%s", c.Name)
		if c.Version != "" {
			fmt.Fprintf(&b, " %s", c.Version)
		}
		for _, in := range c.Installs {
			scope := "for you"
			if in.Scope == "system" {
				scope = "for everyone"
			}
			fmt.Fprintf(&b, " · installed %s at %s", scope, in.Path)
		}
		if c.WebUI != "" {
			fmt.Fprintf(&b, " · Web UI %s", c.WebUI)
		}
		if c.Note != "" {
			fmt.Fprintf(&b, "\n  %s", c.Note)
		} else if c.Supported {
			b.WriteString("\n  ready")
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
