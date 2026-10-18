package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"noskill/internal/tools"
	"noskill/internal/watch"
)

// memWatches is an in-memory WatchService (and WatchRelevance) for the tools' tests.
type memWatches struct {
	tools.Services
	mu       sync.Mutex
	watches  map[string]watch.Watch
	seen     map[string]map[string]bool
	relevant func(topic string, it watch.Item) float64
}

func newMem() *memWatches {
	return &memWatches{watches: map[string]watch.Watch{}, seen: map[string]map[string]bool{}}
}

func (m *memWatches) WatchAdd(_ context.Context, w watch.Watch) (watch.Watch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.watches[w.Name]; ok {
		return w, fmt.Errorf("a watch named %q already exists", w.Name)
	}
	m.watches[w.Name], m.seen[w.Name] = w, map[string]bool{}
	return w, nil
}
func (m *memWatches) WatchGet(_ context.Context, name string) (watch.Watch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.watches[name]
	if !ok {
		return w, fmt.Errorf("no such watch %q", name)
	}
	return w, nil
}
func (m *memWatches) WatchList(context.Context) ([]watch.Watch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []watch.Watch
	for _, w := range m.watches {
		out = append(out, w)
	}
	return out, nil
}
func (m *memWatches) WatchRemove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watches, name)
	delete(m.seen, name)
	return nil
}
func (m *memWatches) WatchSeen(_ context.Context, name string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for k := range m.seen[name] {
		out[k] = true
	}
	return out, nil
}
func (m *memWatches) WatchRecord(_ context.Context, name string, items []watch.Item, etag, lm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		m.seen[name][it.Fingerprint] = true
	}
	w := m.watches[name]
	w.ETag, w.LastModified, w.LastChecked = etag, lm, w.LastChecked.Add(1) // any non-zero time is "checked"
	if w.LastChecked.IsZero() {
		w.LastChecked = w.CreatedAt.AddDate(2026, 0, 0)
	}
	m.watches[name] = w
	return nil
}
func (m *memWatches) WatchRelevant(_ context.Context, topic string, items []watch.Item) ([]float64, error) {
	if m.relevant == nil {
		return nil, nil
	}
	out := make([]float64, len(items))
	for i, it := range items {
		out[i] = m.relevant(topic, it)
	}
	return out, nil
}

func callWatch(t *testing.T, id string, e *tools.Env, args string) (map[string]any, error) {
	t.Helper()
	b := Watcher()
	for _, x := range b {
		if x.ID == id {
			out, err := x.Run(context.Background(), e, json.RawMessage(args))
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("%s returned %q", id, out)
			}
			return m, nil
		}
	}
	t.Fatalf("no tool %s", id)
	return nil, nil
}

func TestWatchAddValidatesAndChecksTheNetworkPermission(t *testing.T) {
	mem := newMem()
	e := env([]string{"good.test"}, nil, nil)
	e.Svc = mem
	if _, err := callWatch(t, "watch_add", e, `{"name":"blog","kind":"rss","target":"https://evil.test/feed"}`); err == nil || !strings.Contains(err.Error(), "network permission") {
		t.Errorf("a host outside the permission was accepted: %v", err)
	}
	if _, err := callWatch(t, "watch_add", e, `{"name":"blog","kind":"rss","target":"not a url"}`); err == nil {
		t.Error("a bad target was accepted")
	}
	m, err := callWatch(t, "watch_add", e, `{"name":"blog","kind":"rss","target":"https://good.test/feed","schedule":"0 7 * * *"}`)
	if err != nil || mem.watches["blog"].Schedule != "cron:0 7 * * *" || m["added"] != "blog" {
		t.Fatalf("%v %v %+v", m, err, mem.watches["blog"])
	}
	if _, err := callWatch(t, "watch_add", e, `{"name":"blog","kind":"rss","target":"https://good.test/feed"}`); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := callWatch(t, "watch_remove", e, `{"name":"blog"}`); err != nil || len(mem.watches) != 0 {
		t.Errorf("remove: %v %v", err, mem.watches)
	}
	bare := env(nil, nil, nil)
	if _, err := callWatch(t, "watch_check", bare, `{"name":"x"}`); err == nil || !strings.Contains(err.Error(), "aren't available") {
		t.Errorf("without a service: %v", err)
	}
}

