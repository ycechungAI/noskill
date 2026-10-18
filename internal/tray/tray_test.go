package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"noskill/internal/config"
)

func TestFromConfigPicksTheTrayToken(t *testing.T) {
	c, err := FromConfig(config.Server{Port: 11435}, nil)
	if err != nil || c.Base != "http://127.0.0.1:11435" || c.Token != "" {
		t.Fatalf("%+v %v", c, err)
	}
	s := config.Server{Bind: "0.0.0.0", Port: 9000, Tokens: []config.Token{{Name: "owui", Token: "secret:X"}, {Name: "tray", Token: "secret:T", Admin: true}}}
	c, err = FromConfig(s, func(n string) (string, bool) { return map[string]string{"T": "tray-token-0123456789"}[n], n == "T" })
	if err != nil || c.Token != "tray-token-0123456789" || c.Base != "http://127.0.0.1:9000" {
		t.Fatalf("%+v %v", c, err)
	}
	for name, s := range map[string]config.Server{
		"no tray token": {Tokens: []config.Token{{Name: "owui", Token: "secret:X"}}},
		"not admin":     {Tokens: []config.Token{{Name: "tray", Token: "secret:T"}}},
	} {
		if _, err := FromConfig(s, func(string) (string, bool) { return "x", true }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := FromConfig(config.Server{Tokens: []config.Token{{Name: "tray", Token: "secret:T", Admin: true}}}, func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "secrets set") {
		t.Errorf("missing secret: %v", err)
	}
}

func TestClientAndMenu(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/noskill/v1/status":
			next := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
			w.Write([]byte(`{"status":{"primary":"qwen3:4b","jobs_paused":false,
			 "approvals":[{"id":"a1","kind":"anime","summary":"Frieren   S2:\napprove?",
			   "fields":[{"name":"dest","label":"Save to","type":"folder","prefill":"/Volumes/RED/ANIME2026"}]}],
			 "jobs":[{"id":"j1","name":"anime-tracker","enabled":true,"next":"` + next + `","last":"done"},{"id":"j2","name":"sweep","enabled":false}],
			 "backends":[{"id":"ollama","ok":true}]}}`))
		case "/noskill/v1/jobs/j2/run":
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "j2 is paused"}})
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := Build(st, nil, time.Now())
	if m.State != Attention || m.Header != "noskill: running · qwen3:4b loaded" || len(m.Approvals) != 1 || m.Approvals[0].Label != "Frieren S2: approve?" ||
		m.Approvals[0].Approve != "Approve: Save to /Volumes/RED/ANIME2026" || !m.Approvals[0].Editable {
		t.Errorf("%+v", m)
	}
	if u := c.ApprovalURL("a1"); u != srv.URL+"/ui#a-a1" {
		t.Errorf("approval url %s", u)
	}
	if len(m.Jobs) != 2 || !strings.HasPrefix(m.Jobs[0].Label, "anime-tracker: next ") || !strings.HasSuffix(m.Jobs[0].Label, "last done") || m.Jobs[1].Label != "sweep: paused" {
		t.Errorf("jobs %+v", m.Jobs)
	}
	if err := c.Answer(ctx, "a1", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Job(ctx, "all", "pause"); err != nil {
		t.Fatal(err)
	}
	if err := c.Job(ctx, "j2", "run"); err == nil || err.Error() != "j2 is paused" {
		t.Errorf("job error: %v", err)
	}
	if got[0] != "GET /noskill/v1/status auth=Bearer tok" || got[1] != "POST /noskill/v1/approvals/a1 auth=Bearer tok" || got[2] != "POST /noskill/v1/jobs/all/pause auth=Bearer tok" {
		t.Errorf("calls %v", got)
	}

	// Nothing listening: the menu says so instead of failing.
	srv.Close()
	_, err = c.Status(ctx)
	m = Build(Status{}, err, time.Now())
	if m.State != Offline || m.Header != "noskill: not running" || !strings.Contains(m.Problem, "isn't running") {
		t.Errorf("offline: %+v (%v)", m, err)
	}
}

func TestIcons(t *testing.T) {
	for _, s := range []State{Offline, Idle, Attention} {
		img, err := png.Decode(bytes.NewReader(Icon(s)))
		if err != nil || img.Bounds().Dx() != 32 {
			t.Fatalf("state %d: %v", s, err)
		}
		if _, _, _, a := img.At(16, 16).RGBA(); a == 0 {
			t.Errorf("state %d: the centre is empty", s)
		}
		ico := ICO(s)
		if len(ico) < 22 || ico[2] != 1 || ico[4] != 1 {
			t.Errorf("state %d: bad ICO header % x", s, ico[:22])
		}
	}
}
