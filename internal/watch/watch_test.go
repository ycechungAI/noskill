package watch

import (
	"strings"
	"testing"
)

const pageV1 = `# Release notes

Ollama 0.35 adds decision models through the new /v1/systemone endpoint.

Faster loading of large models when the disk cache is warm.

Visitors today: 1,204. Last updated 3 March 2026.

Subscribe to our newsletter for updates and offers.`

// Only ads, dates and counters change: nothing is new.
const pageCosmetic = `# Release notes

Ollama 0.35 adds decision models through the new /v1/systemone endpoint.

Faster loading of large models when the disk cache is warm.

Visitors today: 1,377. Last updated 9 March 2026.

Subscribe to our newsletter for updates, offers and more.`

const pageNew = pageCosmetic + `

Ollama 0.36 fixes a crash when two models are loaded at the same time.`

func fps(items []Item) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.Fingerprint] = true
	}
	return m
}

// skills/watcher "Tests": cosmetic-only changes → no alert; new content → alert; dedupe.
func TestPageDiff(t *testing.T) {
	seen := fps(PageItems(pageV1, "https://example.org/notes"))
	if len(seen) != 3 { // heading, two paragraphs, and the counters line; the newsletter line is furniture
		t.Logf("baseline items: %d", len(seen))
	}
	if got := New(PageItems(pageCosmetic, "https://example.org/notes"), seen); len(got) != 0 {
		t.Fatalf("cosmetic change reported as new: %+v", got)
	}
	got := New(PageItems(pageNew, "https://example.org/notes"), seen)
	if len(got) != 1 || !strings.Contains(got[0].Title, "0.36") || got[0].URL != "https://example.org/notes" {
		t.Fatalf("new paragraph: %+v", got)
	}
	// Once recorded, it isn't new again, and duplicates inside one page count once.
	for _, it := range got {
		seen[it.Fingerprint] = true
	}
	if again := New(PageItems(pageNew+"\n\n"+got[0].Summary, ""), seen); len(again) != 0 {
		t.Fatalf("reported twice: %+v", again)
	}
}

const rss = `<?xml version="1.0"?><rss version="2.0"><channel><title>Blog</title>
<item><title>First post</title><link>https://example.org/1</link><guid>post-1</guid><description>&lt;p&gt;Hello &lt;b&gt;world&lt;/b&gt;&lt;/p&gt;</description></item>
<item><title>Second post</title><link>https://example.org/2</link></item>
</channel></rss>`

const atom = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>Blog</title>
<entry><title>Atom entry</title><id>tag:example.org,2026:1</id><link rel="alternate" href="https://example.org/a"/><summary>Short</summary></entry></feed>`

func TestParseFeeds(t *testing.T) {
	items, err := ParseFeed([]byte(rss))
	if err != nil || len(items) != 2 || items[0].Title != "First post" || items[0].Summary != "Hello world" || items[1].URL != "https://example.org/2" {
		t.Fatalf("rss: %+v %v", items, err)
	}
	// A changed title with the same guid isn't a new item.
	renamed := strings.Replace(rss, "First post", "First post (edited)", 1)
	again, _ := ParseFeed([]byte(renamed))
	if got := New(again, fps(items)); len(got) != 0 {
		t.Errorf("an edited title was reported: %+v", got)
	}
	items, err = ParseFeed([]byte(atom))
	if err != nil || len(items) != 1 || items[0].URL != "https://example.org/a" {
		t.Fatalf("atom: %+v %v", items, err)
	}
	if _, err := ParseFeed([]byte("<html></html>")); err == nil {
		t.Error("an HTML page was read as a feed")
	}
}

func TestParseReleases(t *testing.T) {
	items, err := ParseReleases([]byte(`[
	 {"tag_name":"v0.36.0","name":"v0.36.0","html_url":"https://github.com/o/r/releases/tag/v0.36.0","body":"Fixes **a crash**"},
	 {"tag_name":"v0.37.0-rc1","name":"Candidate","html_url":"u","prerelease":true},
	 {"tag_name":"v0.38.0","draft":true}]`))
	if err != nil || len(items) != 2 {
		t.Fatalf("%+v %v", items, err)
	}
	if items[0].Title != "v0.36.0" || !strings.Contains(items[1].Title, "pre-release") || !strings.Contains(items[1].Title, "v0.37.0-rc1") {
		t.Errorf("titles: %q, %q", items[0].Title, items[1].Title)
	}
	if _, err := ParseReleases([]byte(`{"message":"Not Found"}`)); err == nil {
		t.Error("an error object was read as releases")
	}
}

func TestSearchItemsIgnoreTrackingParameters(t *testing.T) {
	a, _ := SearchItem("A", "https://www.example.org/post/?utm_source=x", "")
	b, _ := SearchItem("A", "https://example.org/post", "")
	if a.Fingerprint != b.Fingerprint {
		t.Error("the same article under two URLs counted twice")
	}
	if _, ok := SearchItem("none", "", ""); ok {
		t.Error("an item without a URL")
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		w    Watch
		ok   bool
		want string // the target after validation
	}{
		{Watch{Name: "ollama", Kind: GitHub, Target: "https://github.com/ollama/ollama/"}, true, "ollama/ollama"},
		{Watch{Name: "ollama", Kind: GitHub, Target: "ollama"}, false, ""},
		{Watch{Name: "blog", Kind: RSS, Target: "https://example.org/feed.xml"}, true, "https://example.org/feed.xml"},
		{Watch{Name: "blog", Kind: Page, Target: "example.org"}, false, ""},
		{Watch{Name: "../x", Kind: Search, Target: "q"}, false, ""},
		{Watch{Name: "news", Kind: Search, Target: "local llm news"}, true, "local llm news"},
		{Watch{Name: "news", Kind: "email", Target: "q"}, false, ""},
	} {
		err := c.w.Validate()
		if (err == nil) != c.ok || (c.ok && c.w.Target != c.want) {
			t.Errorf("%+v → %v", c.w, err)
		}
		if c.ok && c.w.Schedule != DefaultSchedule {
			t.Errorf("schedule %q", c.w.Schedule)
		}
	}
	if h := (Watch{Kind: GitHub}).Host(); h != "api.github.com" {
		t.Errorf("host %q", h)
	}
}
