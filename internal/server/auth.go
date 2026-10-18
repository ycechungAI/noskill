// Package server is noskill's local API: the OpenAI- and Ollama-compatible proxy, the /noskill/v1
// API and the status page (PROXY.md).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"

	"noskill/internal/config"
)

// Client is who made a request: a token's name and its allowed skills, or "local" when no tokens
// are configured (PROXY.md "Security").
type Client struct {
	Name   string
	Skills []string // "*" = all
	Admin  bool     // sees all approvals and the status; set by [[server.tokens]] admin = true, and for "local"
}

func (c Client) MaySkill(name string) bool {
	return slices.Contains(c.Skills, "*") || slices.Contains(c.Skills, name)
}

type clientKey struct{}

// ClientFrom returns the authenticated client; handlers behind Auth always have one.
func ClientFrom(ctx context.Context) Client {
	c, _ := ctx.Value(clientKey{}).(Client)
	return c
}

type token struct {
	value []byte
	c     Client
}

// Auth checks Host, Origin and token on every request (SECURITY.md T5, "Local API protection").
type Auth struct {
	hosts   []string
	origins []string
	tokens  []token
}

// NewAuth builds the checks from [server]. secret resolves "secret:NAME" token values.
func NewAuth(s config.Server, secret func(name string) (string, error)) (*Auth, error) {
	a := &Auth{hosts: []string{"localhost", "127.0.0.1", "::1"}}
	for _, h := range s.AllowedHosts {
		a.hosts = append(a.hosts, strings.ToLower(strings.Trim(h, "[]")))
	}
	for _, o := range s.AllowedOrigins {
		a.origins = append(a.origins, strings.TrimSuffix(o, "/"))
	}
	for _, t := range s.Tokens {
		v := t.Token
		if name, ok := config.SecretRef(v); ok {
			got, err := secret(name)
			if err != nil {
				return nil, fmt.Errorf("server token %q: %w", t.Name, err)
			}
			v = got
		}
		if len(v) < 16 {
			return nil, fmt.Errorf("server token %q: shorter than 16 characters", t.Name)
		}
		skills := t.Skills
		if len(skills) == 0 {
			skills = []string{"*"}
		}
		a.tokens = append(a.tokens, token{value: []byte(v), c: Client{Name: t.Name, Skills: skills, Admin: t.Admin}})
	}
	if !config.IsLoopbackBind(s.Bind) && len(a.tokens) == 0 {
		return nil, errors.New("binding beyond this machine needs at least one [[server.tokens]] entry")
	}
	return a, nil
}

func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r.Host) {
			apiError(w, r, http.StatusForbidden, "rejected Host header "+fmt.Sprintf("%q", r.Host)+": add it to server.allowed_hosts if this client is yours")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !a.originOK(origin, r.Host) {
				apiError(w, r, http.StatusForbidden, "rejected Origin "+fmt.Sprintf("%q", origin)+": browser pages can't use noskill unless listed in server.allowed_origins")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else if sf := r.Header.Get("Sec-Fetch-Site"); sf == "cross-site" || sf == "same-site" {
			// A browser request without Origin (a no-cors GET from <img>, <script>, a link) from
			// another site.
			apiError(w, r, http.StatusForbidden, "rejected cross-site browser request")
			return
		}
		// The liveness probe answers without a token: the `ollama` CLI sends HEAD / first, and a
		// 401 there stops it. It reveals nothing but that something is listening.
		// The status page is static HTML and shows nothing itself: the data calls it makes carry the
		// token.
		if (r.URL.Path == "/" || r.URL.Path == "/ui") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, Client{Name: "probe"})))
			return
		}
		c, ok := a.client(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="noskill"`)
			apiError(w, r, http.StatusUnauthorized, "missing or wrong token (Authorization: Bearer <token>)")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, c)))
	})
}

func (a *Auth) hostOK(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	return h != "" && slices.Contains(a.hosts, h)
}

// originOK: listed in allowed_origins, or the page noskill serves itself (/ui), whose origin is
// this request's own (already checked) Host.
func (a *Auth) originOK(origin, host string) bool {
	o := strings.TrimSuffix(origin, "/")
	if slices.Contains(a.origins, o) {
		return true
	}
	return o == "http://"+strings.ToLower(host)
}

func (a *Auth) client(r *http.Request) (Client, bool) {
	if len(a.tokens) == 0 {
		return Client{Name: "local", Skills: []string{"*"}, Admin: true}, true // Authorization is ignored
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return Client{}, false
	}
	var match Client
	found := 0
	for _, t := range a.tokens { // compare against every token; no early exit
		if subtle.ConstantTimeCompare([]byte(got), t.value) == 1 {
			match = t.c
			found = 1
		}
	}
	return match, found == 1
}

// apiError writes an error in the format the client expects (PROXY.md "Errors"): OpenAI on /v1,
// Ollama on /api.
func apiError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/noskill/v1/") {
		typ := map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
			404: "not_found_error", 502: "api_error", 503: "overloaded_error", 504: "timeout_error"}[status]
		if typ == "" {
			typ = "api_error"
		}
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": status}})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
