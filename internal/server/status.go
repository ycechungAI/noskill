package server

import (
	_ "embed"
	"net/http"
)

// The status page and the calls the tray makes (FR-I6, FR-I7, ADR-0011).

//go:embed ui/index.html
var uiPage []byte

// admin restricts a handler to admin clients: the unauthenticated "local" client (no tokens are
// configured, so only this machine can reach the port) and tokens with admin = true.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ClientFrom(r.Context()).Admin {
			apiError(w, r, http.StatusForbidden, "this needs a token with admin = true in [[server.tokens]]")
			return
		}
		next(w, r)
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.Version, "status": s.App.Status(r.Context())})
}

// jobAction: pause | resume | run for one job, or job "all" with pause | resume.
func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	ctx, id, action := r.Context(), r.PathValue("id"), r.PathValue("action")
	var n int
	var err error
	switch {
	case id == "all" && action == "pause":
		n, err = s.App.PauseAllJobs(ctx)
	case id == "all" && action == "resume":
		n, err = s.App.ResumeAllJobs(ctx)
	case id == "all":
		apiError(w, r, http.StatusBadRequest, `"all" takes pause or resume`)
		return
	default:
		err = s.App.JobAction(ctx, id, action)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "jobs": n})
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// Own origin only: no external scripts, styles or frames; the page can only talk to this server.
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.Write(uiPage)
}
