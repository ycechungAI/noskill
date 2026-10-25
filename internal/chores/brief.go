package chores

import (
	"fmt"
	"strings"
	"time"
)

// BriefFacts is what the morning digest reports. The app gathers it from noskill's own state; the
// text below is built in code, so the digest is the same every time and needs no model.
type BriefFacts struct {
	Since            time.Time
	JobsRan          int
	JobsFailed       []string // names of jobs whose latest run failed in the period
	Filed            []string // "Show episode 3", filed in the period
	ApprovalsWaiting int
	LowVolumes       string // disk-watch's alert text; "" when every drive is fine
	Next             []string
}

// Text writes the digest, a few short lines. Nothing to report says so in one line.
func (f BriefFacts) Text(now time.Time) string {
	var lines []string
	since := "since " + f.Since.Local().Format("Mon 15:04")
	if f.JobsRan > 0 || len(f.JobsFailed) > 0 {
		l := fmt.Sprintf("%d scheduled %s ran %s", f.JobsRan, plural(f.JobsRan, "job", "jobs"), since)
		if n := len(f.JobsFailed); n > 0 {
			l += fmt.Sprintf(" and %d failed (%s)", n, strings.Join(f.JobsFailed, ", "))
		}
		lines = append(lines, l+".")
	}
	if n := len(f.Filed); n > 0 {
		show := f.Filed
		more := ""
		if n > 6 {
			show, more = f.Filed[:6], fmt.Sprintf(" and %d more", n-6)
		}
		lines = append(lines, fmt.Sprintf("%d %s filed: %s%s.", n, plural(n, "episode", "episodes"), strings.Join(show, ", "), more))
	}
	if f.ApprovalsWaiting > 0 {
		lines = append(lines, fmt.Sprintf("%d %s waiting for you (noskill approvals list).", f.ApprovalsWaiting, plural(f.ApprovalsWaiting, "approval", "approvals")))
	}
	if f.LowVolumes != "" {
		lines = append(lines, "Low disk space: "+f.LowVolumes+".")
	}
	if len(lines) == 0 {
		lines = append(lines, "Nothing needs you "+since+".")
	}
	if len(f.Next) > 0 {
		lines = append(lines, "Next: "+strings.Join(f.Next, "; ")+".")
	}
	return strings.Join(lines, "\n")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
