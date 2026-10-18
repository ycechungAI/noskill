package anime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A mirror's feed has no links: the magnet comes from the item's page.
func TestMirrorFeedResolvesMagnet(t *testing.T) {
	hash := strings.Repeat("ab", 20)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprintf(w, `<rss xmlns:nyaa="x"><channel>
<item><title>[G] Show S2 - 03 [480p] | Show 2nd Season</title><guid>%s/view/42</guid>
<pubDate>Sun, 11 Oct 2026 22:41:56 +0700</pubDate><nyaa:infoHash></nyaa:infoHash><nyaa:seeders>9</nyaa:seeders></item>
<item><title>[G] Elsewhere - 01 [480p]</title><guid>https://evil.example/view/1</guid><pubDate>Sun, 11 Oct 2026 22:41:56 +0700</pubDate></item>
</channel></rss>`, srv.URL)
		case "/view/42":
			fmt.Fprintf(w, `<a href="https://nyaa.si/download/42.torrent">Download</a> <a href="magnet:?xt=urn:btih:%s&amp;dn=x&amp;tr=udp%%3A%%2F%%2Ft">Magnet</a>`, strings.ToUpper(hash))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	n := &Nyaa{Base: srv.URL}
	items, err := n.Search(context.Background(), "show")
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %+v", err, items) // the item whose page is on another site is dropped
	}
	if items[0].TorrentURL != "" || items[0].Seeders != 9 {
		t.Fatalf("%+v", items[0])
	}
	if err := n.Resolve(context.Background(), &items[0]); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(items[0].TorrentURL, "magnet:?xt=urn:btih:"+strings.ToUpper(hash)+"&dn=x") || items[0].InfoHash != hash {
		t.Fatalf("%+v", items[0])
	}
	r, c := ParseNamed(items[0].Title, []string{"Show 2nd Season"}, 2)
	if c < MatchThreshold || r.Episode != 3 || r.Resolution != "480p" {
		t.Fatalf("%+v %.2f", r, c)
	}
}

func TestFeedURLNeedsHTTPS(t *testing.T) {
	if _, err := (&Nyaa{Base: "http://nyaa.example"}).FeedURL("x"); err == nil {
		t.Error("plain http accepted")
	}
	u, err := (&Nyaa{Base: "https://nyaa.media"}).FeedURL("Frieren")
	if err != nil || u != "https://nyaa.media/?c=1_2&f=0&page=rss&q=Frieren" {
		t.Errorf("%s %v", u, err)
	}
}
