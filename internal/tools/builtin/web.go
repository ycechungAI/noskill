package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"noskill/internal/sandbox"
	"noskill/internal/tools"
)

// Dialer connects page fetches. It only reaches public addresses and resolves names itself, so an
// allowed name can't point a fetch at Ollama, noskill or the LAN (SECURITY.md "Egress proxy").
// Tests replace it.
var Dialer = sandbox.PublicDialer{}

const userAgent = "noskill/0.1 (+https://github.com/noskill/noskill)"

func Web() []tools.Builtin {
	return []tools.Builtin{
		{
			ID:          "web.search",
			Description: "Search the web. Returns title, url and snippet for each result.",
			Schema: json.RawMessage(`{"type":"object","required":["query"],"properties":{
				"query":{"type":"string","minLength":1,"description":"Search keywords, not a sentence"},
				"max_results":{"type":"integer","default":5,"minimum":1,"maximum":10},
				"recency":{"type":"string","enum":["day","week","month","year"]}}}`),
			Run: runSearch,
		},
		{
			ID:          "web.fetch",
			Description: "Fetch a web page and return its text as Markdown.",
			Schema: json.RawMessage(`{"type":"object","required":["url"],"properties":{
				"url":{"type":"string","description":"http or https address"},
				"max_chars":{"type":"integer","default":6000,"minimum":200,"maximum":30000}}}`),
			Run: runFetch,
		},
	}
}

// ---- fetch ----

func fetchClient(env *tools.Env) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				p, _ := strconv.Atoi(port)
				return Dialer.Dial(ctx, host, p)
			},
			TLSHandshakeTimeout: 8 * time.Second, DisableKeepAlives: true,
		},
		// Every hop is checked against the skill's network permission, not just the first URL.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return env.Perms.CheckHost(req.URL.Hostname())
		},
	}
}

func runFetch(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	json.Unmarshal(raw, &a)
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("url must be a full http:// or https:// address")
	}
	if err := env.Perms.CheckHost(u.Hostname()); err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.1")
	resp, err := fetchClient(env).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("couldn't fetch %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s answered %d", u.Host, resp.StatusCode)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	body := io.LimitReader(resp.Body, 5<<20)
	var title, text string
	switch {
	case strings.Contains(ct, "html") || ct == "":
		ex, err := extractHTML(body, resp.Request.URL)
		if err != nil {
			return "", err
		}
		title, text = ex.Title, ex.Text
	case strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml"):
		b, _ := io.ReadAll(body)
		text = string(b)
	case strings.Contains(ct, "pdf"):
		return "", errors.New("this is a PDF; PDF text extraction isn't available yet")
	default:
		return "", fmt.Errorf("can't read content of type %s", ct)
	}
	text = strings.ToValidUTF8(text, "")
	max := a.MaxChars
	if max <= 0 {
		max = 6000
	}
	truncated := false
	if r := []rune(text); len(r) > max {
		text, truncated = string(r[:max]), true
	}
	out, _ := json.Marshal(map[string]any{"url": resp.Request.URL.String(), "title": title, "text_markdown": text, "truncated": truncated})
	return string(out), nil
}

// ---- search ----

type result struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
}

type searchReq struct {
	query, recency string
	n              int
}

type provider struct {
	name string
	// configured reports whether it can be tried, and the endpoint/key it'll use.
	run func(ctx context.Context, env *tools.Env, r searchReq) ([]result, bool, error)
}

var searchCache = struct {
	sync.Mutex
	m    map[string]cached
	down map[string]time.Time
}{m: map[string]cached{}, down: map[string]time.Time{}}

type cached struct {
	at  time.Time
	out string
}

const cacheTTL = 15 * time.Minute

var providers = []provider{
	{"searxng", searxng}, {"ollama", ollamaSearch}, {"brave", brave}, {"ddg", ddg},
}

func runSearch(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
		Recency    string `json:"recency"`
	}
	json.Unmarshal(raw, &a)
	r := searchReq{query: strings.TrimSpace(a.Query), recency: a.Recency, n: a.MaxResults}
	if r.n <= 0 {
		r.n = 5
	}
	order := providers
	if s, ok := env.Setting("providers"); ok && s != "" { // e.g. providers = "brave,ddg"
		order = nil
		for _, name := range strings.Split(s, ",") {
			for _, p := range providers {
				if p.name == strings.TrimSpace(name) {
					order = append(order, p)
				}
			}
		}
	}
	// One cache entry per query, whichever provider answered; a provider that just failed is
	// skipped for a minute so a down SearXNG doesn't slow every search.
	key := fmt.Sprintf("%s|%s|%d", strings.ToLower(r.query), r.recency, r.n)
	searchCache.Lock()
	if c, ok := searchCache.m[key]; ok && time.Since(c.at) < cacheTTL {
		searchCache.Unlock()
		return c.out, nil
	}
	searchCache.Unlock()
	var failures []string
	for _, p := range order {
		searchCache.Lock()
		until := searchCache.down[p.name]
		searchCache.Unlock()
		if time.Now().Before(until) {
			failures = append(failures, p.name+": failed a moment ago, skipped")
			continue
		}
		res, configured, err := p.run(ctx, env, r)
		if !configured {
			continue
		}
		if err != nil {
			searchCache.Lock()
			searchCache.down[p.name] = time.Now().Add(time.Minute)
			searchCache.Unlock()
			failures = append(failures, p.name+": "+err.Error())
			continue
		}
		if len(res) > r.n {
			res = res[:r.n]
		}
		if res == nil {
			res = []result{}
		}
		b, _ := json.Marshal(res)
		searchCache.Lock()
		searchCache.m[key] = cached{time.Now(), string(b)}
		searchCache.Unlock()
		return string(b), nil
	}
	if len(failures) == 0 {
		return "", errors.New("no search provider is configured: set SEARXNG_URL, OLLAMA_API_KEY or BRAVE_API_KEY (noskill secrets set NAME), or add \"ddg\" to the providers setting")
	}
	return "", errors.New("every search provider failed: " + strings.Join(failures, "; "))
}

