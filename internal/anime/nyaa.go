package anime

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NyaaURL is Nyaa's address. Where it's blocked, the user can point nyaa_url at a mirror (for
// example https://nyaa.media, whose feed has no links: Resolve reads the magnet from the item's page).
const NyaaURL = "https://nyaa.si"

// NyaaEnglishSubs is Nyaa's "Anime - English-translated" category: fansubs and official subs in English.
const NyaaEnglishSubs = "1_2"

// NyaaItem is one torrent in a Nyaa RSS feed.
type NyaaItem struct {
	Title      string    `json:"title"`
	TorrentURL string    `json:"torrent_url"` // the .torrent file; noskill hands this URL to the torrent app
	Page       string    `json:"page"`
	InfoHash   string    `json:"info_hash"`
	Category   string    `json:"category"`
	Size       string    `json:"size"`
	Published  time.Time `json:"published"`
	Seeders    int       `json:"seeders"`
	Trusted    bool      `json:"trusted"`
	Remake     bool      `json:"remake"`
}

// Nyaa reads Nyaa's RSS search. It never downloads torrents or video itself: the torrent app does.
type Nyaa struct {
	Base        string // default NyaaURL
	Category    string // default NyaaEnglishSubs
	TrustedOnly bool   // Nyaa's "trusted uploaders" filter
	HTTP        *http.Client
}

// FeedURL is the RSS search for query, also usable as a feed in the user's torrent app.
func (n *Nyaa) FeedURL(query string) (string, error) {
	base := n.Base
	if base == "" {
		base = NyaaURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("nyaa_url %q isn't a URL", base)
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return "", fmt.Errorf("nyaa_url must use https: %s", base)
	}
	cat := n.Category
	if cat == "" {
		cat = NyaaEnglishSubs
	}
	f := "0"
	if n.TrustedOnly {
		f = "2"
	}
	q := url.Values{"page": {"rss"}, "c": {cat}, "f": {f}}
	if query != "" {
		q.Set("q", query)
	}
	u.Path, u.RawQuery = "/", q.Encode()
	return u.String(), nil
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Search returns the newest English-subtitled torrents matching query ("" = everything new).
func (n *Nyaa) Search(ctx context.Context, query string) ([]NyaaItem, error) {
	u, err := n.FeedURL(query)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "noskill anime-tracker")
	c := n.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach Nyaa: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Nyaa: HTTP %d", resp.StatusCode)
	}
	items, err := ParseNyaaRSS(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	// Only keep links on the feed's own site: a feed can't send the torrent app elsewhere. Items
	// without a link (mirrors such as nyaa.media) are resolved from their page later (Resolve).
	host := req.URL.Hostname()
	keep := items[:0]
	for _, it := range items {
		switch {
		case it.TorrentURL == "" && sameHost(it.Page, host):
			keep = append(keep, it)
		case sameHost(it.TorrentURL, host):
			keep = append(keep, it)
		}
	}
	return keep, nil
}

func sameHost(u, host string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || isLoopback(p.Hostname())) && strings.EqualFold(p.Hostname(), host)
}

var (
	magnetRe = regexp.MustCompile(`magnet:\?xt=urn:btih:([0-9a-fA-F]{40}|[A-Z2-7a-z]{32})[^"'<>\s]*`)
	viewIDRe = regexp.MustCompile(`/view/(\d+)$`)
)

// Resolve fills in an item's link when its feed has none: the magnet link on its page. A magnet
// names the torrent by its info hash, so the page can't swap the contents later. Only pages on the
// feed's own site are read.
func (n *Nyaa) Resolve(ctx context.Context, it *NyaaItem) error {
	if it.TorrentURL != "" {
		return nil
	}
	base, err := n.FeedURL("")
	if err != nil {
		return err
	}
	bu, _ := url.Parse(base)
	if !sameHost(it.Page, bu.Hostname()) || !viewIDRe.MatchString(it.Page) {
		return fmt.Errorf("Nyaa: no link for %q", it.Title)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, it.Page, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "noskill anime-tracker")
	c := n.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Nyaa: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Nyaa %s: HTTP %d", it.Page, resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	m := magnetRe.FindStringSubmatch(html.UnescapeString(string(b)))
	if m == nil {
		return fmt.Errorf("Nyaa: no magnet link on %s", it.Page)
	}
	it.TorrentURL = m[0]
	if len(m[1]) == 40 {
		it.InfoHash = strings.ToLower(m[1])
	}
	return nil
}

// ParseNyaaRSS reads Nyaa's RSS, including its nyaa: fields.
func ParseNyaaRSS(r io.Reader) ([]NyaaItem, error) {
	var doc struct {
		Items []struct {
			Title    string `xml:"title"`
			Link     string `xml:"link"`
			GUID     string `xml:"guid"`
			PubDate  string `xml:"pubDate"`
			Seeders  string `xml:"seeders"`
			InfoHash string `xml:"infoHash"`
			Category string `xml:"categoryId"`
			Size     string `xml:"size"`
			Trusted  string `xml:"trusted"`
			Remake   string `xml:"remake"`
		} `xml:"channel>item"`
	}
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("Nyaa: unexpected feed: %w", err)
	}
	out := make([]NyaaItem, 0, len(doc.Items))
	for _, it := range doc.Items {
		t, _ := time.Parse("Mon, 02 Jan 2006 15:04:05 -0700", strings.TrimSpace(it.PubDate))
		seed, _ := strconv.Atoi(strings.TrimSpace(it.Seeders))
		out = append(out, NyaaItem{Title: strings.TrimSpace(it.Title), TorrentURL: strings.TrimSpace(it.Link),
			Page: strings.TrimSpace(it.GUID), InfoHash: strings.ToLower(strings.TrimSpace(it.InfoHash)),
			Category: it.Category, Size: it.Size, Published: t, Seeders: seed,
			Trusted: strings.EqualFold(it.Trusted, "yes"), Remake: strings.EqualFold(it.Remake, "yes")})
	}
	return out, nil
}
