package deepresearch

import (
	"sort"
	"strings"
)

// Corroborate counts, for every claim, the distinct sites that make it or one close to it, and sets
// Claim.Support. Two claims are close when they share enough words; the same site repeating itself
// counts once.
func Corroborate(claims []Claim, sources []Source) {
	host := map[int]string{}
	for _, s := range sources {
		host[s.ID] = s.Host
	}
	set := make([]map[string]bool, len(claims))
	for i, c := range claims {
		set[i] = tokenSet(c.Text)
	}
	for i := range claims {
		hosts := map[string]bool{host[claims[i].Source]: true}
		for j := range claims {
			if i != j && host[claims[j].Source] != host[claims[i].Source] && jaccard(set[i], set[j]) >= 0.4 {
				hosts[host[claims[j].Source]] = true
			}
		}
		claims[i].Support = len(hosts)
	}
}

// Conflict is two claims from different sites that talk about the same thing and give different numbers.
type Conflict struct {
	A, B    Claim
	Numbers [2][]string
}

// Conflicts finds the places where sources disagree on a number: the claims are about the same thing
// (their words, leaving numbers out, are alike) yet their sets of numbers don't meet. It is plain
// code, so a conflict is something a reader can check against the two quotes.
func Conflicts(claims []Claim, sources []Source, max int) []Conflict {
	host := map[int]string{}
	for _, s := range sources {
		host[s.ID] = s.Host
	}
	words := make([]map[string]bool, len(claims))
	nums := make([][]string, len(claims))
	for i, c := range claims {
		words[i] = tokenSet(number.ReplaceAllString(c.Text, " "))
		nums[i] = withoutYears(numbersIn(c.Text))
	}
	var out []Conflict
	used := map[int]bool{}
	for i := range claims {
		for j := i + 1; j < len(claims); j++ {
			if len(nums[i]) == 0 || len(nums[j]) == 0 || host[claims[i].Source] == host[claims[j].Source] || used[i] || used[j] {
				continue
			}
			if jaccard(words[i], words[j]) < 0.5 || meet(nums[i], nums[j]) {
				continue
			}
			used[i], used[j] = true, true
			out = append(out, Conflict{A: claims[i], B: claims[j], Numbers: [2][]string{nums[i], nums[j]}})
			if len(out) == max {
				return out
			}
		}
	}
	return out
}

func meet(a, b []string) bool {
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, y := range b {
		if m[y] {
			return true
		}
	}
	return false
}

// unanswered are the questions the research asked and found nothing for.
func unanswered(qs []Question) []Question {
	var out []Question
	for _, q := range qs {
		if q.Claims == 0 && q.Status != "open" {
			out = append(out, q)
		}
	}
	return out
}

// similarQuestion says whether a question is (nearly) one already asked: small models plan the same
// question again and again, and a gap that repeats an earlier question is not a new gap.
func similarQuestion(text string, qs []Question, topic string) bool {
	w := tokenSet(text)
	for k := range tokenSet(topic) {
		delete(w, k)
	}
	if len(w) < 2 {
		return false
	}
	for _, q := range qs {
		o := tokenSet(q.Text)
		for k := range tokenSet(topic) {
			delete(o, k)
		}
		if jaccard(w, o) >= 0.6 {
			return true
		}
	}
	return false
}

// bestSection picks the section a claim fits best: the one whose title and scope share the most words
// with it, ties to the earlier. -1 when none shares a word.
func bestSection(c Claim, secs []Section, qtext string) int {
	cw := tokenSet(c.Text + " " + qtext)
	best, bestN := -1, 0.0
	for i, s := range secs {
		n := 0.0
		sw := tokenSet(s.Title + " " + s.Scope)
		for w := range cw {
			if sw[w] {
				n++
			}
		}
		if n > bestN {
			best, bestN = i, n
		}
	}
	return best
}

// Section is a part of the report.
type Section struct {
	Title string `json:"title"`
	Scope string `json:"scope"`
}

func sortClaims(cs []Claim) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Support != cs[j].Support {
			return cs[i].Support > cs[j].Support
		}
		return cs[i].ID < cs[j].ID
	})
}

func joinNums(n []string) string { return strings.Join(n, ", ") }

// withoutYears drops numbers that are years: two sources dating the same thing alike are not agreeing
// about a figure, and a year in both claims must not hide a disagreement over the figure.
func withoutYears(ns []string) []string {
	var out []string
	for _, n := range ns {
		if len(n) == 4 && (strings.HasPrefix(n, "19") || strings.HasPrefix(n, "20")) {
			continue
		}
		out = append(out, n)
	}
	return out
}