// trustedClient is for endpoints the user configured themselves (their SearXNG, even on
// localhost or the LAN) and the fixed API hosts below.
var trustedClient = &http.Client{Timeout: 12 * time.Second}

func getJSON(ctx context.Context, env *tools.Env, u string, hdr map[string]string, out any) error {
	pu, err := url.Parse(u)
	if err != nil {
		return err
	}
	if err := env.Perms.CheckHost(pu.Hostname()); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return doJSON(trustedClient, req, out)
}

func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) { // the URL may carry a key; the error shouldn't
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func searxng(ctx context.Context, env *tools.Env, r searchReq) ([]result, bool, error) {
	base := env.Secret("SEARXNG_URL")
	if base == "" {
		base, _ = env.Setting("searxng_url")
	}
	if base == "" {
		// Detect a local instance without making the user configure it (private, recommended).
		if env.Perms.CheckHost("localhost") != nil {
			return nil, false, nil
		}
		base = "http://localhost:8888"
		probe, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
		defer cancel()
		c, err := (&net.Dialer{}).DialContext(probe, "tcp", "127.0.0.1:8888")
		if err != nil {
			return nil, false, nil
		}
		c.Close()
	}
	q := url.Values{"q": {r.query}, "format": {"json"}}
	if r.recency != "" {
		q.Set("time_range", r.recency)
	}
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	if err := getJSON(ctx, env, strings.TrimRight(base, "/")+"/search?"+q.Encode(), nil, &raw); err != nil {
		return nil, true, err
	}
	var res []result
	for _, m := range raw.Results {
		res = append(res, result{Title: str(m["title"]), URL: str(m["url"]), Snippet: str(m["content"]), Published: str(m["publishedDate"])})
	}
	return res, true, nil
}

func str(v any) string { s, _ := v.(string); return s }

func ollamaSearch(ctx context.Context, env *tools.Env, r searchReq) ([]result, bool, error) {
	key := env.Secret("OLLAMA_API_KEY")
	if key == "" {
		return nil, false, nil
	}
	if err := env.Perms.CheckHost("ollama.com"); err != nil {
		return nil, true, err
	}
	body, _ := json.Marshal(map[string]any{"query": r.query, "max_results": r.n})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://ollama.com/api/web_search", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	if err := doJSON(trustedClient, req, &raw); err != nil {
		return nil, true, err
	}
	var res []result
	for _, m := range raw.Results {
		res = append(res, result{Title: str(m["title"]), URL: str(m["url"]), Snippet: str(m["content"])})
	}
	return res, true, nil
}

func brave(ctx context.Context, env *tools.Env, r searchReq) ([]result, bool, error) {
	key := env.Secret("BRAVE_API_KEY")
	if key == "" {
		return nil, false, nil
	}
	q := url.Values{"q": {r.query}, "count": {strconv.Itoa(r.n)}}
	if f := map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}[r.recency]; f != "" {
		q.Set("freshness", f)
	}
	var raw struct {
		Web struct {
			Results []map[string]any `json:"results"`
		} `json:"web"`
	}
	if err := getJSON(ctx, env, braveURL+"?"+q.Encode(), map[string]string{"X-Subscription-Token": key}, &raw); err != nil {
		return nil, true, err
	}
	var res []result
	for _, m := range raw.Web.Results {
		res = append(res, result{Title: str(m["title"]), URL: str(m["url"]), Snippet: stripTags(str(m["description"])), Published: str(m["age"])})
	}
	return res, true, nil
}

var braveURL = "https://api.search.brave.com/res/v1/web/search"

func stripTags(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	return strings.TrimSpace(collapse(textOf(doc)))
}

// ddg scrapes DuckDuckGo's HTML page: no key, rate-limited, last resort.
func ddg(ctx context.Context, env *tools.Env, r searchReq) ([]result, bool, error) {
	if env.Perms.CheckHost("html.duckduckgo.com") != nil {
		return nil, false, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ddgURL+"?"+url.Values{"q": {r.query}}.Encode(), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; noskill)")
	resp, err := trustedClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, true, fmt.Errorf("status %d", resp.StatusCode)
	}
	doc, err := html.Parse(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, true, err
	}
	var res []result
	var cur *result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			cls := attr(n, "class")
			switch {
			case strings.Contains(cls, "result__a"):
				res = append(res, result{Title: strings.TrimSpace(textOf(n)), URL: ddgTarget(attr(n, "href"))})
				cur = &res[len(res)-1]
			case strings.Contains(cls, "result__snippet") && cur != nil:
				cur.Snippet = strings.TrimSpace(collapse(textOf(n)))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if len(res) == 0 {
		return nil, true, errors.New("no results in the page (DuckDuckGo may be rate-limiting)")
	}
	return res, true, nil
}

var ddgURL = "https://html.duckduckgo.com/html/"

func ddgTarget(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err == nil && strings.HasSuffix(u.Hostname(), "duckduckgo.com") {
		if t := u.Query().Get("uddg"); t != "" {
			return t
		}
	}
	return href
}
