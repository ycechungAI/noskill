package anime

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MatchThreshold is the confidence needed to treat a release as a title's (SKILL.md "Library tool":
// below 0.9, skip and report). False matches waste downloads and misfile episodes.
const MatchThreshold = 0.9

// splitSeason takes the season marker off a normalized title: "frieren s2" → ("frieren", 2).
func splitSeason(n string) (string, int) {
	f := strings.Fields(n)
	if len(f) > 1 {
		last := f[len(f)-1]
		if len(last) > 1 && last[0] == 's' && isNum(last[1:]) {
			s, _ := strconv.Atoi(last[1:])
			return strings.Join(f[:len(f)-1], " "), s
		}
	}
	return n, 0
}

// Confidence says how sure it is that a release title (with its parsed season) is one of these
// aliases of a show whose season is season (0 = first). 1 = same name; season mismatches are 0.
func Confidence(releaseTitle string, releaseSeason int, aliases []string, season int) float64 {
	rn, rs := splitSeason(Normalize(releaseTitle))
	if releaseSeason > 0 {
		rs = releaseSeason
	}
	best := 0.0
	for _, a := range aliases {
		an, as := splitSeason(Normalize(a))
		if as == 0 {
			as = season
		}
		if max(rs, 1) != max(as, 1) {
			continue // a sequel or the earlier season: never the same release
		}
		var c float64
		if rn == an {
			c = 1
		} else {
			c = tokenF1(rn, an)
		}
		best = math.Max(best, c)
	}
	return best
}

// tokenF1 is the F1 of the two titles' word sets; one-letter words count too ("Re Zero").
func tokenF1(a, b string) float64 {
	ta, tb := strings.Fields(a), strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := map[string]int{}
	for _, w := range tb {
		set[w]++
	}
	common := 0
	for _, w := range ta {
		if set[w] > 0 {
			set[w]--
			common++
		}
	}
	if common == 0 {
		return 0
	}
	p, r := float64(common)/float64(len(ta)), float64(common)/float64(len(tb))
	return 2 * p * r / (p + r)
}

// ---- RSS rules ----

// aliasPattern matches an alias with any separators: "Sousou no Frieren" also matches
// "Sousou.no.Frieren" and "Sousou_no_Frieren".
func aliasPattern(alias string) string {
	var words []string
	for _, w := range strings.Fields(Normalize(alias)) {
		words = append(words, regexp.QuoteMeta(w))
	}
	return strings.Join(words, `[\W_]+`)
}

// seasonPattern matches a season marker for season n ≥ 2.
func seasonPattern(n int) string {
	suf := "th"
	switch n {
	case 2:
		suf = "nd"
	case 3:
		suf = "rd"
	}
	return fmt.Sprintf(`(?:\bS0?%d(?:\b|E\d)|Season[\W_]?0?%d\b|%d%s[\W_]Season)`, n, n, n, suf)
}

// laterSeasons matches any season marker 2–9, which a first season's rule must not match.
const laterSeasons = `(?:\bS0?[2-9](?:\b|E\d)|Season[\W_]?0?[2-9]\b|\b[2-9](?:nd|rd|th)[\W_]Season)`

// BuildRule writes the RSS rule for a title at one resolution (SKILL.md "Rule building"): its
// aliases AND the resolution must appear, and later seasons, batches, avoided terms and excluded
// episodes must not. qBittorrent matches with PCRE, so lookaheads work.
func BuildRule(t TitleState, res string, avoid []string, feeds []string) Rule {
	var alts []string
	seen := map[string]bool{}
	for _, a := range t.Aliases {
		an, _ := splitSeason(Normalize(a))
		p := aliasPattern(an)
		if p != "" && !seen[p] {
			seen[p] = true
			alts = append(alts, p)
		}
	}
	must := `(?i)(?=.*\b(?:` + strings.Join(alts, "|") + `)\b)(?=.*(?:` + ResolutionPattern(res) + `))`
	if t.Season >= 2 {
		must += `(?=.*` + seasonPattern(t.Season) + `)`
	}
	not := []string{`(?i)\bBatch\b`, `[(\[]\d{1,4}\s?[-~]\s?\d{1,4}[)\]]`, `\s-\s\d{1,4}\s?[-~]\s?\d{1,4}\b`}
	if t.Season < 2 {
		not = append(not, laterSeasons)
	}
	for _, a := range avoid {
		if a = strings.TrimSpace(a); a != "" {
			words := strings.Fields(a)
			for i := range words {
				words[i] = regexp.QuoteMeta(words[i])
			}
			not = append(not, `(?i)`+strings.Join(words, `[\W_]*`))
		}
	}
	for _, e := range t.Excluded {
		not = append(not, EpisodePattern(e))
	}
	return Rule{Enabled: true, MustContain: must, MustNotContain: strings.Join(not, "|"), UseRegex: true,
		SmartFilter: false, AffectedFeeds: feeds}
}

// EpisodePattern matches one episode number in the usual release styles (" - 03", "S01E03", "E03").
func EpisodePattern(ep float64) string {
	n := strconv.FormatFloat(ep, 'f', -1, 64)
	n = strings.ReplaceAll(n, ".", `\.`)
	return `(?:\s[-–]\s0*` + n + `(?:v\d)?(?:\s|$)|E0*` + n + `(?:v\d)?\b)`
}

// RuleName is the title's rule: "noskill: <Title>".
func RuleName(title string) string { return RulePrefix + strings.TrimSpace(title) }

// ---- naming ----

