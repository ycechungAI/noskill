// Package watch is the watcher skill's logic (skills/watcher, FR-J1): what a watch is, and how a
// fetched page, feed, release list or search result becomes a list of items, so that only items new
// since the last check are reported. It does no I/O; the tools fetch, the store remembers.
package watch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type Kind string

const (
	Page   Kind = "page"
	RSS    Kind = "rss"
	GitHub Kind = "github_releases"
	Search Kind = "search"
)

var Kinds = []Kind{Page, RSS, GitHub, Search}

// DefaultSchedule is a daily check at 08:00 (the skill's schedule_default).
const DefaultSchedule = "cron:0 8 * * *"

// Watch is one thing being watched. Each watch is a scheduler job (JobID).
type Watch struct {
	Name         string    `json:"name"`
	Kind         Kind      `json:"kind"`
	Target       string    `json:"target"` // URL, owner/repo, or the search query
	Topic        string    `json:"topic,omitempty"`
	Schedule     string    `json:"schedule"` // cron:<expr> | every:<duration>
	JobID        string    `json:"job_id,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	LastChecked  time.Time `json:"last_checked,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}

// Item is one thing found by a check. Fingerprint identifies it across checks.
type Item struct {
	Fingerprint string `json:"fingerprint"`
	Title       string `json:"title"`
	URL         string `json:"url,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,59}$`)
var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// Validate checks a watch before it is stored and fills in what is missing.
func (w *Watch) Validate() error {
	w.Name = strings.TrimSpace(w.Name)
	if !nameRe.MatchString(w.Name) {
		return errors.New("name: use a short name of letters, digits, spaces, dots, dashes or underscores")
	}
	w.Target = strings.TrimSpace(w.Target)
	switch w.Kind {
	case Page, RSS:
		u, err := url.Parse(w.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("target: %s needs a full http:// or https:// address", w.Kind)
		}
	case GitHub:
		w.Target = strings.TrimPrefix(strings.TrimPrefix(w.Target, "https://github.com/"), "github.com/")
		w.Target = strings.TrimSuffix(strings.Trim(w.Target, "/"), ".git")
		if !repoRe.MatchString(w.Target) {
			return errors.New("target: github_releases needs owner/repo, e.g. ollama/ollama")
		}
	case Search:
		if w.Target == "" {
			return errors.New("target: a search watch needs the search query")
		}
	default:
		return fmt.Errorf("kind %q: want page, rss, github_releases or search", w.Kind)
	}
	if w.Schedule == "" {
		w.Schedule = DefaultSchedule
	}
	return nil
}

// Host is the network host a check connects to, for the permission check.
func (w Watch) Host() string {
	switch w.Kind {
	case GitHub:
		return "api.github.com"
	case Page, RSS:
		if u, err := url.Parse(w.Target); err == nil {
			return u.Hostname()
		}
	}
	return ""
}

// Fingerprint hashes already-normalised text.
func Fingerprint(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

// ---- pages ----

var (
	digitsRe = regexp.MustCompile(`\d+`)
	spaceRe  = regexp.MustCompile(`\s+`)
	// Lines that are page furniture rather than content: cookie notices, share buttons, ads.
	boilerRe = regexp.MustCompile(`(?i)\b(cookie|cookies|subscribe|newsletter|advertisement|sponsored|sign in|log in|all rights reserved|privacy policy|terms of (use|service)|share (on|this)|follow us|skip to (main )?content)\b|^©|^\s*(advertisement|ad)\s*$`)
	// Markdown decoration and link targets don't make text new.
	mdRe = regexp.MustCompile(`[#*_>` + "`" + `|]+|\]\([^)]*\)|\[|\]`)
)

// norm lowers the text, drops Markdown marks and links, and masks digits, so a changed date,
// counter, price or view count doesn't make a paragraph look new.
func norm(s string) string {
	s = mdRe.ReplaceAllString(s, " ")
	s = digitsRe.ReplaceAllString(strings.ToLower(s), "#")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// PageItems splits a page's readable text (Markdown) into paragraphs and returns the content ones.
// Very short paragraphs and page furniture are dropped. The fingerprint is of the normalised text.
func PageItems(text, pageURL string) []Item {
	var out []Item
	seen := map[string]bool{}
	for _, p := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		p = strings.TrimSpace(p)
		n := norm(p)
		if letters(n) < 20 || boilerRe.MatchString(p) {
			continue
		}
		fp := Fingerprint("page", n)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, Item{Fingerprint: fp, Title: clip(strings.TrimSpace(mdRe.ReplaceAllString(firstLine(p), " ")), 120), URL: pageURL, Summary: clip(spaceRe.ReplaceAllString(p, " "), 400)})
	}
	return out
}

func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return spaceRe.ReplaceAllString(l, " ")
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- feeds ----

type rssDoc struct {
	Items []struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
		GUID  string `xml:"guid"`
		Desc  string `xml:"description"`
	} `xml:"channel>item"`
}

