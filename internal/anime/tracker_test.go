package anime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/store"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) // FALL 2026

// fakeAniList answers Season queries from a table.
func fakeAniList(t *testing.T, bySeason map[string][]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		key := fmt.Sprintf("%v %v", req.Variables["s"], req.Variables["y"])
		ms := bySeason[key]
		if ms == nil {
			ms = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Page": map[string]any{"media": ms}}})
	}))
}

func media(id int, romaji, english, season string, year int, status string, score, pop int) map[string]any {
	return map[string]any{"id": id, "title": map[string]any{"romaji": romaji, "english": english}, "format": "TV",
		"episodes": 12, "status": status, "season": season, "seasonYear": year, "genres": []string{"Fantasy"},
		"averageScore": score, "popularity": pop, "siteUrl": fmt.Sprintf("https://anilist.co/anime/%d", id)}
}

type rss struct {
	title string
	age   time.Duration
	seed  int
}

func fakeNyaa(items []rss) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "rss" || r.URL.Query().Get("c") != NyaaEnglishSubs {
			http.Error(w, "bad query", 400)
			return
		}
		fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:nyaa="https://nyaa.si/xmlns/nyaa" version="2.0"><channel>`)
		for i, it := range items {
			fmt.Fprintf(w, `<item><title>%s</title><link>%s/download/%d.torrent</link><guid>%s/view/%d</guid>
<pubDate>%s</pubDate><nyaa:seeders>%d</nyaa:seeders><nyaa:infoHash>%040d</nyaa:infoHash>
<nyaa:categoryId>1_2</nyaa:categoryId><nyaa:trusted>Yes</nyaa:trusted><nyaa:remake>No</nyaa:remake></item>`,
				it.title, srv.URL, i, srv.URL, i, now.Add(-it.age).Format("Mon, 02 Jan 2006 15:04:05 -0700"), it.seed, i)
		}
		fmt.Fprint(w, `</channel></rss>`)
	}))
	return srv
}

// fakeTransmission keeps torrents in memory; added torrents are named after the feed item.
type fakeTransmission struct {
	mu       sync.Mutex
	torrents []map[string]any
	removed  []string
	stopped  []string
	names    map[string]string // torrent URL → release name
}

func (f *fakeTransmission) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Transmission-Session-Id") != "s1" {
		w.Header().Set("X-Transmission-Session-Id", "s1")
		w.WriteHeader(http.StatusConflict)
		return
	}
	var req struct {
		Method    string         `json:"method"`
		Arguments map[string]any `json:"arguments"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	args := map[string]any{}
	switch req.Method {
	case "session-get":
		args = map[string]any{"rpc-version": 18, "version": "4.1.3"}
	case "torrent-add":
		u := req.Arguments["filename"].(string)
		hash := fmt.Sprintf("%040x", len(f.torrents)+1)
		f.torrents = append(f.torrents, map[string]any{"hashString": hash, "name": f.names[u], "status": 4,
			"labels": req.Arguments["labels"], "downloadDir": req.Arguments["download-dir"], "percentDone": 0.0})
		args["torrent-added"] = map[string]any{"hashString": hash}
	case "torrent-set":
	case "torrent-stop":
		f.stopped = append(f.stopped, req.Arguments["ids"].([]any)[0].(string))
		for _, t := range f.torrents {
			if t["hashString"] == req.Arguments["ids"].([]any)[0] {
				t["status"] = 0
			}
		}
	case "torrent-get":
		var out []map[string]any
		ids, _ := req.Arguments["ids"].([]any)
		for _, t := range f.torrents {
			if ids == nil || ids[0] == t["hashString"] {
				out = append(out, t)
			}
		}
		args["torrents"] = out
	case "torrent-remove":
		if req.Arguments["delete-local-data"] != false {
			http.Error(w, "must keep data", 400)
			return
		}
		id := req.Arguments["ids"].([]any)[0].(string)
		f.removed = append(f.removed, id)
		keep := f.torrents[:0]
		for _, t := range f.torrents {
			if t["hashString"] != id {
				keep = append(keep, t)
			}
		}
		f.torrents = keep
	}
	json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": args})
}

type fakeAsk struct{ items map[string]approvals.Item }

