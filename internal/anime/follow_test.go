package anime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/store"
)

// The release names of the three followed shows, as Nyaa lists them (2026-10-18).
func TestFollowedShowsReleaseNamesParse(t *testing.T) {
	ri := Follow{Title: "Renegade Immortal", Match: []string{"renegade immortal"}}
	bt := Follow{Title: "Battle Through the Heavens", Match: []string{"battle through the heavens"}}
	mj := Follow{Title: "A Mortal's Journey", Match: []string{"mortal's journey"}}
	for _, c := range []struct {
		f    Follow
		name string
		ep   float64
	}{
		{ri, "[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 162", 162},
		{ri, "[BonoboSubs][4k]Renegade Immortal - 仙逆 Xian Ni - 162", 162},
		{ri, "[Shridhuu][1080p] Renegade Immortal / 仙逆 / Xian Ni - EP162", 162},
		{bt, "[Shridhuu][1080p HBR] Battle Through the Heavens / 斗破苍穹 / Doupo Cangqiong - S05E215", 215},
		{bt, "[FSP] Battle Through The Heavens NF - 214 [4K]", 214},
		{mj, "[FSP DN] A Record of a Mortal’s Journey to Immortality - 196 (1080p) | 凡人修仙传", 196},
		{mj, "[FSP DN] A Record of a Mortal’s Journey to Immortality - 194 V3 (1080p) | 凡人修仙传", 194},
	} {
		if !c.f.matches(c.name) {
			t.Errorf("%q doesn't match %q", c.name, c.f.Title)
		}
		r, _ := ParseNamed(c.name, c.f.Match, 0)
		if r.Kind != "episode" || r.Episode != c.ep {
			t.Errorf("%q → kind %q episode %v, want episode %v", c.name, r.Kind, r.Episode, c.ep)
		}
	}
	if ri.matches("[Group] Some Other Show - 162") {
		t.Error("matched another show")
	}
}

func TestFollowTargetIsFlatAndNamedLikeTheUsersFiles(t *testing.T) {
	f := Follow{Title: "Renegade Immortal"}
	if got := FollowTarget("/Volumes/RED/@DONG+", f, 163, "mkv"); got != "/Volumes/RED/@DONG+/Renegade Immortal Episode 163.mkv" {
		t.Errorf("got %q", got)
	}
	f.Name = "Battle Through the Heavens Season 8"
	if got := FollowTarget("/d", f, 215, "mp4"); got != "/d/Battle Through the Heavens Season 8 Episode 215.mp4" {
		t.Errorf("got %q", got)
	}
}

// followFixture: Renegade Immortal in a folder up to episode 162; Nyaa lists 161-163 in several sizes.
func followFixture(t *testing.T, releases [][2]string) (*Tracker, *fakeTransmission, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dest, downloads, trash := filepath.Join(dir, "@DONG+"), filepath.Join(dir, "Downloads"), filepath.Join(dir, "Trash")
	for _, d := range []string{dest, downloads, trash} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(dest, "Renegade Immortal Episode 161.mkv"), []byte("161"), 0o644)
	os.WriteFile(filepath.Join(dest, "Renegade Immortal Episode 162.mkv"), []byte("162"), 0o644)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:nyaa="https://nyaa.si/xmlns/nyaa" version="2.0"><channel>`)
		for i, it := range releases {
			fmt.Fprintf(w, `<item><title>%s</title><link>%s/download/%d.torrent</link><guid>%s/view/%d</guid><pubDate>%s</pubDate>