// EpisodeKey identifies an episode in a title's filed list: "3", "12.5", "SP1", "movie".
func EpisodeKey(r Release) string {
	switch r.Kind {
	case "special":
		return "SP" + strconv.FormatFloat(max(r.Episode, 1), 'f', -1, 64)
	case "movie":
		return "movie"
	}
	return strconv.FormatFloat(r.Episode, 'f', -1, 64)
}

// Naming is the target-name settings (SKILL.md "Naming").
type Naming struct {
	Preset string // simple (default) | plex-jellyfin
	// Episodes is how simple names number episodes: sxxexx (default, "Title S02E03 [480p]"),
	// ep ("Title EP 3 [480p]") or episode ("Title Episode 3 [480p]").
	Episodes     string
	TitleFolders bool
	Sanitize     func(name, fs string) string // per target filesystem (files.sanitize_name)
	FS           string                       // target filesystem, e.g. "exfat"
}

// PlanName returns the target path for a release of t inside folder dest (SKILL.md "Naming").
func (n Naming) PlanName(dest string, t TitleState, r Release) string {
	clean := func(s string) string {
		if n.Sanitize != nil {
			return n.Sanitize(s, n.FS)
		}
		return s
	}
	title := clean(t.Title)
	ext := r.Ext
	if ext == "" {
		ext = "mkv"
	}
	width := 2
	if t.Episodes >= 100 || r.Episode >= 100 {
		width = 3
	}
	if t.Episodes >= 1000 || r.Episode >= 1000 {
		width = 4
	}
	num := func(e float64) string {
		whole := int(e)
		s := fmt.Sprintf("%0*d", width, whole)
		if frac := e - float64(whole); frac > 0 {
			s += strings.TrimPrefix(strconv.FormatFloat(frac, 'f', -1, 64), "0")
		}
		return s
	}
	var name, sub string
	switch n.Preset {
	case "plex-jellyfin":
		show := title
		if t.Year > 0 {
			show = fmt.Sprintf("%s (%d)", title, t.Year)
		}
		season := max(t.Season, 1)
		sub = filepath.Join(show, fmt.Sprintf("Season %02d", season))
		switch r.Kind {
		case "special":
			sub = filepath.Join(show, "Specials")
			name = fmt.Sprintf("%s - S00E%s", title, num(max(r.Episode, 1)))
		case "movie":
			sub, name = show, show
		default:
			name = fmt.Sprintf("%s - S%02dE%s", title, season, num(r.Episode))
		}
		if r.Resolution != "" && r.Kind != "movie" {
			name += " [" + r.Resolution + "]"
		}
	default:
		// The user's choice (2026-10-16): the show's name, the episode, the resolution, nothing else.
		sub = title
		season := max(t.Season, 1)
		plain := func(e float64) string { return strconv.FormatFloat(e, 'f', -1, 64) }
		switch {
		case r.Kind == "movie":
			name = title
			if t.Year > 0 {
				name = fmt.Sprintf("%s (%d)", title, t.Year)
			}
		case n.Episodes == "ep" && r.Kind == "special":
			name = fmt.Sprintf("%s SP %s", title, plain(max(r.Episode, 1)))
		case n.Episodes == "ep":
			name = fmt.Sprintf("%s EP %s", title, plain(r.Episode))
		case n.Episodes == "episode" && r.Kind == "special":
			name = fmt.Sprintf("%s Special %s", title, plain(max(r.Episode, 1)))
		case n.Episodes == "episode":
			name = fmt.Sprintf("%s Episode %s", title, plain(r.Episode))
		case r.Kind == "special":
			name = fmt.Sprintf("%s S00E%s", title, num(max(r.Episode, 1)))
		default:
			name = fmt.Sprintf("%s S%02dE%s", title, season, num(r.Episode))
		}
		if r.Resolution != "" && r.Kind != "movie" {
			name += " [" + r.Resolution + "]"
		}
	}
	if !n.TitleFolders && n.Preset != "plex-jellyfin" {
		sub = ""
	}
	return filepath.Join(dest, sub, clean(name)+"."+ext)
}

// RuleMatches evaluates a rule the way qBittorrent does, for checks and dry runs without it. Go's
// regexp has no lookahead, so the top-level `(?=.*X)` groups BuildRule writes are checked one by one.
func RuleMatches(r Rule, title string) (bool, error) {
	if !r.UseRegex {
		return false, fmt.Errorf("only regex rules can be checked locally")
	}
	parts, flags, err := lookaheads(r.MustContain)
	if err != nil {
		return false, err
	}
	for _, p := range parts {
		re, err := regexp.Compile(flags + p)
		if err != nil {
			return false, err
		}
		if !re.MatchString(title) {
			return false, nil
		}
	}
	if r.MustNotContain != "" {
		for _, alt := range splitTop(r.MustNotContain) {
			re, err := regexp.Compile(alt)
			if err != nil {
				return false, err
			}
			if re.MatchString(title) {
				return false, nil
			}
		}
	}
	return true, nil
}

// lookaheads splits "(?i)(?=.*A)(?=.*B)" into "A", "B" and the leading flags.
func lookaheads(s string) ([]string, string, error) {
	flags := ""
	if strings.HasPrefix(s, "(?i)") {
		flags, s = "(?i)", s[4:]
	}
	if !strings.HasPrefix(s, "(?=") {
		return []string{s}, flags, nil
	}
	var out []string
	for len(s) > 0 {
		if !strings.HasPrefix(s, "(?=.*") {
			return nil, "", fmt.Errorf("can't check %q locally", s)
		}
		depth, end := 0, -1
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return nil, "", fmt.Errorf("unbalanced %q", s)
		}
		out = append(out, s[len("(?=.*"):end])
		s = s[end+1:]
	}
	return out, flags, nil
}

// splitTop splits a|b|c at top-level bars, keeping each part's own flags valid on its own.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