func (a *fakeAsk) Create(_ context.Context, n approvals.New) (approvals.Item, error) {
	id := fmt.Sprintf("a%d", len(a.items)+1)
	it := approvals.Item{ID: id, Kind: n.Kind, Summary: n.Summary, Fields: n.Fields, Status: approvals.Pending}
	a.items[id] = it
	return it, nil
}

func (a *fakeAsk) Get(_ context.Context, id string) (approvals.Item, error) {
	it, ok := a.items[id]
	if !ok {
		return it, approvals.ErrNotFound
	}
	return it, nil
}

// fakeFiles does real copies; "trash" moves into a folder so the test can see it.
type fakeFiles struct{ trash string }

func (f fakeFiles) Mkdir(_ context.Context, p string) error { return os.MkdirAll(p, 0o755) }
func (f fakeFiles) Rename(_ context.Context, from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s exists", to)
	}
	return os.Rename(from, to)
}
func (f fakeFiles) Copy(_ context.Context, from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s exists", to)
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}
func (f fakeFiles) TrashPath(_ context.Context, p string) error {
	// Like the real trashes, never reuse a name: a second "Show S01E01.mkv" becomes "Show S01E01.mkv.2".
	to := filepath.Join(f.trash, filepath.Base(p))
	for i := 2; ; i++ {
		if _, err := os.Lstat(to); err != nil {
			break
		}
		to = filepath.Join(f.trash, fmt.Sprintf("%s.%d", filepath.Base(p), i))
	}
	return os.Rename(p, to)
}

func TestTrackerEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dest, other, downloads, trash := filepath.Join(dir, "Anime"), filepath.Join(dir, "Old anime"), filepath.Join(dir, "Downloads"), filepath.Join(dir, "Trash")
	for _, d := range []string{dest, other, downloads, trash} {
		os.MkdirAll(d, 0o755)
	}
	const title = "Sousou no Frieren 2nd Season"
	// Already stored: episode 5 in the chosen folder, and all of "Owned Show" elsewhere.
	os.MkdirAll(filepath.Join(dest, title), 0o755)
	os.WriteFile(filepath.Join(dest, title, title+" - 05.mkv"), []byte("ep5"), 0o644)
	os.WriteFile(filepath.Join(other, "[SubsPlease] Owned Show - 01 (480p).mkv"), []byte("x"), 0o644)

	al := fakeAniList(t, map[string][]map[string]any{
		"FALL 2026": {
			media(2, title, "", "FALL", 2026, "RELEASING", 85, 200000),
			media(3, "Owned Show", "", "FALL", 2026, "RELEASING", 90, 300000),
			media(4, "Meh Show", "", "FALL", 2026, "RELEASING", 30, 1000),
		},
		"SUMMER 2026": {
			media(5, "Long Show", "", "SUMMER", 2026, "RELEASING", 70, 50000),  // second cour: still new
			media(6, "Ended Show", "", "SUMMER", 2026, "FINISHED", 95, 400000), // finished: old
		},
		"WINTER 2027": {media(7, "Next Show", "", "WINTER", 2027, "NOT_YET_RELEASED", 0, 150000)},
	})
	defer al.Close()
	ny := fakeNyaa([]rss{
		{"[SubsPlease] Sousou no Frieren S2 - 01 (480p) [AAAA1111].mkv", 72 * time.Hour, 100},
		{"[SubsPlease] Sousou no Frieren S2 - 01 (1080p) [AAAA2222].mkv", 72 * time.Hour, 900},
		{"[SubsPlease] Sousou no Frieren S2 - 02 (720p) [BBBB1111].mkv", 2 * time.Hour, 50},                 // waits for 480p
		{"[Erai-raws] Sousou no Frieren 2nd Season - 03 [720p][Multiple Subtitle].mkv", 48 * time.Hour, 30}, // falls back
		{"[SubsPlease] Sousou no Frieren S2 - 04 (480p) [CCCC1111].mkv", 200 * 24 * time.Hour, 5},           // old upload
		{"[SubsPlease] Sousou no Frieren S2 - 05 (480p) [DDDD1111].mkv", 24 * time.Hour, 80},                // already stored
		{"[SubsPlease] Sousou no Frieren - 06 (480p) [EEEE1111].mkv", 24 * time.Hour, 80},                   // season 1
		{"[Group] Sousou no Frieren S2 (01-04) [480p] [Batch]", 24 * time.Hour, 80},
	})
	defer ny.Close()
	tr := &fakeTransmission{names: map[string]string{}}
	for i, n := range []string{"[SubsPlease] Sousou no Frieren S2 - 01 (480p) [AAAA1111].mkv", "", "", "[Erai-raws] Sousou no Frieren 2nd Season - 03 [720p][Multiple Subtitle].mkv"} {
		tr.names[fmt.Sprintf("%s/download/%d.torrent", ny.URL, i)] = n
	}
	trs := httptest.NewServer(tr)
	defer trs.Close()

	ask := &fakeAsk{items: map[string]approvals.Item{}}
	tk := &Tracker{State: &State{DB: db, Now: func() time.Time { return now }}, AniList: &AniList{URL: al.URL},
		Nyaa: &Nyaa{Base: ny.URL}, Client: &Transmission{Base: trs.URL, Label: "noskill-anime", SavePath: downloads},
		Ask: ask, Files: fakeFiles{trash}, Now: func() time.Time { return now },
		Settings: Settings{DefaultDest: dest, LibraryDirs: []string{other}, Lang: "romaji", Naming: Naming{TitleFolders: true}}}

	// Suggest: only new shows, not ones the user has, best first.
	rep, err := tk.Run(ctx, "suggest")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rep.Offered, "\n")
	for _, want := range []string{title, "Long Show", "Next Show"} {
		if !strings.Contains(got, want) {
			t.Errorf("not offered %s:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Ended Show", "Meh Show", "Owned Show"} {
		if strings.Contains(got, bad) {
			t.Errorf("offered %s:\n%s", bad, got)
		}
	}
	if !strings.Contains(strings.Join(rep.Skipped, "\n"), "Owned Show: already in your folders") {
		t.Errorf("owned show not reported: %v", rep.Skipped)
	}
	// A second run doesn't offer them again while they wait.
	if rep2, _ := tk.Run(ctx, "suggest"); len(rep2.Offered) != 0 {
		t.Errorf("offered twice: %v", rep2.Offered)
	}

	// The user approves Frieren (keeping the prefilled folder) and skips the rest.
	for id, it := range ask.items {
		if strings.Contains(it.Summary, title) {
			it.Status = approvals.Approved
			if it.Fields[0].Prefill != dest {
				t.Errorf("prefill %q", it.Fields[0].Prefill)
			}
		} else {
			it.Status = approvals.Skipped
		}
		ask.items[id] = it
	}
	rep, err = tk.Run(ctx, "answers")
	if err != nil || len(rep.Approved) != 1 || len(rep.Declined) != 2 {
		t.Fatalf("answers: %+v %v", rep, err)
	}
	if g, _ := tk.State.Granted(ctx); len(g) != 1 || g[0] != dest {
		t.Errorf("granted %v", g)
	}

	// Download: 01 in 480p, 03 falls back to 720p; 02 waits, 04 is old, 05 is stored, S1 and batches never.
	rep, err = tk.Run(ctx, "download")
	if err != nil {
		t.Fatal(err)
	}
	added := strings.Join(rep.Added, "\n")
	if len(rep.Added) != 2 || !strings.Contains(added, "1 (480p)") || !strings.Contains(added, "3 (720p)") {
		t.Fatalf("added:\n%s\nskipped: %v\nproblems: %v", added, rep.Skipped, rep.Problems)
	}
	if !strings.Contains(strings.Join(rep.Skipped, "\n"), "2: waiting for 480p") {
		t.Errorf("skipped: %v", rep.Skipped)
	}
	// Running again adds nothing twice.
	if rep, _ = tk.Run(ctx, "download"); len(rep.Added) != 0 {
		t.Errorf("added twice: %v", rep.Added)
	}

	// Both finish; one is the user's own torrent without the label, which is never touched.
	tr.mu.Lock()
	for _, x := range tr.torrents {
		name := x["name"].(string)
		os.WriteFile(filepath.Join(downloads, name), []byte(name), 0o644)
		x["status"], x["percentDone"], x["doneDate"] = 0, 1.0, now.Unix()
		x["files"] = []map[string]any{{"name": name, "length": len(name), "bytesCompleted": len(name)}}
	}
	tr.torrents = append(tr.torrents, map[string]any{"hashString": "f00d", "name": "[SubsPlease] Sousou no Frieren S2 - 09 (480p).mkv",
		"status": 0, "labels": []string{}, "downloadDir": downloads, "percentDone": 1.0})
	tr.mu.Unlock()

	rep, err = tk.Run(ctx, "file")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Filed) != 2 || len(rep.Problems) != 0 {
		t.Fatalf("file: %+v", rep)
	}
	for _, ep := range []string{"S02E01 [480p]", "S02E03 [720p]"} {
		if _, err := os.Stat(filepath.Join(dest, title, title+" "+ep+".mkv")); err != nil {
			t.Errorf("episode %s not filed: %v", ep, err)
		}
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 2 {
		t.Errorf("downloads not trashed: %d", len(ents))
	}
	if len(tr.removed) != 2 || len(tr.torrents) != 1 || tr.torrents[0]["hashString"] != "f00d" {
		t.Errorf("removed %v, left %v", tr.removed, tr.torrents)
	}
}

func TestNoTorrentAppIsReported(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tk := &Tracker{State: &State{DB: db}, Detected: []ClientInfo{{Name: "Transmission", Supported: true,
		Note: "Transmission's remote access is off."}}}
	rep, err := tk.Run(ctx, "download")
	if err != nil || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "remote access is off") {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestIsNew(t *testing.T) {
	for _, c := range []struct {
		m    Media
		want bool
	}{
		{Media{Season: "FALL", Year: 2026, Status: "RELEASING"}, true},
		{Media{Season: "WINTER", Year: 2027, Status: "NOT_YET_RELEASED"}, true},
		{Media{Season: "SUMMER", Year: 2026, Status: "RELEASING"}, true},
		{Media{Season: "SUMMER", Year: 2026, Status: "FINISHED"}, false},
		{Media{Season: "SPRING", Year: 2026, Status: "RELEASING"}, false}, // long-running: not new
		{Media{Season: "FALL", Year: 2019, Status: "FINISHED"}, false},
		{Media{Season: "SPRING", Year: 2027, Status: "NOT_YET_RELEASED"}, false},
	} {
		if got := IsNew(c.m, now); got != c.want {
			t.Errorf("%+v: %v", c.m, got)
		}
	}
}

// filingFixture: Frieren season 2 episode 1 is filed at 480p, and a finished download holds name.
func filingFixture(t *testing.T, name string) (*Tracker, *fakeTransmission, TitleState, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dest, downloads, trash := filepath.Join(dir, "Anime"), filepath.Join(dir, "Downloads"), filepath.Join(dir, "Trash")
	for _, d := range []string{dest, downloads, trash} {
		os.MkdirAll(d, 0o755)
	}
	const title = "Sousou no Frieren 2nd Season"
	oldPath := filepath.Join(dest, title, title+" S02E01 [480p].mkv")
	os.MkdirAll(filepath.Dir(oldPath), 0o755)
	os.WriteFile(oldPath, []byte("v1"), 0o644)
	os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour)) // as filed: older than the filing itself
	ts := TitleState{ID: 2, Title: title, Aliases: []string{title}, Season: 2, Status: Approved, Dest: dest, Resolution: "480p",
		Filed: map[string]FiledEpi{"1": {Path: oldPath, Resolution: "480p", At: now}}}
	st := &State{DB: db, Now: func() time.Time { return now }}
	if err := st.PutTitle(ctx, ts); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(downloads, name), []byte("v2 with fixes"), 0o644)
	tr := &fakeTransmission{torrents: []map[string]any{{"hashString": "cafe", "name": name, "status": 0, "labels": []string{"noskill-anime"},
		"downloadDir": downloads, "percentDone": 1.0, "doneDate": now.Unix(),
		"files": []map[string]any{{"name": name, "length": 13, "bytesCompleted": 13}}}}}
	srv := httptest.NewServer(tr)
	t.Cleanup(srv.Close)
	tk := &Tracker{State: st, Client: &Transmission{Base: srv.URL, Label: "noskill-anime", SavePath: downloads},
		Files: fakeFiles{trash}, Now: func() time.Time { return now },
		Settings: Settings{Naming: Naming{TitleFolders: true}, ReplaceVersions: true}}
	return tk, tr, ts, oldPath, trash
}

