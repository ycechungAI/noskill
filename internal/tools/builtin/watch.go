package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"noskill/internal/tools"
	"noskill/internal/watch"
)

// The watcher's tools (skills/watcher, FR-J1). Fetching and diffing happen here and in
// internal/watch; what a watch is and which items were seen is kept by the application.

// WatchService stores watches and their seen items, and schedules each watch as a job.
type WatchService interface {
	// WatchAdd stores the watch and creates its scheduler job.
	WatchAdd(ctx context.Context, w watch.Watch) (watch.Watch, error)
	WatchGet(ctx context.Context, name string) (watch.Watch, error)
	WatchList(ctx context.Context) ([]watch.Watch, error)
	// WatchRemove deletes the watch, its items and its job.
	WatchRemove(ctx context.Context, name string) error
	WatchSeen(ctx context.Context, name string) (map[string]bool, error)
	// WatchRecord marks items as seen and keeps the response validators for the next check.
	WatchRecord(ctx context.Context, name string, items []watch.Item, etag, lastModified string) error
}

// WatchRelevance is the optional filter (skills/watcher decision "relevant"): how relevant each
// item is to the watch's topic, 0–1, from the decision model. nil scores means no decision model
// is available and everything is passed on for the driver model to judge.
type WatchRelevance interface {
	WatchRelevant(ctx context.Context, topic string, items []watch.Item) ([]float64, error)
}

// watchRelevantMin is the relevance an item needs to be reported (the skill's threshold).
const watchRelevantMin = 0.6

func watchSvc(env *tools.Env) (WatchService, error) {
	if s, ok := env.Svc.(WatchService); ok && s != nil {
		return s, nil
	}
	return nil, errors.New("watches aren't available in this run")
}

const watchMaxNew = 20 // items returned per check; the rest are recorded as seen and counted

// Watcher returns the watcher builtins.
func Watcher() []tools.Builtin {
	return []tools.Builtin{
		{ID: "watch_add", Description: "Start watching a page, feed, GitHub project's releases or a search topic on a schedule.",
			Schema: json.RawMessage(`{"type":"object","required":["name","kind","target"],"properties":{` +
				`"name":{"type":"string","description":"A short name for the watch"},` +
				`"kind":{"type":"string","enum":["page","rss","github_releases","search"]},` +
				`"target":{"type":"string","description":"page or rss: the URL; github_releases: owner/repo; search: the query"},` +
				`"topic":{"type":"string","description":"What the user cares about, in their words"},` +
				`"schedule":{"type":"string","description":"A cron expression such as '0 8 * * *' or 'every:6h'. Default: daily at 08:00"}}}`),
			Run: runWatchAdd},
		{ID: "watch_list", Description: "List the watches: what, how often, when last checked.",
			Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
				s, err := watchSvc(env)
				if err != nil {
					return "", err
				}
				ws, err := s.WatchList(ctx)
				if err != nil {
					return "", err
				}
				return jsonOut(ws)
			}},
		{ID: "watch_check", Description: "Check a watch now and return the items that are new since the last check. The first check only records what is there.",
			Schema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
			Run:    runWatchCheck},
		{ID: "watch_remove", Description: "Stop watching and delete the watch and its schedule.",
			Schema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				s, err := watchSvc(env)
				if err != nil {
					return "", err
				}
				var a struct{ Name string }
				if err := arg(raw, &a); err != nil {
					return "", err
				}
				if err := s.WatchRemove(ctx, a.Name); err != nil {
					return "", err
				}
				return fmt.Sprintf(`{"removed":%q}`, a.Name), nil
			}},
	}
}

func runWatchAdd(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	s, err := watchSvc(env)
	if err != nil {
		return "", err
	}
	var a struct {
		Name, Kind, Target, Topic, Schedule string
	}
	if err := arg(raw, &a); err != nil {
		return "", err
	}
	w := watch.Watch{Name: a.Name, Kind: watch.Kind(a.Kind), Target: a.Target, Topic: a.Topic, Schedule: normalizeSchedule(a.Schedule)}
	if err := w.Validate(); err != nil {
		return "", err
	}
	if h := w.Host(); h != "" {
		if err := env.Perms.CheckHost(h); err != nil {
			return "", err
		}
	}
	w, err = s.WatchAdd(ctx, w)
	if err != nil {
		return "", err
	}
	return jsonOut(map[string]any{"added": w.Name, "kind": w.Kind, "target": w.Target, "schedule": w.Schedule,
		"note": "the first check records what is there; later checks report only what's new"})
}

// normalizeSchedule accepts a bare cron expression ("0 8 * * *") as well as the stored forms.
func normalizeSchedule(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(s, "cron:"), strings.HasPrefix(s, "every:"):
		return s
	case strings.HasPrefix(s, "@every "):
		return "every:" + strings.TrimSpace(strings.TrimPrefix(s, "@every "))
	}
	return "cron:" + s
}

