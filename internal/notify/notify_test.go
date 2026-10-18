package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"noskill/internal/config"
	"noskill/internal/platform"
)

func TestNtfy(t *testing.T) {
	var got http.Header
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	n := &Ntfy{URL: srv.URL + "/topic", Token: "tk_123"}
	err := n.Send(context.Background(), Message{Title: "Approval needed – 🎬", Body: "files wants to run trash", Urgent: true})
	if err != nil || body != "files wants to run trash" || got.Get("Authorization") != "Bearer tk_123" || got.Get("Priority") != "high" {
		t.Fatalf("%q %v %v", body, got, err)
	}
	if strings.ContainsAny(got.Get("Title"), "–🎬") {
		t.Errorf("non-ASCII in a header: %q", got.Get("Title"))
	}
}

func TestTelegramAndDiscordPayloadsAndSecretHygiene(t *testing.T) {
	var path, ct string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ct = r.URL.Path, r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&payload)
	}))
	defer srv.Close()
	tg := &Telegram{BotToken: "123:SECRETTOKEN", ChatID: "42", BaseURL: srv.URL}
	if err := tg.Send(context.Background(), Message{Title: "T", Body: "B", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if path != "/bot123:SECRETTOKEN/sendMessage" || payload["chat_id"] != "42" || payload["text"] != "T\nB" || payload["disable_notification"] != false || ct != "application/json" {
		t.Errorf("telegram: %s %v", path, payload)
	}
	d := &Discord{Webhook: srv.URL + "/api/webhooks/1/SECRETHOOK"}
	if err := d.Send(context.Background(), Message{Title: "T", Body: "@everyone hi"}); err != nil {
		t.Fatal(err)
	}
	if am := payload["allowed_mentions"].(map[string]any)["parse"].([]any); len(am) != 0 {
		t.Errorf("mentions allowed: %v", payload)
	}
	// A failing request must not repeat the credential in its error.
	srv.Close()
	for _, c := range []Channel{tg, d} {
		err := c.Send(context.Background(), Message{Title: "x", Body: "y"})
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s error = %v", c.Name(), err)
		}
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer bad.Close()
	err := (&Telegram{BotToken: "123:SECRETTOKEN", ChatID: "1", BaseURL: bad.URL}).Send(context.Background(), Message{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("status error = %v", err)
	}
}

func TestEmail(t *testing.T) {
	var from, msg string
	var to []string
	var auth smtp.Auth
	e := &Email{SMTP: "smtp.example.com:587", User: "me@example.com", Password: "pw-123456", To: "you@example.com",
		SendMail: func(addr string, a smtp.Auth, f string, t []string, m []byte) error {
			from, to, msg, auth = f, t, string(m), a
			return nil
		}}
	if err := e.Send(context.Background(), Message{Title: "Sub\r\nBcc: evil@example.com", Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if from != "me@example.com" || to[0] != "you@example.com" || auth == nil || strings.Contains(msg, "\r\nBcc:") {
		t.Errorf("%s %v %q", from, to, msg)
	}
	e.SendMail = func(string, smtp.Auth, string, []string, []byte) error {
		return errors.New("auth failed for pw-123456")
	}
	if err := e.Send(context.Background(), Message{}); err == nil || strings.Contains(err.Error(), "pw-123456") {
		t.Errorf("password in error: %v", err)
	}
	e.Password = ""
	if err := e.Send(context.Background(), Message{}); err == nil || strings.Contains(err.Error(), "[secret]") {
		t.Errorf("empty password mangled the error: %v", err)
	}
	if err := (&Email{SMTP: "nope"}).Send(context.Background(), Message{}); err == nil {
		t.Error("bad smtp address accepted")
	}
}

type fakeOS struct {
	mu   sync.Mutex
	sent []platform.Notification
}

func (f *fakeOS) SupportsActions() bool { return false }
func (f *fakeOS) Notify(_ context.Context, n platform.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	return nil
}

type failing struct{}

func (failing) Name() string                        { return "broken" }
func (failing) Send(context.Context, Message) error { return errors.New("down") }

type slow struct{}

func (slow) Name() string { return "slow" }
func (slow) Send(ctx context.Context, _ Message) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestHubIsolatesFailures(t *testing.T) {
	f := &fakeOS{}
	h := &Hub{Channels: []Channel{failing{}, OSChannel{N: f}, slow{}}, Timeout: 100 * time.Millisecond}
	errs := h.Send(context.Background(), Message{ID: "a1", Title: "t", Body: "b", Urgent: true})
	if len(errs) != 2 || len(f.sent) != 1 || !f.sent[0].Sound || f.sent[0].ID != "a1" {
		t.Fatalf("errs=%v sent=%+v", errs, f.sent)
	}
	if !strings.HasPrefix(errs[0].Error(), "broken: ") || !strings.HasPrefix(errs[1].Error(), "slow: ") {
		t.Errorf("errors must name their channel: %v", errs)
	}
}

func TestFromConfig(t *testing.T) {
	cfg := config.Notify{OS: true,
		Ntfy:     &config.Ntfy{URL: "https://ntfy.sh/x", Token: "secret:NTFY"},
		Telegram: &config.Telegram{BotToken: "secret:MISSING", ChatID: "1"},
		Discord:  &config.Discord{Webhook: "secret:HOOK"}}
	secrets := func(n string) (string, error) {
		if n == "MISSING" {
			return "", errors.New("secret MISSING isn't set")
		}
		return "value-" + n, nil
	}
	chans, errs := FromConfig(cfg, secrets, &fakeOS{})
	var names []string
	for _, c := range chans {
		names = append(names, c.Name())
	}
	if strings.Join(names, ",") != "os,ntfy,discord" || len(errs) != 1 || !strings.Contains(errs[0].Error(), "telegram") {
		t.Errorf("channels=%v errs=%v", names, errs)
	}
	if n := chans[1].(*Ntfy); n.Token != "value-NTFY" {
		t.Errorf("secret not resolved: %q", n.Token)
	}
	cfg.OS = false
	if chans, _ := FromConfig(cfg, secrets, &fakeOS{}); chans[0].Name() == "os" {
		t.Error("os channel present though disabled")
	}
}
