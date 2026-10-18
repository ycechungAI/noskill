// Package notify sends messages to the places a person reads them: the OS, ntfy, Telegram,
// Discord and email (FR-J4, FR-A6). Channels carry no buttons in v0.1; approvals are answered with
// `noskill approvals approve <id>` (S7: macOS action buttons are unproven).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"time"

	"noskill/internal/config"
	"noskill/internal/platform"
)

type Message struct {
	ID     string // approval or job id
	Title  string
	Body   string
	Urgent bool // needs a person: sound / high priority
}

type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Hub fans a message out to every channel. One channel failing never stops the others.
type Hub struct {
	Channels []Channel
	Timeout  time.Duration // per channel; default 15 s
}

// Send returns one error per failed channel, named.
func (h *Hub) Send(ctx context.Context, m Message) []error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	errs := make([]error, len(h.Channels))
	var wg sync.WaitGroup
	for i, c := range h.Channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := c.Send(cctx, m); err != nil {
				errs[i] = fmt.Errorf("%s: %w", c.Name(), err)
			}
		}()
	}
	wg.Wait()
	var out []error
	for _, e := range errs {
		if e != nil {
			out = append(out, e)
		}
	}
	return out
}

// SendTo sends only to the named channels (a job's --notify list). A name with no configured
// channel is reported, so a job set to notify by ntfy doesn't fail silently.
func (h *Hub) SendTo(ctx context.Context, names []string, m Message) []error {
	var picked []Channel
	var errs []error
	for _, n := range names {
		found := false
		for _, c := range h.Channels {
			if c.Name() == n {
				picked, found = append(picked, c), true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("%s: not set up in [notify]", n))
		}
	}
	sub := &Hub{Channels: picked, Timeout: h.Timeout}
	return append(errs, sub.Send(ctx, m)...)
}

// SecretResolver turns "secret:NAME" into its value.
type SecretResolver func(name string) (string, error)

func resolve(v string, secrets SecretResolver) (string, error) {
	if name, ok := config.SecretRef(v); ok {
		if secrets == nil {
			return "", fmt.Errorf("secret %s can't be read here", name)
		}
		return secrets(name)
	}
	return v, nil
}

// FromConfig builds the configured channels. A channel that can't be built is reported and left
// out; the others still work.
func FromConfig(c config.Notify, secrets SecretResolver, os platform.Notifier) ([]Channel, []error) {
	var chans []Channel
	var errs []error
	add := func(ch Channel, err error) {
		if err != nil {
			errs = append(errs, err)
			return
		}
		chans = append(chans, ch)
	}
	if c.OS && os != nil {
		chans = append(chans, OSChannel{N: os})
	}
	if c.Ntfy != nil {
		tok, err := resolve(c.Ntfy.Token, secrets)
		if err != nil {
			add(nil, fmt.Errorf("ntfy: %w", err))
		} else {
			add(&Ntfy{URL: c.Ntfy.URL, Token: tok}, nil)
		}
	}
	if c.Telegram != nil {
		tok, err := resolve(c.Telegram.BotToken, secrets)
		if err != nil {
			add(nil, fmt.Errorf("telegram: %w", err))
		} else {
			add(&Telegram{BotToken: tok, ChatID: c.Telegram.ChatID}, nil)
		}
	}
	if c.Discord != nil {
		hook, err := resolve(c.Discord.Webhook, secrets)
		if err != nil {
			add(nil, fmt.Errorf("discord: %w", err))
		} else {
			add(&Discord{Webhook: hook}, nil)
		}
	}
	if c.Email != nil {
		pw, err := resolve(c.Email.Password, secrets)
		if err != nil {
			add(nil, fmt.Errorf("email: %w", err))
		} else {
			add(&Email{SMTP: c.Email.SMTP, User: c.Email.User, Password: pw, To: c.Email.To}, nil)
		}
	}
	return chans, errs
}

// ---- OS ----