// SKILL.md "Rules": a fixed release (v2) of the same episode and resolution replaces the old
// file, and the old one goes to the trash first.
func TestFixedReleaseReplacesTheFiledEpisode(t *testing.T) {
	tk, tr, _, oldPath, trash := filingFixture(t, "[SubsPlease] Sousou no Frieren S2 - 01v2 (480p) [ABCD1234].mkv")
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Problems) != 0 || len(rep.Filed) != 1 || !strings.Contains(rep.Filed[0], "replaced the earlier release") {
		t.Fatalf("%+v %v", rep, err)
	}
	if b, _ := os.ReadFile(oldPath); string(b) != "v2 with fixes" {
		t.Errorf("filed episode is %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(trash, filepath.Base(oldPath))); string(b) != "v1" {
		t.Errorf("the old file isn't in the trash: %q", b)
	}
	ts, _ := tk.State.Titles(context.Background())
	if ts[0].Filed["1"].Version != 2 || len(tr.removed) != 1 {
		t.Errorf("filed %+v, removed %v", ts[0].Filed["1"], tr.removed)
	}

	// With replace_versions off, or a lower version, the filed episode stays.
	tk2, _, _, old2, trash2 := filingFixture(t, "[SubsPlease] Sousou no Frieren S2 - 01v2 (480p) [ABCD1234].mkv")
	tk2.Settings.ReplaceVersions = false
	rep, _ = tk2.Run(context.Background(), "file")
	if b, _ := os.ReadFile(old2); string(b) != "v1" || len(rep.Filed) != 0 {
		t.Errorf("replaced although replace_versions is off: %+v", rep)
	}
	// Only the finished download is cleaned up; the filed episode isn't touched.
	if _, err := os.Stat(filepath.Join(trash2, filepath.Base(old2))); err == nil {
		t.Errorf("the filed episode was trashed")
	}
}

