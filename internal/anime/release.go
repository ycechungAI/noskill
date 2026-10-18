// Package anime is the anime-tracker skill's engine (skills/anime-tracker/SKILL.md): release-name
// parsing, qBittorrent and AniList clients, rule building, naming, filing and the per-title state.
// The steps are plain Go so a small model only has to call them and report (AGENTS.md: design for
// 3B–8B models); decisions are used where the spec asks for them and fall back to rules without.
package anime

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Release is what a torrent or file name says about itself.
type Release struct {
	Name       string  `json:"name"`
	Group      string  `json:"group,omitempty"`
	Title      string  `json:"title"`
	Season     int     `json:"season,omitempty"`  // from S2, Season 2, 2nd Season, S02E05; 0 = not said
	Episode    float64 `json:"episode,omitempty"` // 0 = none; 12.5 for recap episodes
	EpisodeEnd float64 `json:"episode_end,omitempty"`
	Version    int     `json:"version,omitempty"` // v2 = 2; 0 = first release
	Resolution string  `json:"resolution,omitempty"`
	Kind       string  `json:"kind"` // episode | batch | special | movie | other
	Year       int     `json:"year,omitempty"`
	Codec      string  `json:"codec,omitempty"` // "HEVC", "AV1", ...
	TenBit     bool    `json:"ten_bit,omitempty"`
	Ext        string  `json:"ext,omitempty"`
}

var (
	videoExt  = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|m4v|webm|ts)$`)
	bracketRe = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|【[^】]*】`)
	resRe     = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])(?:(\d{3,4})x(\d{3,4})|(\d{3,4})[pi])(?:$|[^0-9a-z])`)
	sdRe      = regexp.MustCompile(`(?i)(?:^|[^a-z])(SD|DVD(?:rip)?)(?:$|[^a-z])`)
	hashRe    = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)
	yearRe    = regexp.MustCompile(`^(19[5-9]\d|20[0-4]\d)$`)
	sxeRe     = regexp.MustCompile(`(?i)\bS(\d{1,2})\s?E(\d{1,4}(?:\.5)?)(?:\s?-\s?E?(\d{1,4}))?(?:v(\d))?\b`)
	dashEpRe  = regexp.MustCompile(`(?i)\s[-–—]\s#?(\d{1,4}(?:\.5)?)(?:\s?[-~]\s?(\d{1,4}))?(?:v(\d))?(?:\s?(?:END|FINAL))?(?:\s|$)`)
	specialRe = regexp.MustCompile(`(?i)(?:\s[-–—]\s|\s)(OVA|OAD|ONA|SP|Special|Specials|Recap)\s?(\d{1,3})?(?:v(\d))?(?:\s|$)`)
	epWordRe  = regexp.MustCompile(`(?i)\b(?:Episode|Ep\.?|E)\s?(\d{1,4}(?:\.5)?)(?:v(\d))?\b`)
	trailEpRe = regexp.MustCompile(`\s(\d{1,4})(?:v(\d))?$`)
	rangeRe   = regexp.MustCompile(`^\s*(\d{1,4})\s?[-~]\s?(\d{1,4})\s*$`)
	seasonRe  = regexp.MustCompile(`(?i)\s(?:S(\d{1,2})|Season\s?(\d{1,2})|(\d{1,2})(?:st|nd|rd|th)\s+Season)$`)
	movieRe   = regexp.MustCompile(`(?i)\b(Movie|Gekijouban|the Movie|Film)\b`)
	batchRe   = regexp.MustCompile(`(?i)\b(Batch|Complete|Season Pack)\b`)
)

