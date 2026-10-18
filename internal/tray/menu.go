package tray

import (
	"fmt"
	"strings"
	"time"
)

// State is the dot in the tray.
type State int

const (
	Offline   State = iota // the daemon isn't reachable
	Idle                   // running, nothing to do
	Attention              // something waits for an answer
)

// Menu is what the tray draws (ADR-0004 "Menu (v1)").
type Menu struct {
	State     State
	Header    string // "noskill: running · qwen3:8b loaded"
	Tooltip   string
	Approvals []ApprovalEntry
	Jobs      []JobEntry
	Paused    bool // "pause all" is in effect
	Problem   string
}

type ApprovalEntry struct {
	ID    string
	Label string
	// Approve is the approve item's title: with editable fields (FR-A6) it says what approving
	// uses as is, e.g. "Approve: Save to /Volumes/RED/ANIME2026". A menu can't take typing, so
	// Editable adds "Change…", which opens the status page at this approval.
	Approve  string
	Editable bool
}

type JobEntry struct {
	ID      string
	Label   string // "anime-tracker: next 03:00 · last ok"
	Enabled bool
	Running bool
}

// Build turns a snapshot, or the error from fetching it, into a menu. now is used for "next 03:00".
func Build(st Status, err error, now time.Time) Menu {
	if err != nil {
		m := Menu{State: Offline, Header: "noskill: not running", Tooltip: "noskill: not running", Problem: err.Error()}
		var off ErrOffline
		if !asOffline(err, &off) {
			m.Header = "noskill: " + oneLine(err.Error(), 60)
		}
		return m
	}
	m := Menu{State: Idle, Paused: st.JobsPaused}
	h := "noskill: running"
	if st.Primary != "" {
		h += " · " + st.Primary + " loaded"
	}
	for _, b := range st.Backends {
		if !b.OK {
			h += " · " + b.ID + " unreachable"
		}
	}
	m.Header = h
	for _, a := range st.Approvals {
		e := ApprovalEntry{ID: a.ID, Label: oneLine(a.Summary, 70), Approve: "Approve", Editable: len(a.Fields) > 0}
		var parts []string
		for _, f := range a.Fields {
			if f.Prefill == "" {
				continue
			}
			l := f.Label
			if l == "" {
				l = f.Name
			}
			parts = append(parts, l+" "+f.Prefill)
		}
		if len(parts) > 0 {
			e.Approve = "Approve: " + oneLine(strings.Join(parts, ", "), 70)
		}
		m.Approvals = append(m.Approvals, e)
	}
	for _, j := range st.Jobs {
		var p []string
		switch {
		case j.Running:
			p = append(p, "running")
		case !j.Enabled:
			p = append(p, "paused")
		case !j.Next.IsZero():
			p = append(p, "next "+when(j.Next, now))
		}
		if j.Last != "" {
			p = append(p, "last "+j.Last)
		}
		l := j.Name
		if len(p) > 0 {
			l += ": " + strings.Join(p, " · ")
		}
		m.Jobs = append(m.Jobs, JobEntry{ID: j.ID, Label: l, Enabled: j.Enabled, Running: j.Running})
	}
	if len(m.Approvals) > 0 {
		m.State = Attention
		m.Tooltip = fmt.Sprintf("noskill: %d waiting for you", len(m.Approvals))
	} else {
		m.Tooltip = h
	}
	return m
}

func asOffline(err error, target *ErrOffline) bool {
	for err != nil {
		if e, ok := err.(ErrOffline); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func when(t, now time.Time) string {
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Mon 15:04")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
