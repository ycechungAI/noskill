package anime

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The user's release preferences (SKILL.md "Rules"): English subtitles only, 720p and above only when
// the file is small, and a preferred release group first.

// DefaultHDMaxMB is the biggest file a 720p-or-higher release may be (quality.hd_max_size_mb).
const DefaultHDMaxMB = 500

var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9][0-9.,]*)\s*([kmgt]?)i?b?\s*$`)

// ParseSize reads Nyaa's size text ("385.3 MiB", "1.2 GiB") as bytes; -1 when it can't.
func ParseSize(s string) int64 {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return -1
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil {
		return -1
	}
	mult := map[string]float64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}[strings.ToLower(m[2])]
	return int64(n * mult)
}

// IsHD says whether a resolution tag is 720p or higher ("720p", "1080p", "2160p", "4K").
func IsHD(res string) bool {
	r := strings.ToLower(strings.TrimSpace(res))
	if r == "4k" || r == "8k" {
		return true
	}
	h, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(r, "p"), "i"))
	return err == nil && h >= 700
}

// tooBig says whether a release is 720p or higher and bigger than maxMB (0 = no limit). A size that
// can't be read counts as too big: the rule is "only if it is small".
func tooBig(res, size string, maxMB int) bool {
	if maxMB <= 0 || !IsHD(res) {
		return false
	}
	b := ParseSize(size)
	return b < 0 || b > int64(maxMB)<<20
}

var (
	otherSubs   = regexp.MustCompile(`(?i)(?:^|[\s\[\(._-])(chs|cht|big5|chinese|portuguese|pt-br|spanish|latino|german|french|italian|russian|vietnamese|indonesian|thai|arabic|korean|hardsub (?:pt|es|fr|de|ru|it|ar|id|vi|th))(?:$|[\s\]\)._-])`)
	englishSubs = regexp.MustCompile(`(?i)(?:^|[\s\[\(._+-])(eng|english|multi(?:ple)?[ -]?sub(?:title)?s?|multi-?subs?)(?:$|[\s\]\)._+-])`)
)

// EnglishSubs says whether a release name doesn't announce subtitles in another language only. Nyaa's
// English-translated category is already English; this drops the odd upload there that is tagged for
// another language, unless it also says English or multiple subtitles ("[ENG+CHS]", Erai-raws).
func EnglishSubs(name string) bool {
	return !otherSubs.MatchString(name) || englishSubs.MatchString(name)
}

// groupRank orders release groups: the index in prefer (case-insensitive), or len(prefer) for any other.
func groupRank(group string, prefer []string) int {
	for i, g := range prefer {
		if strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(group)) {
			return i
		}
	}
	return len(prefer)
}

// seasonEnd is the last moment of the broadcast season now falls in: Winter ends 31 March, Spring 30 June,
// Summer 30 September, Fall 31 December (AniList's seasons, in now's own time zone).
func seasonEnd(now time.Time) time.Time {
	endMonth := (int(now.Month())-1)/3*3 + 3 // 3, 6, 9 or 12
	first := time.Date(now.Year(), time.Month(endMonth), 1, 0, 0, 0, 0, now.Location())
	return first.AddDate(0, 1, 0).Add(-time.Second)
}

// untilSeasonEnd is how long an approval card for a show offered now stays open: to the end of the
// season, and at least a day.
func untilSeasonEnd(now time.Time) time.Duration {
	return max(seasonEnd(now).Sub(now), 24*time.Hour)
}