// ParseRelease reads a release or file name, anitopy-style: [Group] Title - 03v2 (480p) [ABCD1234].mkv,
// Title.S02E05.1080p.WEB.H264-GROUP.mkv, [Group] Title (01-12) [BD 1080p], and so on.
func ParseRelease(name string) Release {
	r := Release{Name: name, Kind: "other"}
	s := strings.TrimSpace(name)
	if m := videoExt.FindStringSubmatch(s); m != nil {
		r.Ext = strings.ToLower(m[1])
		s = s[:len(s)-len(m[0])]
	}
	// Scene style: dots or underscores for spaces.
	if !strings.Contains(s, " ") && strings.Count(s, ".")+strings.Count(s, "_") >= 2 {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	} else if !strings.Contains(s, " ") && strings.Contains(s, "_") {
		s = strings.ReplaceAll(s, "_", " ")
	}
	// Tags in brackets: the first one at the start is the group.
	var tags []string
	if loc := bracketRe.FindStringIndex(s); loc != nil && loc[0] == 0 && strings.HasPrefix(s, "[") {
		r.Group = strings.TrimSpace(s[1 : loc[1]-1])
		s = s[loc[1]:]
	}
	for _, t := range bracketRe.FindAllString(s, -1) {
		tags = append(tags, strings.TrimSpace(t[len(firstRune(t)):len(t)-len(lastRune(t))]))
	}
	main := strings.TrimSpace(bracketRe.ReplaceAllString(s, " "))
	main = strings.Join(strings.Fields(main), " ")

	all := name
	r.Resolution = resolution(all)
	up := strings.ToUpper(all)
	switch {
	case strings.Contains(up, "HEVC") || strings.Contains(up, "X265") || strings.Contains(up, "H265") || strings.Contains(up, "H.265"):
		r.Codec = "HEVC"
	case strings.Contains(up, "AV1"):
		r.Codec = "AV1"
	case strings.Contains(up, "X264") || strings.Contains(up, "H264") || strings.Contains(up, "H.264") || strings.Contains(up, "AVC"):
		r.Codec = "AVC"
	}
	r.TenBit = strings.Contains(up, "10BIT") || strings.Contains(up, "10-BIT") || strings.Contains(up, "HI10")
	for _, t := range tags {
		if yearRe.MatchString(t) {
			r.Year, _ = strconv.Atoi(t)
		}
		if m := rangeRe.FindStringSubmatch(t); m != nil && r.Episode == 0 {
			r.Episode, _ = strconv.ParseFloat(m[1], 64)
			r.EpisodeEnd, _ = strconv.ParseFloat(m[2], 64)
		}
	}
	// Scene names end with source and group: cut at the first quality word.
	if i := sceneCut(main); i > 0 {
		main = strings.TrimSpace(main[:i])
	}

	title := main
	switch m := sxeRe.FindStringSubmatchIndex(main); {
	case m != nil:
		r.Season, _ = strconv.Atoi(main[m[2]:m[3]])
		if r.Season == 0 {
			r.Kind = "special" // S00E02: season 0 is specials (Plex, TheTVDB, and noskill's own names)
		}
		r.Episode, _ = strconv.ParseFloat(main[m[4]:m[5]], 64)
		if m[6] >= 0 {
			r.EpisodeEnd, _ = strconv.ParseFloat(main[m[6]:m[7]], 64)
		}
		if m[8] >= 0 {
			r.Version, _ = strconv.Atoi(main[m[8]:m[9]])
		}
		title = main[:m[0]]
	default:
		if loc := lastMatch(specialRe, main); loc != nil {
			r.Kind = "special"
			if loc[4] >= 0 {
				r.Episode, _ = strconv.ParseFloat(main[loc[4]:loc[5]], 64)
			}
			if loc[6] >= 0 {
				r.Version, _ = strconv.Atoi(main[loc[6]:loc[7]])
			}
			title = main[:loc[0]]
			break
		}
		if loc := lastMatch(dashEpRe, main); loc != nil {
			r.Episode, _ = strconv.ParseFloat(main[loc[2]:loc[3]], 64)
			if loc[4] >= 0 {
				r.EpisodeEnd, _ = strconv.ParseFloat(main[loc[4]:loc[5]], 64)
			}
			if loc[6] >= 0 {
				r.Version, _ = strconv.Atoi(main[loc[6]:loc[7]])
			}
			title = main[:loc[0]]
			break
		}
		if loc := epWordRe.FindStringSubmatchIndex(main); loc != nil && loc[0] > 0 {
			r.Episode, _ = strconv.ParseFloat(main[loc[2]:loc[3]], 64)
			if loc[4] >= 0 {
				r.Version, _ = strconv.Atoi(main[loc[4]:loc[5]])
			}
			title = main[:loc[0]]
			break
		}
		if loc := trailEpRe.FindStringSubmatchIndex(main); loc != nil && loc[0] > 0 && r.Episode == 0 && !yearRe.MatchString(main[loc[2]:loc[3]]) {
			r.Episode, _ = strconv.ParseFloat(main[loc[2]:loc[3]], 64)
			if loc[4] >= 0 {
				r.Version, _ = strconv.Atoi(main[loc[4]:loc[5]])
			}
			title = main[:loc[0]]
		}
	}
	title = strings.TrimSpace(strings.Trim(strings.TrimSpace(title), "-–—_:"))
	if m := seasonRe.FindStringSubmatch(" " + title); m != nil && r.Season == 0 {
		for _, g := range m[1:] {
			if g != "" {
				r.Season, _ = strconv.Atoi(g)
			}
		}
	}
	r.Title = title

	switch {
	case r.Kind == "special":
	case r.EpisodeEnd > r.Episode && r.Episode > 0, batchRe.MatchString(name):
		r.Kind = "batch"
	case r.Episode > 0:
		r.Kind = "episode"
	case movieRe.MatchString(name) || r.Year > 0:
		r.Kind = "movie"
	}
	return r
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

func lastRune(s string) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return ""
	}
	return string(rs[len(rs)-1])
}

