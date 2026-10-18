// Package server is noskill's local API: the OpenAI- and Ollama-compatible proxy, noskill's own
// /noskill/v1 endpoints and the liveness probe (PROXY.md). Every request passes Host, Origin and
// token checks first (SECURITY.md T5).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"noskill/internal/app"
	"noskill/internal/backends"
	"noskill/internal/config"
)

// maxBody bounds a request body: a chat with a few large images fits, a flood doesn't.
const maxBody = 64 << 20

type Server struct {
	App     *app.App
	cfg     config.Server
	auth    *Auth
	Version string
}

// New builds the server. Tokens named "secret:NAME" are read from the keychain.
func New(a *app.App, version string) (*Server, error) {
	cfg := a.Cfg.Server
	auth, err := NewAuth(cfg, func(name string) (string, error) {
		if v, ok := a.Secrets.Get(name); ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %s isn't set: run `noskill secrets set %s`", name, name)
	})
	if err != nil {
		return nil, err
	}
	return &Server{App: a, cfg: cfg, auth: auth, Version: version}, nil
}

// Addr is where to listen.
func (s *Server) Addr() string {
	bind := s.cfg.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	return net.JoinHostPort(bind, strconv.Itoa(s.cfg.Port))
}

// Handler returns the full API behind the security checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.root)

	mux.HandleFunc("POST /v1/chat/completions", s.openAIChat)
	mux.HandleFunc("GET /v1/models", s.openAIModels)
	mux.HandleFunc("POST /v1/embeddings", s.passthroughV1)
	mux.HandleFunc("POST /v1/completions", s.passthroughV1)
	mux.HandleFunc("POST /v1/systemone", s.systemone)

	mux.HandleFunc("POST /api/chat", s.ollamaChat)
	mux.HandleFunc("POST /api/generate", s.ollamaGenerate)
	mux.HandleFunc("GET /api/tags", s.ollamaTags)
	mux.HandleFunc("POST /api/show", s.ollamaShow)
	mux.HandleFunc("GET /api/ps", s.passthroughAPI)
	mux.HandleFunc("GET /api/version", s.passthroughAPI)
	mux.HandleFunc("POST /api/embed", s.passthroughAPI)
	mux.HandleFunc("POST /api/embeddings", s.passthroughAPI)
	for _, p := range []string{"pull", "delete", "create", "copy", "push"} {
		mux.HandleFunc("POST /api/"+p, s.modelManagement)
	}
	mux.HandleFunc("DELETE /api/delete", s.modelManagement)

	mux.HandleFunc("GET /noskill/v1/health", s.health)
	mux.HandleFunc("POST /noskill/v1/feedback", s.feedback)
	mux.HandleFunc("GET /noskill/v1/runs/{id}", s.runInfo)
	mux.HandleFunc("GET /noskill/v1/approvals", s.listApprovals)
	mux.HandleFunc("POST /noskill/v1/approvals/{id}", s.answerApproval)
	mux.HandleFunc("GET /noskill/v1/events", s.events)
	mux.HandleFunc("GET /noskill/v1/status", s.admin(s.status))
	mux.HandleFunc("POST /noskill/v1/jobs/{id}/{action}", s.admin(s.jobAction))
	mux.HandleFunc("GET /ui", s.ui)

	return s.auth.Wrap(s.limitBody(mux))
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// root is the liveness probe. The `ollama` CLI starts with HEAD / and gives up on a 404 (S2).
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		apiError(w, r, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		io.WriteString(w, "noskill is running\n")
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			apiError(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request is larger than %d MB", maxBody>>20))
		} else {
			apiError(w, r, http.StatusBadRequest, "couldn't read the request body: "+err.Error())
		}
		return nil, false
	}
	return b, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// fail maps an error to the status PROXY.md promises and writes it in the right format.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		nb  *app.NoBackendError
		nm  *app.NoModelError
		ue  *app.UserError
		be  *backends.Error
		brq badRequest
	)
	switch {
	case errors.Is(err, app.ErrBusy):
		w.Header().Set("Retry-After", "5")
		apiError(w, r, http.StatusServiceUnavailable, "noskill is busy with another request; try again in a moment")
	case errors.As(err, &nb):
		w.Header().Set("Retry-After", "10")
		apiError(w, r, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &nm):
		apiError(w, r, http.StatusNotFound, err.Error())
	case errors.As(err, &brq), errors.As(err, &ue):
		apiError(w, r, http.StatusBadRequest, err.Error())
	case errors.As(err, &be):
		status := be.Status
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "10")
		}
		if status < 400 {
			status = http.StatusBadGateway
		}
		apiError(w, r, status, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		apiError(w, r, http.StatusGatewayTimeout, "the run took longer than its time budget: "+err.Error())
	case errors.Is(err, context.Canceled):
		// The client went away; nobody is reading.
	default:
		apiError(w, r, http.StatusInternalServerError, err.Error())
	}
}

// runSummary is a model listing entry's identity.
func trimName(n string) string { return strings.TrimPrefix(n, "noskill/") }

var _ = time.Now
