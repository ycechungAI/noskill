package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/platform"
	"noskill/internal/store"
)

// ---- /noskill/v1/health ----

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, errs := s.App.Models(ctx)
	type be struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	var backendsOut []be
	for _, nb := range s.App.Backends {
		e := be{ID: nb.ID, Kind: nb.Kind, OK: true}
		for _, err := range errs {
			if strings.HasPrefix(err.Error(), nb.ID+": ") {
				e.OK, e.Error = false, strings.TrimPrefix(err.Error(), nb.ID+": ")
			}
		}
		backendsOut = append(backendsOut, e)
	}
	pending, _ := s.App.Approvals.List(ctx, false, 0)
	sb := platform.NewSandbox().Strength()
	status := "ok"
	for _, b := range backendsOut {
		if !b.OK {
			status = "degraded"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status, "version": s.Version, "backends": backendsOut, "approvals_pending": len(pending),
		"skills":  len(s.App.Skills.All()),
		"sandbox": map[string]any{"mechanism": sb.Mechanism, "files": sb.Files, "network": sb.Network, "limits": sb.Limits},
	})
}

// ---- /noskill/v1/feedback ----

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		RunID    string `json:"run_id"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Rating  string `json:"rating"` // up | down
		Comment string `json:"comment"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad("the request body isn't valid JSON: %v", err))
		return
	}
	rating := map[string]string{"up": "good", "down": "bad"}[in.Rating]
	if rating == "" {
		s.fail(w, r, bad(`"rating" must be "up" or "down"`))
		return
	}
	ctx := r.Context()
	client := ClientFrom(ctx).Name
	var run store.Run
	var err error
	switch {
	case in.RunID != "":
		run, err = s.App.DB.GetRun(ctx, in.RunID)
	case len(in.Messages) > 0:
		var msgs []chatMessage
		for _, m := range in.Messages {
			msgs = append(msgs, chatMessage{Role: m.Role, Content: m.Content})
		}
		run, err = s.App.DB.LatestRunByFP(ctx, conversationFP(msgs))
	default:
		s.fail(w, r, bad(`give "run_id" or "messages"`))
		return
	}
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.Client != client) { // not yours looks the same as not there
		apiError(w, r, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done, err := s.App.Rate(ctx, run.ID, rating, in.Comment)
	if err != nil {
		s.fail(w, r, bad("%v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": done.ID, "outcome": done.Outcome})
}

// ---- /noskill/v1/runs/{id} ----

func (s *Server) runInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.App.DB.GetRun(ctx, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.Client != ClientFrom(ctx).Name) {
		apiError(w, r, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	skills, _ := s.App.DB.RunSkills(ctx, run.ID)
	effects, _ := s.App.DB.UndoEntries(ctx, run.ID)
	type fx struct {
		Op   string `json:"op"`
		From string `json:"from,omitempty"`
		To   string `json:"to,omitempty"`
	}
	var fxs []fx
	for _, e := range effects {
		fxs = append(fxs, fx{e.Op, e.From, e.To})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": run.ID, "status": run.Status, "model": run.DriverModel, "skills": skills, "steps": run.Steps, "tokens": run.Tokens, "ms": run.Ms,
		"outcome": run.Outcome, "outcome_source": run.OutcomeSource, "started_at": run.StartedAt, "ended_at": run.EndedAt, "file_effects": fxs,
	})
}

// ---- approvals ----

// approvalClient is the token that triggered an approval ("" for ones made by jobs).
func approvalClient(it approvals.Item) string {
	var d struct{ Client string }
	json.Unmarshal(it.Details, &d)
	return d.Client
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := s.App.Approvals.List(ctx, false, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cl := ClientFrom(ctx)
	out := []approvals.Item{}
	for _, it := range items {
		if cl.Admin || approvalClient(it) == cl.Name { // a client sees only the approvals it caused
			it.ResumeToken = "" // internal, and may hold the conversation
			out = append(out, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
}

func (s *Server) answerApproval(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Action string            `json:"action"`
		Fields map[string]string `json:"fields"`
		Note   string            `json:"note"`
	}
	if err := json.Unmarshal(body, &in); err != nil || (in.Action != "approve" && in.Action != "skip") {
		s.fail(w, r, bad(`"action" must be "approve" or "skip"`))
		return
	}
	ctx := r.Context()
	it, err := s.App.Approvals.Get(ctx, r.PathValue("id"))
	if errors.Is(err, approvals.ErrNotFound) || (err == nil && !ClientFrom(ctx).Admin && approvalClient(it) != ClientFrom(ctx).Name) {
		apiError(w, r, http.StatusNotFound, "no such approval")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Action == "approve" && strings.Contains(string(it.Details), `"exec"`) && !ClientFrom(ctx).Admin {
		// Permissions with exec are never approvable from chat or HTTP (PROXY.md).
		apiError(w, r, http.StatusForbidden, "this needs the tray, the status page with an admin token, or `noskill approvals approve`")
		return
	}
	done, err := s.App.Approvals.Resolve(ctx, it.ID, approvals.Answer{Approve: in.Action == "approve", Set: in.Fields, Note: in.Note, Via: "api"})
	switch {
	case errors.Is(err, approvals.ErrResolved), errors.Is(err, approvals.ErrExpired):
		apiError(w, r, http.StatusConflict, err.Error())
	case err != nil:
		apiError(w, r, http.StatusBadRequest, err.Error())
	default:
		done.ResumeToken = ""
		writeJSON(w, http.StatusOK, done)
	}
}

// ---- /noskill/v1/events ----

// events streams server-sent events for the tray and status page: approvals created or answered,
// runs finished. It reads the database every two seconds, so it also sees changes made by other
// processes (the CLI answering an approval).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	me := ClientFrom(ctx)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}
	send("hello", map[string]string{"version": s.Version})
	rc.Flush()
	since := time.Now()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n := s.pollEvents(ctx, me, since, send)
		since = time.Now().Add(-100 * time.Millisecond)
		if n == 0 {
			if idle++; idle%7 == 0 {
				fmt.Fprint(w, ": keepalive\n\n")
			}
		}
		rc.Flush()
	}
}

func (s *Server) pollEvents(ctx context.Context, me Client, since time.Time, send func(string, any)) int {
	n := 0
	ms := since.UnixMilli()
	db := s.App.DB
	rows, err := db.QueryContext(ctx, `SELECT id, status, summary, created_at, resolved_at FROM approvals WHERE created_at > ? OR resolved_at > ? ORDER BY created_at`, ms, ms)
	if err == nil {
		type ev struct {
			ID, Status, Summary string
			Created             int64
			Resolved            sql.NullInt64
		}
		var evs []ev
		for rows.Next() {
			var e ev
			if rows.Scan(&e.ID, &e.Status, &e.Summary, &e.Created, &e.Resolved) == nil {
				evs = append(evs, e)
			}
		}
		rows.Close()
		for _, e := range evs {
			it, err := s.App.Approvals.Get(ctx, e.ID)
			if err != nil || (!me.Admin && approvalClient(it) != me.Name) {
				continue
			}
			kind := "approval.created"
			if e.Created <= ms {
				kind = "approval.resolved"
			}
			send(kind, map[string]string{"id": e.ID, "status": e.Status, "summary": e.Summary})
			n++
		}
	}
	rows, err = db.QueryContext(ctx, `SELECT id, status, COALESCE(driver_model, ''), COALESCE(client, '') FROM runs WHERE ended_at > ? AND source <> 'background' ORDER BY ended_at`, ms)
	if err == nil {
		defer rows.Close()
		var out [][4]string
		for rows.Next() {
			var a, b, c, d string
			if rows.Scan(&a, &b, &c, &d) == nil && (me.Admin || d == me.Name) {
				out = append(out, [4]string{a, b, c, d})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		for _, o := range out {
			send("run.finished", map[string]string{"id": o[0], "status": o[1], "model": o[2]})
			n++
		}
	}
	return n
}
