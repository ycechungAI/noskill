package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule says when a job runs next (FR-J1): a cron expression or a fixed interval. Stored as
// "cron:<expr>" or "every:<duration>".
type Schedule interface {
	// Next is the first run time strictly after t, in t's location.
	Next(t time.Time) time.Time
	String() string
}

// ParseSchedule reads the stored form.
func ParseSchedule(s string) (Schedule, error) {
	kind, v, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("schedule %q: want cron:<expr> or every:<duration>", s)
	}
	switch kind {
	case "cron":
		return ParseCron(v)
	case "every":
		return ParseEvery(v)
	}
	return nil, fmt.Errorf("schedule %q: want cron:<expr> or every:<duration>", s)
}

// Every runs at a fixed interval from the previous scheduled time.
type Every struct{ D time.Duration }

// MinEvery keeps a typo like "--every 1s" from hammering the model.
const MinEvery = time.Minute

func ParseEvery(s string) (Every, error) {
	s = strings.TrimSpace(s)
	d, err := time.ParseDuration(s)
	if err != nil {
		// Allow days, which time.ParseDuration doesn't know: "1d", "2d12h".
		if days, rest, ok := strings.Cut(s, "d"); ok {
			n, err1 := strconv.Atoi(days)
			var r time.Duration
			var err2 error
			if rest != "" {
				r, err2 = time.ParseDuration(rest)
			}
			if err1 == nil && err2 == nil && n >= 0 {
				d, err = time.Duration(n)*24*time.Hour+r, nil
			}
		}
	}
	if err != nil {
		return Every{}, fmt.Errorf("every %q: want a duration like 30m, 6h or 1d", s)
	}
	if d < MinEvery {
		return Every{}, fmt.Errorf("every %s: the shortest interval is %s", s, MinEvery)
	}
	return Every{d}, nil
}

func (e Every) Next(t time.Time) time.Time { return t.Add(e.D) }
func (e Every) String() string             { return "every:" + FormatDuration(e.D) }

// FormatDuration writes 1d, 6h, 1h30m, 45m: the forms ParseEvery reads.
func FormatDuration(d time.Duration) string {
	var b strings.Builder
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= days * 24 * time.Hour
	}
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	if d > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", int(d/time.Second))
	}
	return b.String()
}

// Cron is a standard five-field expression: minute hour day-of-month month day-of-week, in local
// time. Fields take *, numbers, ranges (1-5), steps (*/15, 1-30/5), lists (1,15) and names (jan,
// mon). As in classic cron, when both day fields are restricted a day matches if either does.
type Cron struct {
	expr                       string
	min, hour, dom, month, dow uint64 // bit i set = value i allowed
	domStar, dowStar           bool
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func ParseCron(expr string) (*Cron, error) {
	expr = strings.TrimSpace(expr)
	src := expr
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday), e.g. \"0 3 * * *\" for 03:00 daily", src)
	}
	c := &Cron{expr: src}
	var err error
	if c.min, err = field(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", src, err)
	}
	if c.hour, err = field(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", src, err)
	}
	if c.dom, err = field(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q day of month: %w", src, err)
	}
	if c.month, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", src, err)
	}
	if c.dow, err = field(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q weekday: %w", src, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*" || f[2] == "?", f[4] == "*" || f[4] == "?"
	return c, nil
}

func field(s string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepS)
			}
			step = n
		}
		a, b := lo, hi
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var err error
			if a, err = value(x, names); err != nil {
				return 0, err
			}
			if b, err = value(y, names); err != nil {
				return 0, err
			}
		default:
			v, err := value(rng, names)
			if err != nil {
				return 0, err
			}
			a, b = v, v
			if hasStep {
				b = hi // "5/15" = from 5 to the end, every 15
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	if bits == 0 {
		return 0, errors.New("matches nothing")
	}
	return bits, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a number", s)
	}
	return n, nil
}

func (c *Cron) String() string { return "cron:" + c.expr }

func (c *Cron) dayMatches(t time.Time) bool {
	d := c.dom&(1<<uint(t.Day())) != 0
	w := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return w
	case c.dowStar:
		return d
	}
	return d || w
}

// Next skips whole months, days and hours that can't match, so even "29 feb" is found quickly.
// Wall-clock times that a DST change skips are passed over; repeated ones run once.
func (c *Cron) Next(t time.Time) time.Time {
	loc := t.Location()
	wall := func(x time.Time) time.Time {
		return time.Date(x.Year(), x.Month(), x.Day(), x.Hour(), x.Minute(), 0, 0, time.UTC)
	}
	from := wall(t)
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = ahead(t, time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc))
			continue
		}
		if !c.dayMatches(t) {
			t = ahead(t, time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc))
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			t = ahead(t, time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc))
			continue
		}
		if c.min&(1<<uint(t.Minute())) == 0 || !wall(t).After(from) {
			t = t.Add(time.Minute) // the second pass through a repeated DST hour doesn't count
			continue
		}
		return t
	}
	return time.Time{} // never (e.g. 31 feb)
}

// ahead returns n, unless a DST gap made time.Date land back at or before t (Go may resolve a
// missing wall time with the later offset): then the next whole hour by the clock on the wall.
func ahead(t, n time.Time) time.Time {
	if n.After(t) {
		return n
	}
	return t.Add(time.Duration(60-t.Minute()) * time.Minute)
}