func runWatchCheck(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	s, err := watchSvc(env)
	if err != nil {
		return "", err
	}
	var a struct{ Name string }
	if err := arg(raw, &a); err != nil {
		return "", err
	}
	w, err := s.WatchGet(ctx, a.Name)
	if err != nil {
		return "", err
	}
	if h := w.Host(); h != "" {
		if err := env.Perms.CheckHost(h); err != nil {
			return "", err
		}
	}
	items, etag, lm, unchanged, err := fetchWatch(ctx, env, w)
	if err != nil {
		return "", err
	}
	if unchanged {
		if err := s.WatchRecord(ctx, w.Name, nil, w.ETag, w.LastModified); err != nil {
			return "", err
		}
		return jsonOut(map[string]any{"name": w.Name, "new": []watch.Item{}, "unchanged": true})
	}
	seen, err := s.WatchSeen(ctx, w.Name)
	if err != nil {
		return "", err
	}
	baseline := w.LastChecked.IsZero()
	fresh := watch.New(items, seen)
	if err := s.WatchRecord(ctx, w.Name, fresh, etag, lm); err != nil {
		return "", err
	}
	out := map[string]any{"name": w.Name, "topic": w.Topic, "checked": len(items)}
	if baseline {
		out["baseline"] = true
		out["recorded"] = len(fresh)
		out["new"] = []watch.Item{}
		out["sample"] = fresh[:min(3, len(fresh))]
		return jsonOut(out)
	}
	more, dropped := 0, 0
	if len(fresh) > watchMaxNew {
		more, fresh = len(fresh)-watchMaxNew, fresh[:watchMaxNew]
	}
	if r, ok := env.Svc.(WatchRelevance); ok && w.Topic != "" && len(fresh) > 0 {
		if scores, err := r.WatchRelevant(ctx, w.Topic, fresh); err == nil && len(scores) == len(fresh) {
			kept := fresh[:0:0]
			for i, it := range fresh {
				if scores[i] >= watchRelevantMin {
					kept = append(kept, it)
				}
			}
			dropped, fresh = len(fresh)-len(kept), kept
		}
	}
	if fresh == nil {
		fresh = []watch.Item{} // "new": [] rather than null, so a small model reads "nothing new"
	}
	out["new"], out["not_shown"], out["not_relevant"] = fresh, more, dropped
	return jsonOut(out)
}

// fetchWatch fetches what a watch watches and turns it into items. unchanged is true when the
// server answered 304 to the stored validators.
func fetchWatch(ctx context.Context, env *tools.Env, w watch.Watch) (items []watch.Item, etag, lastModified string, unchanged bool, err error) {
	switch w.Kind {
	case watch.Search:
		raw, _ := json.Marshal(map[string]any{"query": w.Target, "max_results": 10, "recency": "month"})
		out, err := runSearch(ctx, env, raw)
		if err != nil {
			return nil, "", "", false, err
		}
		var rs []result
		if err := json.Unmarshal([]byte(out), &rs); err != nil {
			return nil, "", "", false, fmt.Errorf("couldn't read the search results: %v", err)
		}
		for _, r := range rs {
			if it, ok := watch.SearchItem(r.Title, r.URL, r.Snippet); ok {
				items = append(items, it)
			}
		}
		return items, "", "", false, nil
	case watch.GitHub:
		u := "https://api.github.com/repos/" + w.Target + "/releases?per_page=30"
		hdr := map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
		if tok := env.Secret("GITHUB_TOKEN"); tok != "" {
			hdr["Authorization"] = "Bearer " + tok
		}
		body, resp, err := getConditional(ctx, env, u, hdr, w)
		if err != nil {
			return nil, "", "", false, err
		}
		if resp.StatusCode == http.StatusNotModified {
			return nil, "", "", true, nil
		}
		items, err = watch.ParseReleases(body)
		return items, resp.Header.Get("ETag"), "", false, err
	case watch.RSS:
		body, resp, err := getConditional(ctx, env, w.Target, map[string]string{"Accept": "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.1"}, w)
		if err != nil {
			return nil, "", "", false, err
		}
		if resp.StatusCode == http.StatusNotModified {
			return nil, "", "", true, nil
		}
		items, err = watch.ParseFeed(body)
		return items, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, err
	case watch.Page:
		body, resp, err := getConditional(ctx, env, w.Target, map[string]string{"Accept": "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1"}, w)
		if err != nil {
			return nil, "", "", false, err
		}
		if resp.StatusCode == http.StatusNotModified {
			return nil, "", "", true, nil
		}
		text := string(body)
		if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "html") || ct == "" {
			ex, err := extractHTML(strings.NewReader(text), resp.Request.URL)
			if err != nil {
				return nil, "", "", false, err
			}
			text = ex.Text
		}
		return watch.PageItems(strings.ToValidUTF8(text, ""), resp.Request.URL.String()), resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
	}
	return nil, "", "", false, fmt.Errorf("unknown watch kind %q", w.Kind)
}

// getConditional GETs a URL through the permission-checked, public-only client, sending the
// stored ETag / Last-Modified so an unchanged resource costs one small answer.
func getConditional(ctx context.Context, env *tools.Env, rawURL string, hdr map[string]string, w watch.Watch) ([]byte, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if w.ETag != "" {
		req.Header.Set("If-None-Match", w.ETag)
	}
	if w.LastModified != "" {
		req.Header.Set("If-Modified-Since", w.LastModified)
	}
	resp, err := fetchClient(env).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, nil, fmt.Errorf("couldn't fetch %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, resp, nil
	}
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("%s answered %d", u.Host, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	return b, resp, err
}
