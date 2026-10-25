package chores

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reminder is a reminder read from a sentence: the text to show and when. Exactly one of Cron
// (a repeating time of day, day of week or month), Every (a fixed interval such as "2h") or a one-off
// At is set. Once reminders are scheduled at their date and switch themselves off after they fire.
type Reminder struct {
	Message string    `json:"message"`
	Cron    string    `json:"cron,omitempty"`
	Every   string    `json:"every,omitempty"`
	At      time.Time `json:"at,omitzero"`
	Once    bool      `json:"once"`
	When    string    `json:"when"` // what was understood, in words, for the user to check
}

var (
	numWords = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
		"nine": 9, "ten": 10, "eleven": 11, "twelve": 12}
	weekdays = map[string]int{"sunday": 0, "sun": 0, "monday": 1, "mon": 1, "tuesday": 2, "tue": 2, "tues": 2, "wednesday": 3, "wed": 3,
		"thursday": 4, "thu": 4, "thur": 4, "thurs": 4, "friday": 5, "fri": 5, "saturday": 6, "sat": 6}
	dayNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

	wdPat = `(sun(?:day)?|mon(?:day)?|tue(?:s|sday)?|wed(?:nesday)?|thu(?:r|rs|rsday)?|fri(?:day)?|sat(?:urday)?)s?`
	wordN = `one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve`

	todMeridiem = regexp.MustCompile(`(?i)\b(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*([ap])\.?m\b\.?`)
	tod24       = regexp.MustCompile(`(?i)\b(?:at\s+)?(\d{1,2}):(\d{2})\b`)
	todBare     = regexp.MustCompile(`(?i)\bat\s+(\d{1,2})\b`)
	todNoon     = regexp.MustCompile(`(?i)\b(?:at\s+)?(noon|midnight)\b`)
	todWord     = regexp.MustCompile(`(?i)\b(?:at\s+(` + wordN + `)(?:\s+o'?clock)?|(` + wordN + `)\s+o'?clock)(?:\s+(?:in the\s+)?([ap])\.?m\b\.?)?`)

	everyN     = regexp.MustCompile(`(?i)\bevery\s+(\d+|` + wordN + `)\s+(minute|min|hour|hr)s?\b`)
	everyDay   = regexp.MustCompile(`(?i)\b(?:every\s+(?:day|morning|evening|night)|daily|each\s+day)\b`)
	everyWkday = regexp.MustCompile(`(?i)\b(?:every\s+week\s?day|on\s+week\s?days|week\s?days)\b`)
	everyWkend = regexp.MustCompile(`(?i)\b(?:every\s+weekend|on\s+weekends)\b`)
	everyDow   = regexp.MustCompile(`(?i)\b(?:every|each)\s+` + wdPat + `\b`)
	onDows     = regexp.MustCompile(`(?i)\bon\s+` + wdPat + `s\b`)
	everyMonth = regexp.MustCompile(`(?i)\b(?:every\s+month\s+on\s+the\s+(\d{1,2})(?:st|nd|rd|th)?|monthly\s+on\s+the\s+(\d{1,2})(?:st|nd|rd|th)?)\b`)
	inN        = regexp.MustCompile(`(?i)\bin\s+(\d+|an?|` + wordN + `)\s+(minute|min|hour|hr|day)s?\b`)
	dayWord    = regexp.MustCompile(`(?i)\b(today|tonight|tomorrow)\b`)
	nextDow    = regexp.MustCompile(`(?i)\b(?:on\s+|next\s+)?` + wdPat + `\b`)
	lead       = regexp.MustCompile(`(?i)^[\s,.;:-]*(?:please\s+)?(?:remind\s+me|set\s+(?:a\s+)?reminder)?[\s,]*(?:to|that|about|of|:)?\s*`)
)

