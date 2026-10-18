package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// RulePrefix marks the RSS rules noskill owns. It never creates, changes or removes any other rule
// (SKILL.md "Rules").
const RulePrefix = "noskill: "

// ErrNotOurs means a call would touch a rule or torrent that isn't noskill's.
var ErrNotOurs = errors.New("not noskill's")

// Qbit talks to qBittorrent's Web API v2. Logging in is only needed when the user hasn't turned on
// "Bypass authentication for clients on localhost"; the credentials come from the OS keychain.
// Endpoint and field names follow the v2 docs; the S6 spike checks them against the user's version.
type Qbit struct {
	Base     string // http://localhost:8080
	User     string
	Pass     string
	Category string // only torrents in this category are ever added, listed for filing or removed
	SavePath string // download folder for the category
	HTTP     *http.Client

	once   sync.Once
	jar    http.CookieJar
	logged bool
}

func (q *Qbit) client() *http.Client {
	q.once.Do(func() { q.jar, _ = cookiejar.New(nil) })
	c := http.Client{Timeout: 30 * time.Second, Jar: q.jar}
	if q.HTTP != nil {
		c.Transport, c.CheckRedirect = q.HTTP.Transport, q.HTTP.CheckRedirect
		if q.HTTP.Timeout > 0 {
			c.Timeout = q.HTTP.Timeout
		}
	}
	return &c
}

// QbitError is a failed call, with what to do about it.
type QbitError struct {
	Status int
	Path   string
	Body   string
}

