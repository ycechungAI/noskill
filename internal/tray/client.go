// Package tray is everything the tray app does except draw the menu (FR-I7, ADR-0004, ADR-0011).
// It has no state of its own and no CGO: it asks the daemon's /noskill/v1 API, over loopback with an
// admin token, and turns the answer into a menu model. cmd/noskill-tray draws that model with a
// native tray library.
package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"noskill/internal/config"
	"noskill/internal/platform"
	"noskill/internal/secrets"
)

// TokenName is the [[server.tokens]] entry the tray uses.
const TokenName = "tray"

// Status mirrors the parts of the daemon's snapshot the menu shows.
type Status struct {
	Primary    string `json:"primary"`
	JobsPaused bool   `json:"jobs_paused"`
	Approvals  []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
		Fields  []struct {
			Name    string `json:"name"`
			Label   string `json:"label"`
			Type    string `json:"type"`
			Prefill string `json:"prefill"`
		} `json:"fields"`
	} `json:"approvals"`
	Jobs []struct {
		ID      string    `json:"id"`
		Name    string    `json:"name"`
		Enabled bool      `json:"enabled"`
		Running bool      `json:"running"`
		Next    time.Time `json:"next"`
		Last    string    `json:"last"`
	} `json:"jobs"`
	Backends []struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	} `json:"backends"`
}

// Client talks to the daemon.
type Client struct {
	Base  string // http://127.0.0.1:11435
	Token string
	HTTP  *http.Client
}

// Connect finds the daemon from config.toml: its address and, when [[server.tokens]] is set, the
// admin token named "tray" (read from the OS keychain when it is a secret: reference).
func Connect(environ []string) (*Client, error) {
	home := ""
	for _, e := range environ {
		if v, ok := strings.CutPrefix(e, "NOSKILL_HOME="); ok {
			home = v
		}
	}
	dirs, err := platform.DefaultDirs(home)
	if err != nil {
		return nil, err
	}
	l, err := config.Load(config.Options{File: dirs.ConfigFile(), Env: environ})
	if err != nil {
		return nil, err
	}
	sec := secrets.NewManager(secrets.OSKeychain(), filepath.Join(dirs.Data, "secrets.names"))
	return FromConfig(l.Config.Server, sec.Get)
}

// FromConfig builds a client from [server] and a secret lookup.
func FromConfig(s config.Server, secret func(string) (string, bool)) (*Client, error) {
	host := s.Bind
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &Client{Base: "http://" + net.JoinHostPort(host, strconv.Itoa(s.Port)), HTTP: &http.Client{Timeout: 10 * time.Second}}
	if len(s.Tokens) == 0 {
		return c, nil
	}
	for _, t := range s.Tokens {
		if t.Name != TokenName {
			continue
		}
		if !t.Admin {
			return nil, fmt.Errorf("the %q token in [[server.tokens]] needs admin = true", TokenName)
		}
		v := t.Token
		if name, ok := config.SecretRef(v); ok {
			got, ok := secret(name)
			if !ok {
				return nil, fmt.Errorf("secret %s isn't set: run `noskill secrets set %s`", name, name)
			}
			v = got
		}
		c.Token = v
		return c, nil
	}
	return nil, fmt.Errorf("[[server.tokens]] is set, so the tray needs one named %q with admin = true", TokenName)
}

// PageURL is the status page.
func (c *Client) PageURL() string { return c.Base + "/ui" }

// ApprovalURL opens the status page at one approval, where its fields can be changed (FR-A6).
func (c *Client) ApprovalURL(id string) string { return c.PageURL() + "#a-" + url.PathEscape(id) }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ErrOffline{err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Message string }
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("noskill answered %s", resp.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// ErrOffline: the daemon isn't reachable.
type ErrOffline struct{ Err error }

func (e ErrOffline) Error() string {
	var ue *url.Error
	if errors.As(e.Err, &ue) {
		return "noskill isn't running (" + ue.Err.Error() + ")"
	}
	return "noskill isn't running (" + e.Err.Error() + ")"
}
func (e ErrOffline) Unwrap() error { return e.Err }

func (c *Client) Status(ctx context.Context) (Status, error) {
	var out struct{ Status Status }
	err := c.do(ctx, http.MethodGet, "/noskill/v1/status", nil, &out)
	return out.Status, err
}

// Answer approves or skips an approval.
func (c *Client) Answer(ctx context.Context, id string, approve bool) error {
	a := "skip"
	if approve {
		a = "approve"
	}
	return c.do(ctx, http.MethodPost, "/noskill/v1/approvals/"+url.PathEscape(id), map[string]string{"action": a}, nil)
}

// Job runs pause | resume | run for one job, or pause | resume for id "all".
func (c *Client) Job(ctx context.Context, id, action string) error {
	return c.do(ctx, http.MethodPost, "/noskill/v1/jobs/"+url.PathEscape(id)+"/"+action, nil, nil)
}