// ParseReminder reads "remind me to submit the report every Monday at nine" and similar: repeating
// ("every day at 8:30", "every weekday", "every 2 hours", "every month on the 1st") or once
// ("tomorrow at 5pm", "in 20 minutes", "on Friday at 3pm"). It never guesses silently: When says
// what it understood. A time with no am/pm is read as 7–11 morning, 12 noon, 1–6 afternoon.
func ParseReminder(text string, now time.Time) (Reminder, error) {
	s := strings.TrimSpace(text)
	if s == "" {
		return Reminder{}, errors.New("say what to remind you of and when, for example: remind me to call Sam tomorrow at 5pm")
	}
	var r Reminder
	cut := func(loc []int) { s = s[:loc[0]] + " " + s[loc[1]:] }
	hasTime := func() (int, int, bool) { // reads and removes a time of day
		h, m, loc, ok := timeOfDay(s)
		if ok {
			cut(loc)
		}
		return h, m, ok
	}
	needDefault := func(h, m int, ok bool) (int, int) {
		if !ok {
			return 9, 0
		}
		return h, m
	}
	hm := func(h, m int) string { return fmt.Sprintf("%02d:%02d", h, m) }
	local := now.Location()

	switch {
	case everyN.MatchString(s):
		loc := everyN.FindStringSubmatchIndex(s)
		g := everyN.FindStringSubmatch(s)
		n := atoiWord(g[1])
		unit := "m"
		if strings.HasPrefix(strings.ToLower(g[2]), "h") {
			unit = "h"
		}
		if n <= 0 || (unit == "m" && n < 5) {
			return Reminder{}, errors.New("every N minutes needs N of at least 5")
		}
		cut(loc)
		r.Every, r.When = strconv.Itoa(n)+unit, fmt.Sprintf("every %d %s", n, map[string]string{"m": "minutes", "h": "hours"}[unit])
	case everyMonth.MatchString(s):
		loc := everyMonth.FindStringSubmatchIndex(s)
		g := everyMonth.FindStringSubmatch(s)
		d := atoiWord(g[1] + g[2])
		if d < 1 || d > 28 {
			return Reminder{}, errors.New("monthly reminders work for days 1 to 28")
		}
		cut(loc)
		h, m := needDefault(hasTime())
		r.Cron, r.When = fmt.Sprintf("%d %d %d * *", m, h, d), fmt.Sprintf("every month on day %d at %s", d, hm(h, m))
	case everyWkday.MatchString(s) || everyWkend.MatchString(s):
		re, dow, word := everyWkday, "1-5", "weekday"
		if everyWkend.MatchString(s) {
			re, dow, word = everyWkend, "6,0", "weekend day"
		}
		cut(re.FindStringIndex(s))
		h, m := needDefault(hasTime())
		r.Cron, r.When = fmt.Sprintf("%d %d * * %s", m, h, dow), fmt.Sprintf("every %s at %s", word, hm(h, m))
	case everyDow.MatchString(s) || onDows.MatchString(s):
		re := everyDow
		if !everyDow.MatchString(s) {
			re = onDows
		}
		g := re.FindStringSubmatch(s)
		cut(re.FindStringIndex(s))
		d := weekdays[strings.ToLower(g[1])]
		h, m := needDefault(hasTime())
		r.Cron, r.When = fmt.Sprintf("%d %d * * %d", m, h, d), fmt.Sprintf("every %s at %s", dayNames[d], hm(h, m))
	case everyDay.MatchString(s):
		cut(everyDay.FindStringIndex(s))
		h, m := needDefault(hasTime())
		r.Cron, r.When = fmt.Sprintf("%d %d * * *", m, h), "every day at "+hm(h, m)
	case inN.MatchString(s):
		g := inN.FindStringSubmatch(s)
		cut(inN.FindStringIndex(s))
		n := atoiWord(g[1])
		if strings.HasPrefix(strings.ToLower(g[1]), "a") {
			n = 1
		}
		d := time.Duration(n) * time.Minute
		switch strings.ToLower(g[2])[0] {
		case 'h':
			d = time.Duration(n) * time.Hour
		case 'd':
			d = time.Duration(n) * 24 * time.Hour
		}
		if n <= 0 {
			return Reminder{}, errors.New("in how long? For example: in 20 minutes")
		}
		r.At, r.Once = now.Add(d).Truncate(time.Minute), true
	case dayWord.MatchString(s):
		w := strings.ToLower(dayWord.FindString(s))
		cut(dayWord.FindStringIndex(s))
		h, m, ok := hasTime()
		switch w {
		case "tomorrow":
			h, m = needDefault(h, m, ok)
			r.At = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, local)
		default:
			if !ok {
				if w == "tonight" {
					h, m = 20, 0
				} else {
					return Reminder{}, errors.New("today at what time? For example: today at 5pm")
				}
			}
			r.At = time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, local)
			if !r.At.After(now) {
				return Reminder{}, fmt.Errorf("%s is already past today", hm(h, m))
			}
		}
		r.Once = true
	case nextDow.MatchString(s):
		g := nextDow.FindStringSubmatch(s)
		cut(nextDow.FindStringIndex(s))
		d := weekdays[strings.ToLower(g[1])]
		h, m := needDefault(hasTime())
		at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, local)
		for i := 0; i < 8 && (int(at.Weekday()) != d || !at.After(now)); i++ {
			at = at.AddDate(0, 0, 1)
		}
		r.At, r.Once = at, true
	default:
		h, m, ok := hasTime()
		if !ok {
			return Reminder{}, errors.New("I couldn't find when. Say, for example: every Monday at 9, tomorrow at 5pm, in 20 minutes, or every 2 hours")
		}
		at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, local)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		r.At, r.Once = at, true
	}
	if r.Once {
		r.Cron = fmt.Sprintf("%d %d %d %d *", r.At.Minute(), r.At.Hour(), r.At.Day(), int(r.At.Month()))
		r.When = "once, " + r.At.Format("Mon 2 Jan at 15:04")
	}
	msg := strings.Join(strings.Fields(s), " ")
	msg = strings.TrimSpace(lead.ReplaceAllString(msg, ""))
	msg = strings.Trim(msg, " ,.;:-")
	if msg == "" {
		return Reminder{}, errors.New("what should the reminder say? For example: remind me to call Sam tomorrow at 5pm")
	}
	r.Message = strings.ToUpper(msg[:1]) + msg[1:]
	return r, nil
}

