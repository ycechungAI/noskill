package deepresearch

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// ---- words ----

var stop = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for are but not you all any can had her was one our out has have been from that this with they will would there their what about which when your said each she how its who into than them these some could other then also more most such only over very just where while being those after before between because through during under again further once here both does did doing his him himself herself itself ours yours theirs than too off`) {
		m[w] = true
	}
	return m
}()

// tokens are the lower-case words of s that carry meaning: no stop words, no one- or two-letter words.
func tokens(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 3 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

func tokenSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range tokens(s) {
		m[w] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for k := range a {
		if b[k] {
			common++
		}
	}
	return float64(common) / float64(len(a)+len(b)-common)
}

// overlap is how many of a's words b has, as a share of a.
func overlap(a, b map[string]bool) float64 {
	if len(a) == 0 {
		return 0
	}
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// norm lower-cases and collapses whitespace and the quote and dash variants that pages and models
// disagree on, so a quote is compared by its words and not its typography.
func norm(s string) string {
	r := strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`, "–", "-", "—", "-", " ", " ", "…", "...")
	return strings.Join(strings.Fields(strings.ToLower(r.Replace(s))), " ")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if strings.HasPrefix(raw, "file:") {
			return "local"
		}
		return strings.ToLower(raw)
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// ---- passages ----

// passages cuts a page into pieces of about size characters, on paragraph and sentence ends where it
// can, each starting a little before the last ended so a fact on a boundary isn't lost.
func passages(text string, size int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	r := []rune(text)
	if len(r) <= size {
		return []string{text}
	}
	var out []string
	for start := 0; start < len(r); {
		end := min(start+size, len(r))
		if end < len(r) { // back up to a sentence or paragraph end in the last quarter
			for i := end; i > start+size*3/4; i-- {
				if r[i-1] == '\n' || r[i-1] == '.' || r[i-1] == '!' || r[i-1] == '?' {
					end = i
					break
				}
			}
		}
		out = append(out, strings.TrimSpace(string(r[start:end])))
		if end == len(r) {
			break
		}
		start = max(end-size/8, start+1)
	}
	return out
}

// bestPassages ranks a page's passages by how many words of the question and the topic they hold
// (the question's words count double) and returns the top n in page order.
func bestPassages(ps []string, question, topic string, n int) []string {
	q, t := tokenSet(question), tokenSet(topic)
	type sc struct {
		i int
		s float64
	}
	var all []sc
	for i, p := range ps {
		pt := tokenSet(p)
		s := 0.0
		for w := range pt {
			switch {
			case q[w]:
				s += 2
			case t[w]:
				s++
			}
		}
		all = append(all, sc{i, s / (1 + float64(len(pt))/40)})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].s > all[b].s })
	if len(all) > n {
		all = all[:n]
	}
	sort.Slice(all, func(a, b int) bool { return all[a].i < all[b].i })
	out := make([]string, 0, len(all))
	for _, x := range all {
		if x.s > 0 {
			out = append(out, ps[x.i])
		}
	}
	if len(out) == 0 && len(ps) > 0 {
		out = append(out, ps[0])
	}
	return out
}

// ---- links ----

var mdLink = regexp.MustCompile(`\[([^\]]{2,120})\]\((https?://[^)\s]+)\)`)

// LinksIn reads the markdown links in text.
func LinksIn(text string) []Link {
	var out []Link
	seen := map[string]bool{}
	for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
		u := strings.TrimRight(m[2], ".,;")
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, Link{Text: m[1], URL: u})
	}
	return out
}

// ---- sources ----

// lowValue are sites whose pages answer questions badly or only repeat other pages.
var lowValue = []string{"pinterest.", "quora.com", "facebook.com", "instagram.com", "tiktok.com", "twitter.com", "x.com", "linkedin.com",
	"reddit.com", "youtube.com", "youtu.be", "amazon.", "ebay.", "slideshare.", "scribd.", "medium.com/tag", "wikipedia.org", "britannica.com", "fandom.com", "wikiwand.com", "baike.baidu.com"}

// sourceScore rates a result by where it is from: primary and institutional sites first, content farms last.
func sourceScore(u string) float64 {
	h := hostOf(u)
	s := 0.0
	switch {
	case strings.HasSuffix(h, ".gov") || strings.Contains(h, ".gov."):
		s += 3
	case strings.HasSuffix(h, ".edu") || strings.Contains(h, ".ac."):
		s += 3
	case strings.HasSuffix(h, ".int") || strings.HasSuffix(h, ".org"):
		s += 1
	}
	for _, k := range []string{"arxiv.org", "nature.com", "science.org", "nih.gov", "who.int", "ieee.org", "acm.org", "springer.com", "sciencedirect.com", "pubmed", "doi.org", "github.com"} {
		if strings.Contains(h, k) {
			s += 2
		}
	}
	for _, k := range lowValue {
		if strings.Contains(strings.ToLower(u), k) {
			s -= 4
		}
	}
	if strings.HasSuffix(strings.ToLower(u), ".pdf") {
		s += 0.5
	}
	return s
}

// ---- claims ----

var number = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?%?`)

// numbersIn lists the numbers in s as plain strings ("1,200" becomes "1200").
func numbersIn(s string) []string {
	var out []string
	for _, m := range number.FindAllString(s, -1) {
		m = strings.ReplaceAll(m, ",", "")
		m = strings.TrimRight(m, ".")
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// verifyClaim says whether a proposed claim is backed by its quote in the passage text: the quote
// must be in the text (by its words, not its typography), long enough to say something, and the
// claim must share its main words with the quote, so a real quote can't stand behind an invented fact.
// why is empty when it holds.
func verifyClaim(claim, quote, text string) (ok bool, why string) {
	claim, quote = strings.TrimSpace(claim), strings.TrimSpace(quote)
	nq := norm(quote)
	switch {
	case claim == "" || nq == "":
		return false, "no claim or no quote"
	case len([]rune(nq)) < 20:
		return false, "quote too short to back a fact"
	case !strings.Contains(norm(text), strings.TrimSuffix(strings.TrimPrefix(nq, "..."), "...")):
		return false, "the quote isn't in the page"
	}
	ct := tokenSet(claim)
	if len(ct) > 0 && overlap(ct, tokenSet(quote)) < 0.4 {
		return false, "the quote doesn't say what the claim says"
	}
	// A number in the claim must be in the quote.
	qn := map[string]bool{}
	for _, n := range numbersIn(quote) {
		qn[n] = true
	}
	for _, n := range numbersIn(claim) {
		if !qn[n] {
			return false, "the claim has a number the quote doesn't"
		}
	}
	return true, ""
}
