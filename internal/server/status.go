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

// draftJob fills in the Add a job form from {"text": a plain-language description} with the local model.
func (s *Server) draftJob(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad(`send {"text": what the job should do and when}`))
		return
	}
	d, err := s.App.DraftJob(r.Context(), in.Text)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "draft": d})
}

// draftModel reports the Auto fill model choice: GET for the choice, POST {"model": name} to set it ("" = automatic).
func (s *Server) draftModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.App.DraftModelInfo(r.Context()))
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad(`send {"model": an installed chat model, or "" for automatic}`))
		return
	}
	if err := s.App.SetDraftModel(r.Context(), in.Model); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.App.DraftModelInfo(r.Context()))
}

// editJob rewrites a job from the same spec as adding one (FR-J8). Its history and enabled state stay.
func (s *Server) editJob(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in app.JobSpec
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad(`send the job's settings, as for adding one`))
		return
	}
	j, err := s.App.UpdateJob(r.Context(), r.PathValue("id"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": j})
}

// listJobBackups lists the backups folder, newest first (FR-J6).
func (s *Server) listJobBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.ListJobBackups()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []app.JobBackupInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list, "folder": s.App.JobBackupsDir()})
}

// backupJobs backs up the job list now, into the backups folder.
func (s *Server) backupJobs(w http.ResponseWriter, r *http.Request) {
	info, err := s.App.BackupJobs(r.Context(), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "backup": info})
}

// restoreJobs restores {"file": a name in the backups folder} or {"backup": a backup file's contents}.
func (s *Server) restoreJobs(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		File   string          `json:"file"`
		Backup json.RawMessage `json:"backup"`
	}
	if err := json.Unmarshal(body, &in); err != nil || (in.File == "") == (len(in.Backup) == 0) {
		s.fail(w, r, bad(`send {"file": a backup's name} or {"backup": the contents of a backup file}`))
		return
	}
	var rep app.RestoreReport
	var err error
	if in.File != "" {
		rep, err = s.App.RestoreJobBackup(r.Context(), in.File)
	} else {
		var b app.JobBackup
		if b, err = app.ParseJobBackup(in.Backup); err == nil {
			rep, err = s.App.RestoreJobs(r.Context(), b)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": rep.Added, "skipped": rep.Skipped})
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
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-src http://127.0.0.1:8765 http://localhost:8765 http://127.0.0.1:8767 http://localhost:8767; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