func TestPageWatchReportsOnlyNewContent(t *testing.T) {
	toLocal(t)
	var body atomicString
	body.Store(`<html><body><main><h1>Notes</h1><p>Ollama 0.35 adds decision models through the new endpoint.</p><p>Visitors today: 1,204.</p></main></body></html>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body.Load())
	}))
	defer srv.Close()
	mem := newMem()
	e := env([]string{"good.test"}, nil, nil)
	e.Svc = mem
	target := "http://good.test:" + port(srv) + "/notes"
	if _, err := callWatch(t, "watch_add", e, fmt.Sprintf(`{"name":"notes","kind":"page","target":%q,"topic":"ollama releases"}`, target)); err != nil {
		t.Fatal(err)
	}
	m, err := callWatch(t, "watch_check", e, `{"name":"notes"}`)
	if err != nil || m["baseline"] != true || len(m["new"].([]any)) != 0 {
		t.Fatalf("first check: %v %v", m, err)
	}
	// Only the counter changes.
	body.Store(strings.Replace(body.Load(), "1,204", "1,377", 1))
	m, err = callWatch(t, "watch_check", e, `{"name":"notes"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m["new"].([]any)) != 0 || m["baseline"] != nil {
		t.Fatalf("a counter change was reported: %v", m)
	}
	// A real new paragraph.
	body.Store(strings.Replace(body.Load(), "</main>", "<p>Ollama 0.36 fixes a crash when two models load at once.</p></main>", 1))
	m, _ = callWatch(t, "watch_check", e, `{"name":"notes"}`)
	items := m["new"].([]any)
	if len(items) != 1 || !strings.Contains(items[0].(map[string]any)["title"].(string), "0.36") {
		t.Fatalf("new paragraph: %v", m)
	}
	if m, _ = callWatch(t, "watch_check", e, `{"name":"notes"}`); len(m["new"].([]any)) != 0 {
		t.Errorf("reported twice: %v", m)
	}
}

func TestFeedWatchUsesValidatorsAndFiltersByTopic(t *testing.T) {
	toLocal(t)
	feed := func(items ...string) string {
		return `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title>` + strings.Join(items, "") + `</channel></rss>`
	}
	item := func(id, title string) string {
		return fmt.Sprintf(`<item><title>%s</title><link>https://example.org/%s</link><guid>%s</guid></item>`, title, id, id)
	}
	var body atomicString
	body.Store(feed(item("1", "Ollama 0.35")))
	var conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := fmt.Sprintf(`"%d"`, len(body.Load()))
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, body.Load())
	}))
	defer srv.Close()
	mem := newMem()
	mem.relevant = func(topic string, it watch.Item) float64 {
		if strings.Contains(it.Title, "Ollama") {
			return 0.9
		}
		return 0.1
	}
	e := env([]string{"good.test"}, nil, nil)
	e.Svc = mem
	callWatch(t, "watch_add", e, fmt.Sprintf(`{"name":"blog","kind":"rss","target":"http://good.test:%s/feed","topic":"ollama"}`, port(srv)))
	callWatch(t, "watch_check", e, `{"name":"blog"}`) // baseline
	m, _ := callWatch(t, "watch_check", e, `{"name":"blog"}`)
	if m["unchanged"] != true || conditional != 1 {
		t.Fatalf("an unchanged feed should cost a 304: %v (%d conditional)", m, conditional)
	}
	body.Store(feed(item("1", "Ollama 0.35"), item("2", "Ollama 0.36"), item("3", "Cooking with cast iron")))
	m, _ = callWatch(t, "watch_check", e, `{"name":"blog"}`)
	items := m["new"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != "Ollama 0.36" || m["not_relevant"] != float64(1) {
		t.Fatalf("topic filter: %v", m)
	}
	// The dropped item is remembered as seen, so it doesn't come back.
	if m, _ = callWatch(t, "watch_check", e, `{"name":"blog"}`); m["unchanged"] != true {
		t.Errorf("%v", m)
	}
}

type atomicString struct {
	mu sync.Mutex
	s  string
}

func (a *atomicString) Store(s string) { a.mu.Lock(); a.s = s; a.mu.Unlock() }
func (a *atomicString) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.s }