type OSChannel struct{ N platform.Notifier }

func (OSChannel) Name() string { return "os" }

func (c OSChannel) Send(ctx context.Context, m Message) error {
	return c.N.Notify(ctx, platform.Notification{ID: m.ID, Title: m.Title, Body: m.Body, Sound: m.Urgent})
}

// ---- HTTP helpers ----

var httpClient = &http.Client{Timeout: 20 * time.Second}

// post sends a request and reports failure without the URL: webhook URLs and bot tokens are
// credentials and must not reach logs or the model.
func post(ctx context.Context, what, rawURL string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: bad URL", what)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drops the URL
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s: status %d: %s", what, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ---- ntfy ----

type Ntfy struct{ URL, Token string }

func (*Ntfy) Name() string { return "ntfy" }

func (n *Ntfy) Send(ctx context.Context, m Message) error {
	h := map[string]string{"Title": m.Title, "Content-Type": "text/plain; charset=utf-8"}
	if m.Urgent {
		h["Priority"] = "high"
		h["Tags"] = "bell"
	}
	if n.Token != "" {
		h["Authorization"] = "Bearer " + n.Token
	}
	return post(ctx, "ntfy", n.URL, []byte(m.Body), asciiHeaders(h))
}

// asciiHeaders keeps header values valid: HTTP headers can't carry newlines or raw non-ASCII text.
func asciiHeaders(h map[string]string) map[string]string {
	for k, v := range h {
		v = strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
		h[k] = strings.Map(func(r rune) rune {
			if r > 126 {
				return '?'
			}
			return r
		}, v)
	}
	return h
}

// ---- Telegram ----

type Telegram struct {
	BotToken, ChatID string
	BaseURL          string // tests; default https://api.telegram.org
}

func (*Telegram) Name() string { return "telegram" }

func (t *Telegram) Send(ctx context.Context, m Message) error {
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": m.Title + "\n" + m.Body, "disable_notification": !m.Urgent})
	return post(ctx, "telegram", base+"/bot"+t.BotToken+"/sendMessage", body, map[string]string{"Content-Type": "application/json"})
}

// ---- Discord ----

type Discord struct{ Webhook string }

func (*Discord) Name() string { return "discord" }

func (d *Discord) Send(ctx context.Context, m Message) error {
	text := "**" + m.Title + "**\n" + m.Body
	if len(text) > 1900 { // Discord's limit is 2000
		text = text[:1900] + "…"
	}
	// allowed_mentions empty: a message built from a web page can't ping @everyone.
	body, _ := json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
	return post(ctx, "discord", d.Webhook, body, map[string]string{"Content-Type": "application/json"})
}

// ---- Email ----

type Email struct {
	SMTP, User, Password, To string
	SendMail                 func(addr string, a smtp.Auth, from string, to []string, msg []byte) error // tests
}

func (*Email) Name() string { return "email" }

func (e *Email) Send(ctx context.Context, m Message) error {
	host, _, ok := strings.Cut(e.SMTP, ":")
	if !ok || host == "" {
		return errors.New("email: smtp must be host:port")
	}
	clean := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	msg := "From: " + clean(e.User) + "\r\nTo: " + clean(e.To) + "\r\nSubject: " + clean(m.Title) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.Body + "\r\n"
	var auth smtp.Auth
	if e.Password != "" {
		auth = smtp.PlainAuth("", e.User, e.Password, host) // net/smtp refuses to send it without TLS
	}
	send := e.SendMail
	if send == nil {
		send = smtp.SendMail // upgrades with STARTTLS when the server offers it
	}
	done := make(chan error, 1)
	go func() { done <- send(e.SMTP, auth, e.User, []string{e.To}, []byte(msg)) }()
	select {
	case err := <-done:
		if err != nil {
			msg := err.Error()
			if e.Password != "" {
				msg = strings.ReplaceAll(msg, e.Password, "[secret]")
			}
			return fmt.Errorf("email: %s", msg)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