// SKILL.md "Destinations on external drives": the copy needs the file's size plus 5% free.
func TestFilingStopsWhenTheDriveIsFull(t *testing.T) {
	tk, tr, _, oldPath, trash := filingFixture(t, "[SubsPlease] Sousou no Frieren S2 - 02 (480p) [ABCD1234].mkv")
	tk.Free = func(string) (uint64, error) { return 13, nil } // the file is 13 bytes: 13 free is short of the 5% reserve
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 0 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "not enough space") {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(tr.removed) != 0 || len(tr.torrents) != 1 {
		t.Errorf("the torrent must stay for the next run: removed %v", tr.removed)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 0 {
		t.Errorf("trashed %d files", len(ents))
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("episode 1 is gone: %v", err)
	}
	tk.Free = func(string) (uint64, error) { return 1 << 30, nil }
	if rep, _ = tk.Run(context.Background(), "file"); len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Errorf("with room: %+v", rep)
	}
}

// SKILL.md `mode = "notify-only"`: the suggestions go out as a notification, with no approval
// cards and nothing recorded as waiting.
func TestNotifyOnlySendsTheListWithoutApprovals(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	al := fakeAniList(t, map[string][]map[string]any{"FALL 2026": {media(2, "Frieren S2", "", "FALL", 2026, "RELEASING", 85, 200000)}})
	defer al.Close()
	ny := fakeNyaa(nil)
	defer ny.Close()
	ask := &fakeAsk{items: map[string]approvals.Item{}}
	var title, body string
	calls := 0
	tk := &Tracker{State: &State{DB: db, Now: func() time.Time { return now }}, AniList: &AniList{URL: al.URL}, Nyaa: &Nyaa{Base: ny.URL},
		Ask: ask, Now: func() time.Time { return now }, Settings: Settings{NotifyOnly: true, Lang: "romaji"},
		Notify: func(_ context.Context, t, b string) { calls++; title, body = t, b }}
	rep, err := tk.Run(ctx, "suggest")
	if err != nil || len(rep.Offered) != 1 || !strings.Contains(rep.Offered[0], "notify-only") {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(ask.items) != 0 {
		t.Errorf("approval cards were created: %v", ask.items)
	}
	if calls != 1 || !strings.Contains(body, "Frieren S2") || title == "" {
		t.Errorf("notification %d: %q %q", calls, title, body)
	}
	if ts, _ := tk.State.Titles(ctx); len(ts) != 0 {
		t.Errorf("recorded as waiting: %+v", ts)
	}
}