<nyaa:seeders>10</nyaa:seeders><nyaa:infoHash>%040d</nyaa:infoHash><nyaa:size>%s</nyaa:size><nyaa:categoryId>1_2</nyaa:categoryId><nyaa:trusted>No</nyaa:trusted><nyaa:remake>No</nyaa:remake></item>`,
				it[0], srv.URL, i, srv.URL, i, now.Add(-2*time.Hour).Format("Mon, 02 Jan 2006 15:04:05 -0700"), i+1, it[1])
		}
		fmt.Fprint(w, `</channel></rss>`)
	}))
	t.Cleanup(srv.Close)
	tr := &fakeTransmission{names: map[string]string{}}
	for i, it := range releases {
		tr.names[fmt.Sprintf("%s/download/%d.torrent", srv.URL, i)] = it[0]
	}
	trs := httptest.NewServer(tr)
	t.Cleanup(trs.Close)
	tk := &Tracker{State: &State{DB: db, Now: func() time.Time { return now }}, Nyaa: &Nyaa{Base: srv.URL},
		Client: &Transmission{Base: trs.URL, Label: "noskill-anime", SavePath: downloads}, Files: fakeFiles{trash},
		Now:      func() time.Time { return now },
		Settings: Settings{HDMaxMB: 500, Follows: []Follow{{Title: "Renegade Immortal", Match: []string{"renegade immortal", "xian ni"}, Dest: dest}}}}
	return tk, tr, dest, downloads
}

// "latest episode of the week, smallest file": 163 is the newest; 161 and 162 are already in the
// folder; the 740 MiB file beats the 1080p (845 MiB) and the 4K ones, and HD size limits don't apply.
func TestFollowedShowFetchesOnlyTheNewestEpisodeAsTheSmallestFile(t *testing.T) {
	tk, tr, dest, downloads := followFixture(t, [][2]string{
		{"[Shridhuu][1080p] Renegade Immortal / 仙逆 / Xian Ni - EP163", "850 MiB"},
		{"[Shridhuu][4K] Renegade Immortal / 仙逆 / Xian Ni - EP163", "1.6 GiB"},
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163", "740 MiB"},
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 162", "700 MiB"},
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 161", "590 MiB"},
		{"[Group] Renegade Immortal - 仙逆 - 163 [CHS]", "100 MiB"}, // not English: ignored although smaller
	})
	rep, err := tk.Run(context.Background(), "download")
	if err != nil || len(rep.Problems) != 0 || len(rep.Added) != 1 || !strings.Contains(rep.Added[0], "episode 163") || !strings.Contains(rep.Added[0], "BonoboSubs") {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(tr.torrents) != 1 || tr.torrents[0]["name"] != "[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163" {
		t.Fatalf("torrents %v", tr.torrents)
	}
	// Running again doesn't add it twice.
	if rep, _ = tk.Run(context.Background(), "download"); len(rep.Added) != 0 || len(tr.torrents) != 1 {
		t.Errorf("second run: %+v, %d torrents", rep, len(tr.torrents))
	}

	// The download finishes: it is renamed like the user's other files and filed flat into the folder.
	tr.torrents[0]["percentDone"] = 1.0
	tr.torrents[0]["status"] = 0
	tr.torrents[0]["downloadDir"] = downloads
	tr.torrents[0]["files"] = []map[string]any{{"name": "[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163.mkv", "length": 3, "bytesCompleted": 3}}
	os.WriteFile(filepath.Join(downloads, "[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163.mkv"), []byte("163"), 0o644)
	tr.torrents[0]["name"] = "[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163.mkv"
	tr.torrents[0]["labels"] = []string{"noskill-anime"}
	rep, err = tk.Run(context.Background(), "file")
	if err != nil || len(rep.Problems) != 0 || len(rep.Filed) != 1 {
		t.Fatalf("filing: %+v %v", rep, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "Renegade Immortal Episode 163.mkv")); string(b) != "163" {
		ents, _ := os.ReadDir(dest)
		t.Errorf("episode 163 isn't in the folder under the user's naming: %v", ents)
	}
}

// With nothing from the show in the folder yet, the newest episode out now is the starting point:
// nothing old is fetched.
func TestFollowedShowStartsFromWhatIsOutNow(t *testing.T) {
	tk, tr, dest, _ := followFixture(t, [][2]string{
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 162", "700 MiB"},
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 161", "590 MiB"},
	})
	os.Remove(filepath.Join(dest, "Renegade Immortal Episode 161.mkv"))
	os.Remove(filepath.Join(dest, "Renegade Immortal Episode 162.mkv"))
	rep, _ := tk.Run(context.Background(), "download")
	if len(rep.Added) != 0 || len(tr.torrents) != 0 {
		t.Errorf("fetched something old: %+v", rep)
	}
}

// `from` says what the user already has, whatever the folder shows (they watch some elsewhere).
func TestFollowFromOverridesTheFolder(t *testing.T) {
	tk, tr, _, _ := followFixture(t, [][2]string{
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 164", "700 MiB"},
		{"[BonoboSubs]Renegade Immortal - 仙逆 Xian Ni - 163", "640 MiB"},
	})
	tk.Settings.Follows[0].From = 164
	if rep, _ := tk.Run(context.Background(), "download"); len(rep.Added) != 0 || len(tr.torrents) != 0 {
		t.Errorf("fetched an episode the user has: %+v", rep)
	}
	tk.Settings.Follows[0].From = 163
	if rep, _ := tk.Run(context.Background(), "download"); len(rep.Added) != 1 || !strings.Contains(rep.Added[0], "episode 164") {
		t.Errorf("should fetch 164: %+v", rep)
	}
}
