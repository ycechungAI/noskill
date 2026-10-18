package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"noskill/internal/config"
)

func handler(t *testing.T, s config.Server) http.Handler {
	t.Helper()
	a, err := NewAuth(s, func(name string) (string, error) {
		if name == "OWUI" {
			return "owui-token-0123456789", nil
		}
		return "", errors.New("no such secret")
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok " + ClientFrom(r.Context()).Name))
	}))
}

func do(h http.Handler, method, path, host string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHostAndOrigin(t *testing.T) {
	s := config.Defaults().Server
	s.AllowedHosts = []string{"host.docker.internal"}
	s.AllowedOrigins = []string{"chrome-extension://abc"}
	h := handler(t, s)
	for _, tc := range []struct {
		name, host string
		hdr        map[string]string
		want       int
	}{
		{"loopback", "127.0.0.1:11435", nil, 200},
		{"localhost", "localhost:11435", nil, 200},
		{"ipv6", "[::1]:11435", nil, 200},
		{"docker", "host.docker.internal:11435", nil, 200},
		{"rebinding", "evil.example:11435", nil, 403},
		{"lan ip", "192.168.1.5:11435", nil, 403},
		{"foreign origin", "127.0.0.1:11435", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin", "127.0.0.1:11435", map[string]string{"Origin": "null"}, 403},
		{"other local port", "127.0.0.1:11435", map[string]string{"Origin": "http://127.0.0.1:3000"}, 403},
		{"allowed origin", "127.0.0.1:11435", map[string]string{"Origin": "chrome-extension://abc"}, 200},
		{"own ui", "127.0.0.1:11435", map[string]string{"Origin": "http://127.0.0.1:11435"}, 200},
		{"no-cors cross-site", "127.0.0.1:11435", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"typed url", "127.0.0.1:11435", map[string]string{"Sec-Fetch-Site": "none"}, 200},
		// A simple-request POST from a web page (text/plain avoids preflight) still carries Origin.
		{"text/plain post", "127.0.0.1:11435", map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}, 403},
	} {
		if w := do(h, "POST", "/api/chat", tc.host, tc.hdr); w.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}
	w := do(h, "OPTIONS", "/v1/chat/completions", "127.0.0.1:11435", map[string]string{"Origin": "chrome-extension://abc"})
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "chrome-extension://abc" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
}

func TestTokens(t *testing.T) {
	s := config.Defaults().Server
	s.Tokens = []config.Token{{Name: "openwebui", Token: "secret:OWUI", Skills: []string{"research"}}}
	h := handler(t, s)
	if w := do(h, "GET", "/api/tags", "127.0.0.1", nil); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", w.Code)
	}
	if w := do(h, "GET", "/api/tags", "127.0.0.1", map[string]string{"Authorization": "Bearer wrong-token-0123456789"}); w.Code != 401 {
		t.Errorf("wrong token: %d", w.Code)
	}
	w := do(h, "GET", "/api/tags", "127.0.0.1", map[string]string{"Authorization": "Bearer owui-token-0123456789"})
	if w.Code != 200 || w.Body.String() != "ok openwebui" {
		t.Errorf("good token: %d %s", w.Code, w.Body)
	}
	// Without tokens, Authorization is ignored.
	h = handler(t, config.Defaults().Server)
	if w := do(h, "GET", "/api/tags", "127.0.0.1", map[string]string{"Authorization": "Bearer anything"}); w.Body.String() != "ok local" {
		t.Errorf("no tokens configured: %d %s", w.Code, w.Body)
	}
}

func TestLANBindNeedsToken(t *testing.T) {
	s := config.Defaults().Server
	s.Bind = "0.0.0.0"
	if _, err := NewAuth(s, nil); err == nil {
		t.Fatal("0.0.0.0 without a token accepted")
	}
	s.Tokens = []config.Token{{Name: "x", Token: "short"}}
	if _, err := NewAuth(s, nil); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestErrorFormats(t *testing.T) {
	h := handler(t, config.Defaults().Server)
	var oa struct {
		Error struct {
			Message string
			Type    string
			Code    int
		}
	}
	w := do(h, "POST", "/v1/chat/completions", "evil.example", nil)
	if json.Unmarshal(w.Body.Bytes(), &oa); oa.Error.Type != "permission_error" || oa.Error.Code != 403 {
		t.Errorf("openai format: %s", w.Body)
	}
	var ol struct{ Error string }
	w = do(h, "POST", "/api/chat", "evil.example", nil)
	if json.Unmarshal(w.Body.Bytes(), &ol); !strings.Contains(ol.Error, "Host") {
		t.Errorf("ollama format: %s", w.Body)
	}
}
