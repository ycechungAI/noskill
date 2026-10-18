package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"noskill/internal/platform"
)

// Preconditions (FR-J5): a job runs only while all of its preconditions hold. When one doesn't at
// the scheduled time, the occurrence waits, checked on every tick, until it holds or the next
// occurrence comes; then it is recorded as unmet and skipped (PLATFORMS.md: "jobs wait or skip
// cleanly"). Retries wait the same way. `schedule run` starts a job now without checking them.
//
//	volume:<label>     the drive is mounted (and is the one bound to that label)
//	ac-power           not on battery
//	unmetered          not on a metered connection
//	window:HH:MM-HH:MM only inside these local hours (may cross midnight, e.g. window:22:00-06:00)
//
// A condition the OS can't report (power on a desktop, metered on macOS) counts as met.

// ParsePrecondition checks one --when value and returns it in stored form.
func ParsePrecondition(s string) (string, error) {
	s = strings.TrimSpace(s)
	kind, arg, _ := strings.Cut(s, ":")
	switch kind {
	case "ac-power", "unmetered":
		if arg != "" {
			return "", fmt.Errorf("--when %s takes no value", kind)
		}
		return kind, nil
	case "volume":
		if arg == "" || strings.ContainsAny(arg, `/\`) {
			return "", fmt.Errorf("--when volume:<label>, e.g. volume:RED")
		}
		return s, nil
	case "window":
		if _, _, err := parseWindow(arg); err != nil {
			return "", err
		}
		return s, nil
	}
	return "", fmt.Errorf("--when %q: want volume:<label>, ac-power, unmetered or window:HH:MM-HH:MM", s)
}

func parseWindow(arg string) (from, to int, err error) {
	a, b, ok := strings.Cut(arg, "-")
	bad := fmt.Errorf("--when window:%s: want window:HH:MM-HH:MM in local time, e.g. window:22:00-06:00", arg)
	if !ok {
		return 0, 0, bad
	}
	if from, err = hhmm(a); err != nil {
		return 0, 0, bad
	}
	if to, err = hhmm(b); err != nil || to == from {
		return 0, 0, bad
	}
	return from, to, nil
}

// hhmm parses HH:MM into minutes after midnight.
func hhmm(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, errors.New("bad time")
	}
	return hh*60 + mm, nil
}

// InWindow says whether t falls in window "HH:MM-HH:MM" (local time; the end is exclusive).
func InWindow(arg string, t time.Time) bool {
	from, to, err := parseWindow(arg)
	if err != nil {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	if from < to {
		return now >= from && now < to
	}
	return now >= from || now < to // crosses midnight
}

// Checker tells whether a precondition holds now; why says what is missing when it doesn't.
type Checker interface {
	Check(ctx context.Context, cond string, now time.Time) (ok bool, why string)
}

// PlatformChecker checks preconditions on this machine. Results of the slower checks (drives,
// network) are kept for CacheFor, since a waiting job is checked on every tick.
type PlatformChecker struct {
	Volumes  platform.Volumes // resolves and binds volume:<label>; nil = only the label is checked
	OnAC     func() (on, known bool)
	Metered  func() (metered, known bool)
	CacheFor time.Duration // default 30 s

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	ok  bool
	why string
	at  time.Time
}

func (c *PlatformChecker) Check(ctx context.Context, cond string, now time.Time) (bool, string) {
	kind, arg, _ := strings.Cut(cond, ":")
	if kind == "window" {
		if InWindow(arg, now) {
			return true, ""
		}
		return false, "outside " + arg
	}
	ttl := c.CacheFor
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	c.mu.Lock()
	if r, ok := c.cache[cond]; ok && now.Sub(r.at) < ttl && !now.Before(r.at) {
		c.mu.Unlock()
		return r.ok, r.why
	}
	c.mu.Unlock()
	ok, why := c.check(kind, arg)
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	c.cache[cond] = cached{ok, why, now}
	c.mu.Unlock()
	return ok, why
}

func (c *PlatformChecker) check(kind, arg string) (bool, string) {
	switch kind {
	case "ac-power":
		f := c.OnAC
		if f == nil {
			f = platform.OnACPower
		}
		if on, known := f(); known && !on {
			return false, "on battery"
		}
		return true, ""
	case "unmetered":
		f := c.Metered
		if f == nil {
			f = platform.NetworkMetered
		}
		if m, known := f(); known && m {
			return false, "on a metered connection"
		}
		return true, ""
	case "volume":
		if c.Volumes != nil {
			if _, _, err := c.Volumes.Resolve(arg, ""); err != nil {
				return false, err.Error()
			}
			return true, ""
		}
		vs, err := platform.ListVolumes()
		if err != nil {
			return false, err.Error()
		}
		for _, v := range vs {
			if v.Label == arg {
				return true, ""
			}
		}
		return false, "drive " + arg + " isn't mounted"
	}
	return false, "unknown precondition " + kind
}

// unmet returns why the first precondition that doesn't hold fails, or "" when all hold.
func (s *Scheduler) unmet(ctx context.Context, j Job, now time.Time) string {
	if len(j.Preconditions) == 0 {
		return ""
	}
	c := s.Check
	if c == nil {
		s.mu.Lock()
		if s.defaultCheck == nil {
			s.defaultCheck = &PlatformChecker{}
		}
		c = s.defaultCheck
		s.mu.Unlock()
	}
	for _, p := range j.Preconditions {
		if ok, why := c.Check(ctx, p, now); !ok {
			if why == "" {
				why = p
			}
			return p + ": " + why
		}
	}
	return ""
}