func (e *QbitError) Error() string {
	switch e.Status {
	case http.StatusForbidden:
		return "qBittorrent refused the login: turn on Web UI › \"Bypass authentication for clients on localhost\", or store QBITTORRENT_USER and QBITTORRENT_PASS with `noskill secrets set`"
	case http.StatusConflict:
		return "qBittorrent: " + strings.TrimSpace(e.Body)
	}
	return fmt.Sprintf("qBittorrent %s: HTTP %d %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

func (q *Qbit) do(ctx context.Context, method, path string, form url.Values, out any) error {
	err := q.do1(ctx, method, path, form, out)
	var qe *QbitError
	if errors.As(err, &qe) && qe.Status == http.StatusForbidden && !q.logged && q.User != "" {
		if lerr := q.login(ctx); lerr != nil {
			return lerr
		}
		return q.do1(ctx, method, path, form, out)
	}
	return err
}

func (q *Qbit) do1(ctx context.Context, method, path string, form url.Values, out any) error {
	u := strings.TrimRight(q.Base, "/") + "/api/v2/" + path
	var body io.Reader
	if method == http.MethodGet && form != nil {
		u += "?" + form.Encode()
	} else if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", strings.TrimRight(q.Base, "/")) // the Web UI's CSRF check wants a matching origin
	return q.send(req, path, out)
}

func (q *Qbit) send(req *http.Request, path string, out any) error {
	resp, err := q.client().Do(req)
	if err != nil {
		return fmt.Errorf("can't reach qBittorrent at %s (is it running with the Web UI on?): %w", q.Base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return &QbitError{Status: resp.StatusCode, Path: path, Body: string(b)}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(b)
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("qBittorrent %s: unexpected reply: %w", path, err)
	}
	return nil
}

func (q *Qbit) login(ctx context.Context) error {
	var s string
	err := q.do1(ctx, http.MethodPost, "auth/login", url.Values{"username": {q.User}, "password": {q.Pass}}, &s)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s) != "Ok." {
		return errors.New("qBittorrent didn't accept QBITTORRENT_USER / QBITTORRENT_PASS")
	}
	q.logged = true
	return nil
}

// Version is the Web API version, e.g. "2.11.2".
func (q *Qbit) Version(ctx context.Context) (string, error) {
	var s string
	err := q.do(ctx, http.MethodGet, "app/webapiVersion", nil, &s)
	return strings.TrimSpace(s), err
}

// Feed is one RSS feed the user added to qBittorrent.
type Feed struct {
	Name     string    `json:"name"` // path in qBittorrent's feed tree, folders joined with `\`
	URL      string    `json:"url"`
	Articles []Article `json:"articles,omitempty"`
}

// Article is one feed item.
type Article struct {
	Feed       string    `json:"feed"`
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	TorrentURL string    `json:"torrentURL"`
	Link       string    `json:"link,omitempty"`
	Date       time.Time `json:"date,omitzero"`
}

// Feeds lists the user's feeds, with their recent articles when withData.
func (q *Qbit) Feeds(ctx context.Context, withData bool) ([]Feed, error) {
	var tree map[string]json.RawMessage
	if err := q.do(ctx, http.MethodGet, "rss/items", url.Values{"withData": {fmt.Sprint(withData)}}, &tree); err != nil {
		return nil, err
	}
	var out []Feed
	walkFeeds("", tree, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func walkFeeds(prefix string, tree map[string]json.RawMessage, out *[]Feed) {
	for name, raw := range tree {
		var probe struct {
			URL      string `json:"url"`
			Articles []struct {
				ID         json.RawMessage `json:"id"`
				Title      string          `json:"title"`
				TorrentURL string          `json:"torrentURL"`
				Link       string          `json:"link"`
				Date       string          `json:"date"`
			} `json:"articles"`
		}
		path := name
		if prefix != "" {
			path = prefix + `\` + name
		}
		if json.Unmarshal(raw, &probe) == nil && probe.URL != "" {
			f := Feed{Name: path, URL: probe.URL}
			for _, a := range probe.Articles {
				art := Article{Feed: path, ID: strings.Trim(string(a.ID), `"`), Title: a.Title, TorrentURL: a.TorrentURL, Link: a.Link}
				if art.TorrentURL == "" { // some feeds only fill link
					art.TorrentURL = a.Link
				}
				for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "02 Jan 2006 15:04:05 -0700"} {
					if t, err := time.Parse(layout, a.Date); err == nil {
						art.Date = t
						break
					}
				}
				f.Articles = append(f.Articles, art)
			}
			*out = append(*out, f)
			continue
		}
		var sub map[string]json.RawMessage
		if json.Unmarshal(raw, &sub) == nil {
			walkFeeds(path, sub, out)
		}
	}
}

// Rule is an RSS download rule (qBittorrent's ruleDef). Both the old (assignedCategory, savePath)
// and the 4.6+ (torrentParams) ways of setting category and folder are sent.
type Rule struct {
	Enabled                   bool           `json:"enabled"`
	MustContain               string         `json:"mustContain"`
	MustNotContain            string         `json:"mustNotContain"`
	UseRegex                  bool           `json:"useRegex"`
	EpisodeFilter             string         `json:"episodeFilter"`
	SmartFilter               bool           `json:"smartFilter"`
	PreviouslyMatchedEpisodes []string       `json:"previouslyMatchedEpisodes"`
	AffectedFeeds             []string       `json:"affectedFeeds"`
	IgnoreDays                int            `json:"ignoreDays"`
	AddPaused                 *bool          `json:"addPaused"`
	AssignedCategory          string         `json:"assignedCategory"`
	SavePath                  string         `json:"savePath"`
	TorrentParams             map[string]any `json:"torrentParams,omitempty"`
}

func checkRuleName(name string) error {
	if !strings.HasPrefix(name, RulePrefix) || len(name) == len(RulePrefix) {
		return fmt.Errorf("rule %q: %w (noskill only manages rules named %q…)", name, ErrNotOurs, RulePrefix)
	}
	return nil
}

// SetRule creates or replaces one of noskill's rules. Category and folder are always noskill's.
func (q *Qbit) SetRule(ctx context.Context, name string, r Rule) error {
	if err := checkRuleName(name); err != nil {
		return err
	}
	r.AssignedCategory, r.SavePath = q.Category, q.SavePath
	r.TorrentParams = map[string]any{"category": q.Category, "save_path": q.SavePath}
	if r.PreviouslyMatchedEpisodes == nil {
		r.PreviouslyMatchedEpisodes = []string{}
	}
	if r.AffectedFeeds == nil {
		r.AffectedFeeds = []string{}
	}
	def, _ := json.Marshal(r)
	return q.do(ctx, http.MethodPost, "rss/setRule", url.Values{"ruleName": {name}, "ruleDef": {string(def)}}, nil)
}

// Rules returns every rule, the user's included (read only).
func (q *Qbit) Rules(ctx context.Context) (map[string]Rule, error) {
	out := map[string]Rule{}
	err := q.do(ctx, http.MethodGet, "rss/rules", nil, &out)
	return out, err
}

// RemoveRule removes one of noskill's rules.
func (q *Qbit) RemoveRule(ctx context.Context, name string) error {
	if err := checkRuleName(name); err != nil {
		return err
	}
	return q.do(ctx, http.MethodPost, "rss/removeRule", url.Values{"ruleName": {name}}, nil)
}

// MatchingArticles lists what a rule matches now: feed name → article titles.
func (q *Qbit) MatchingArticles(ctx context.Context, name string) (map[string][]string, error) {
	out := map[string][]string{}
	err := q.do(ctx, http.MethodGet, "rss/matchingArticles", url.Values{"ruleName": {name}}, &out)
	return out, err
}

// TestRule sets r under a temporary name, reads what it matches and removes it again.
func (q *Qbit) TestRule(ctx context.Context, r Rule) ([]string, error) {
	const name = RulePrefix + "__test__"
	r.Enabled = false // a disabled rule still reports matches but never downloads
	if err := q.SetRule(ctx, name, r); err != nil {
		return nil, err
	}
	defer q.RemoveRule(context.WithoutCancel(ctx), name)
	m, err := q.MatchingArticles(ctx, name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, titles := range m {
		out = append(out, titles...)
	}
	sort.Strings(out)
	return out, nil
}

// Torrent is one torrent in qBittorrent.
type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Category    string  `json:"category"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Progress    float64 `json:"progress"`
	Size        int64   `json:"size"`
	CompletedOn int64   `json:"completion_on"` // unix seconds
	AddedOn     int64   `json:"added_on"`
	Ratio       float64 `json:"ratio"`
	MaxRatio    float64 `json:"max_ratio"`
	MaxSeedTime int64   `json:"max_seeding_time"` // minutes; < 0 = no limit
}

// Stopped says whether the torrent is already paused or stopped (done, so not seeding any more).
func (t Torrent) Stopped() bool { return t.State == "stoppedUP" || t.State == "pausedUP" }

// Seeding reports whether qBittorrent is still seeding toward a share limit the user set: filing
// waits until the limit is reached (SKILL.md D.1). Without limits it doesn't wait.
func (t Torrent) Seeding() bool {
	switch t.State {
	case "stoppedUP", "pausedUP":
		return false
	}
	return t.MaxRatio > 0 || t.MaxSeedTime > 0
}

// Completed lists finished torrents in noskill's category, oldest first.
func (q *Qbit) Completed(ctx context.Context) ([]Torrent, error) {
	var out []Torrent
	if err := q.do(ctx, http.MethodGet, "torrents/info", url.Values{"category": {q.Category}, "filter": {"completed"}}, &out); err != nil {
		return nil, err
	}
	keep := out[:0]
	for _, t := range out {
		if t.Category == q.Category && t.Progress >= 1 { // never trust the filter alone
			keep = append(keep, t)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].CompletedOn < keep[j].CompletedOn })
	return keep, nil
}

// Torrent returns one torrent by hash.
func (q *Qbit) Torrent(ctx context.Context, hash string) (Torrent, error) {
	var out []Torrent
	if err := q.do(ctx, http.MethodGet, "torrents/info", url.Values{"hashes": {hash}}, &out); err != nil {
		return Torrent{}, err
	}
	for _, t := range out {
		if strings.EqualFold(t.Hash, hash) {
			return t, nil
		}
	}
	return Torrent{}, fmt.Errorf("qBittorrent has no torrent %s", hash)
}

// TorrentFile is one file inside a torrent; Name is relative to the torrent's save path.
type TorrentFile struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}

func (q *Qbit) Files(ctx context.Context, hash string) ([]TorrentFile, error) {
	var out []TorrentFile
	err := q.do(ctx, http.MethodGet, "torrents/files", url.Values{"hash": {hash}}, &out)
	return out, err
}

// AddTorrent adds a torrent URL in noskill's category and download folder.
func (q *Qbit) AddTorrent(ctx context.Context, torrentURL string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("urls", torrentURL)
	w.WriteField("category", q.Category)
	if q.SavePath != "" {
		w.WriteField("savepath", q.SavePath)
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(q.Base, "/")+"/api/v2/torrents/add", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Referer", strings.TrimRight(q.Base, "/"))
	var s string
	if err := q.send(req, "torrents/add", &s); err != nil {
		var qe *QbitError
		if errors.As(err, &qe) && qe.Status == http.StatusForbidden && !q.logged && q.User != "" {
			if err := q.login(ctx); err != nil {
				return err
			}
			return q.AddTorrent(ctx, torrentURL)
		}
		return err
	}
	if strings.TrimSpace(s) == "Fails." {
		return errors.New("qBittorrent couldn't add the torrent (already added, or the link is dead)")
	}
	return nil
}

// RemoveTorrent removes a torrent from qBittorrent, keeping its files: noskill trashes them itself
// after filing. Only torrents in noskill's category can be removed.
// Stop pauses one of noskill's torrents so it stops seeding: "stop" on qBittorrent 5, "pause" on 4.
func (q *Qbit) Stop(ctx context.Context, hash string) error {
	t, err := q.Torrent(ctx, hash)
	if err != nil {
		return err
	}
	if t.Category != q.Category || q.Category == "" {
		return fmt.Errorf("torrent %q is in category %q: %w", t.Name, t.Category, ErrNotOurs)
	}
	err = q.do(ctx, http.MethodPost, "torrents/stop", url.Values{"hashes": {t.Hash}}, nil)
	var qe *QbitError
	if errors.As(err, &qe) && qe.Status == http.StatusNotFound {
		err = q.do(ctx, http.MethodPost, "torrents/pause", url.Values{"hashes": {t.Hash}}, nil)
	}
	return err
}

func (q *Qbit) RemoveTorrent(ctx context.Context, hash string) error {
	t, err := q.Torrent(ctx, hash)
	if err != nil {
		return err
	}
	if t.Category != q.Category || q.Category == "" {
		return fmt.Errorf("torrent %q is in category %q: %w", t.Name, t.Category, ErrNotOurs)
	}
	return q.do(ctx, http.MethodPost, "torrents/delete", url.Values{"hashes": {t.Hash}, "deleteFiles": {"false"}}, nil)
}

// EnsureCategory creates noskill's category with its download folder (409 = it exists).
func (q *Qbit) EnsureCategory(ctx context.Context) error {
	err := q.do(ctx, http.MethodPost, "torrents/createCategory", url.Values{"category": {q.Category}, "savePath": {q.SavePath}}, nil)
	var qe *QbitError
	if errors.As(err, &qe) && qe.Status == http.StatusConflict {
		return nil
	}
	return err
}

// Name is the client's name for reports.
func (q *Qbit) Name() string { return "qBittorrent" }

// Ensure creates noskill's category (Downloader).
func (q *Qbit) Ensure(ctx context.Context) error { return q.EnsureCategory(ctx) }

// All lists every torrent, in any category: only read, to avoid adding what the user already has.
func (q *Qbit) All(ctx context.Context) ([]Torrent, error) {
	var out []Torrent
	err := q.do(ctx, http.MethodGet, "torrents/info", nil, &out)
	return out, err
}