func atoiWord(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, ok := numWords[s]; ok {
		return n
	}
	n, _ := strconv.Atoi(s)
	return n
}

// timeOfDay finds a time in s: "5pm", "17:30", "at 9", "at nine", "noon". It returns the hour and
// minute (24-hour) and where it was in s.
func timeOfDay(s string) (h, m int, loc []int, ok bool) {
	if g := todMeridiem.FindStringSubmatch(s); g != nil {
		h, _ = strconv.Atoi(g[1])
		m, _ = strconv.Atoi(g[2])
		return to24(h, m, strings.ToLower(g[3])), m, todMeridiem.FindStringSubmatchIndex(s)[:2], valid(h, m, true)
	}
	if g := tod24.FindStringSubmatch(s); g != nil {
		h, _ = strconv.Atoi(g[1])
		m, _ = strconv.Atoi(g[2])
		if g[1][0] != '0' { // "5:30" is half past five in the afternoon; "05:30" is the 24-hour reading
			h = to24(h, m, "")
		}
		return h, m, tod24.FindStringIndex(s), valid(h, m, false)
	}
	if g := todNoon.FindStringSubmatch(s); g != nil {
		if strings.EqualFold(g[1], "noon") {
			return 12, 0, todNoon.FindStringIndex(s), true
		}
		return 0, 0, todNoon.FindStringIndex(s), true
	}
	if g := todWord.FindStringSubmatch(s); g != nil {
		w := g[1]
		if w == "" {
			w = g[2]
		}
		h = numWords[strings.ToLower(w)]
		return to24(h, 0, strings.ToLower(g[3])), 0, todWord.FindStringIndex(s), true
	}
	if g := todBare.FindStringSubmatch(s); g != nil {
		h, _ = strconv.Atoi(g[1])
		return to24(h, 0, ""), 0, todBare.FindStringIndex(s), valid(h, 0, true)
	}
	return 0, 0, nil, false
}

func valid(h, m int, twelve bool) bool {
	if twelve {
		return h >= 0 && h <= 24 && m >= 0 && m < 60
	}
	return h >= 0 && h < 24 && m >= 0 && m < 60
}

// to24 turns a 12-hour clock reading into 24 hours; with no am/pm, 7–11 is morning, 12 noon, 1–6 afternoon.
func to24(h, m int, mer string) int {
	switch mer {
	case "a":
		if h == 12 {
			return 0
		}
		return h
	case "p":
		if h < 12 {
			return h + 12
		}
		return h
	}
	if h >= 1 && h <= 6 {
		return h + 12
	}
	return h
}
