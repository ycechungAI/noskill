package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"noskill/internal/app"
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

// addJob: a JobSpec (app.JobSpec) as JSON, the same checks as `noskill schedule add`. The daemon
// picks the job up from the database within a few seconds (FR-J1).
func (s *Server) addJob(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in app.JobSpec
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad(`send {"target": a skill or a prompt, "every": "6h" or "cron": "0 3 * * *", "name"?, "prompt"?, "when"?, "notify"?}`))
		return
	}
	j, err := s.App.AddJob(r.Context(), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": j})
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

// brief: the morning digest (daily-brief skill), built in code with no model.
func (s *Server) brief(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 168 {
			s.fail(w, r, bad("hours: 1 to 168"))
			return
		}
		hours = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"hours": hours, "brief": s.App.Brief(r.Context(), hours), "at": time.Now()})
}

// reminderView is a reminder job as the page shows it.
type reminderView struct {
	Name     string    `json:"name"`
	Text     string    `json:"text"`
	Schedule string    `json:"schedule"`
	Next     time.Time `json:"next,omitzero"`
	Once     bool      `json:"once,omitempty"`
	Active   bool      `json:"active"`
}

func (s *Server) listReminders(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.App.Reminders(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []reminderView{}
	for _, j := range jobs {
		out = append(out, reminderView{j.Name, j.Target, j.Schedule, j.NextRunAt, j.Args.Once, j.Enabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": out})
}

// addReminder: {"text": "call Sam tomorrow at 5pm"}. The sentence is read in Go; when it can't tell
// when, the 400 says what to say instead.
func (s *Server) addReminder(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Text) == "" {
		s.fail(w, r, bad(`send {"text": "call Sam tomorrow at 5pm"}`))
		return
	}
	j, rem, err := s.App.AddReminder(r.Context(), in.Text)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": j.Name, "reminder": rem.Message, "when": rem.When, "next": j.NextRunAt})
}

func (s *Server) removeReminder(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RemoveReminder(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