type atomDoc struct {
	Entries []struct {
		Title string `xml:"title"`
		ID    string `xml:"id"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Summary string `xml:"summary"`
	} `xml:"entry"`
}

// ParseFeed reads RSS 2.0 or Atom. An item's identity is its guid/id, else its link, else its title.
func ParseFeed(body []byte) ([]Item, error) {
	var r rssDoc
	if err := xml.Unmarshal(body, &r); err == nil && len(r.Items) > 0 {
		var out []Item
		for _, it := range r.Items {
			id := firstNonEmpty(it.GUID, it.Link, it.Title)
			if id == "" {
				continue
			}
			out = append(out, Item{Fingerprint: Fingerprint("feed", strings.TrimSpace(id)), Title: strings.TrimSpace(it.Title),
				URL: strings.TrimSpace(it.Link), Summary: clip(plain(it.Desc), 300)})
		}
		return out, nil
	}
	var a atomDoc
	if err := xml.Unmarshal(body, &a); err == nil && len(a.Entries) > 0 {
		var out []Item
		for _, e := range a.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			id := firstNonEmpty(e.ID, link, e.Title)
			out = append(out, Item{Fingerprint: Fingerprint("feed", strings.TrimSpace(id)), Title: strings.TrimSpace(e.Title), URL: link, Summary: clip(plain(e.Summary), 300)})
		}
		return out, nil
	}
	return nil, errors.New("not an RSS or Atom feed")
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func plain(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(s, " "), " "))
}

func firstNonEmpty(l ...string) string {
	for _, s := range l {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// ---- GitHub releases ----

// ParseReleases reads GitHub's releases API. Drafts are skipped; a pre-release says so in its title.
func ParseReleases(body []byte) ([]Item, error) {
	var rs []struct {
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		URL        string `json:"html_url"`
		Body       string `json:"body"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, errors.New("not a list of GitHub releases")
	}
	var out []Item
	for _, r := range rs {
		if r.Draft || r.Tag == "" {
			continue
		}
		title := firstNonEmpty(r.Name, r.Tag)
		if r.Name != "" && r.Name != r.Tag && !strings.Contains(r.Name, r.Tag) {
			title = r.Tag + ": " + r.Name
		}
		if r.Prerelease {
			title += " (pre-release)"
		}
		out = append(out, Item{Fingerprint: Fingerprint("release", r.Tag), Title: title, URL: r.URL, Summary: clip(plain(r.Body), 300)})
	}
	return out, nil
}

// SearchItem makes an item from a search result; it is new when its URL hasn't been seen for this
// watch (the host and path, without tracking parameters).
func SearchItem(title, link, snippet string) (Item, bool) {
	if link == "" {
		return Item{}, false
	}
	return Item{Fingerprint: Fingerprint("search", canonicalURL(link)), Title: title, URL: link, Summary: clip(snippet, 300)}, true
}

func canonicalURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	return strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")) + strings.TrimRight(u.Path, "/")
}

// New returns the items whose fingerprints aren't in seen, once each, in order.
func New(items []Item, seen map[string]bool) []Item {
	var out []Item
	done := map[string]bool{}
	for _, it := range items {
		if it.Fingerprint == "" || seen[it.Fingerprint] || done[it.Fingerprint] {
			continue
		}
		done[it.Fingerprint] = true
		out = append(out, it)
	}
	return out
}