// lastMatch returns the submatch indexes of re's last match in s.
func lastMatch(re *regexp.Regexp, s string) []int {
	all := re.FindAllStringSubmatchIndex(s, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// sceneCut finds where a scene name's title and episode end: the first source/quality word.
func sceneCut(s string) int {
	words := strings.Fields(s)
	pos := 0
	for i, w := range words {
		lw := strings.ToLower(w)
		if i > 0 && (resRe.MatchString(" "+lw+" ") || lw == "web" || lw == "web-dl" || lw == "webrip" || lw == "bluray" || lw == "bdrip" || lw == "hdtv" || strings.HasPrefix(lw, "h264") || strings.HasPrefix(lw, "x264") || strings.HasPrefix(lw, "x265") || lw == "multi") {
			return pos
		}
		pos += len(w) + 1
	}
	return 0
}

// resolution normalises "848x480", "480p" and SD/DVD to "480p" (and so on for 720p, 1080p, 2160p).
func resolution(name string) string {
	best := ""
	for _, m := range resRe.FindAllStringSubmatch(name, -1) {
		h := 0
		if m[2] != "" {
			h, _ = strconv.Atoi(m[2])
		} else {
			h, _ = strconv.Atoi(m[3])
		}
		var v string
		switch {
		case h >= 360 && h <= 576:
			v = "480p"
		case h >= 700 && h <= 800:
			v = "720p"
		case h >= 1000 && h <= 1200:
			v = "1080p"
		case h >= 2000 && h <= 2200:
			v = "2160p"
		}
		if v != "" {
			best = v
		}
	}
	if best == "" && sdRe.MatchString(name) {
		best = "480p"
	}
	return best
}

// ResolutionPattern is the regex a rule's mustContain uses for one resolution (SKILL.md "Resolution
// patterns"), case-insensitive.
func ResolutionPattern(res string) string {
	switch res {
	case "480p":
		return `480p|848x480|854x480|640x480`
	case "720p":
		return `720p|1280x720`
	case "1080p":
		return `1080p|1920x1080`
	case "2160p":
		return `2160p|3840x2160|4K`
	}
	return regexp.QuoteMeta(res)
}

// Normalize makes a title comparable: lower case, letters and digits only, single spaces, and
// "Season 2" / "2nd Season" / "S2" all written "s2".
func Normalize(t string) string {
	t = strings.ToLower(t)
	var b strings.Builder
	for _, r := range t {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’':
		default:
			b.WriteRune(' ')
		}
	}
	f := strings.Fields(b.String())
	var out []string
	for i := 0; i < len(f); i++ {
		w := f[i]
		if w == "season" && i+1 < len(f) && isNum(f[i+1]) {
			out = append(out, "s"+strings.TrimLeft(f[i+1], "0"))
			i++
			continue
		}
		if i+1 < len(f) && f[i+1] == "season" && ordinal(w) != "" {
			out = append(out, "s"+ordinal(w))
			i++
			continue
		}
		if len(w) > 1 && w[0] == 's' && isNum(w[1:]) {
			out = append(out, "s"+strings.TrimLeft(w[1:], "0"))
			continue
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func ordinal(w string) string {
	for _, suf := range []string{"st", "nd", "rd", "th"} {
		if n, ok := strings.CutSuffix(w, suf); ok && isNum(n) {
			return n
		}
	}
	return ""
}
