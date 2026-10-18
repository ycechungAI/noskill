package anime

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/store"
)

type planRig struct {
	tk                        *Tracker
	tr                        *fakeTransmission
	ask                       *fakeAsk
	dest, downloads, trashDir string
	srvs                      []*httptest.Server
}

func newPlanRig(t *testing.T, askFirst bool) *planRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := &planRig{dest: filepath.Join(dir, "Anime"), downloads: filepath.Join(dir, "Downloads"), trashDir: filepath.Join(dir, "Trash")}
	for _, d := range []string{r.dest, r.downloads, r.trashDir} {
		os.MkdirAll(d, 0o755)
	}
	ny := fakeNyaa([]rss{{"[SubsPlease] Test Show - 01 (480p) [AAAA1111].mkv", 2 * time.Hour, 100}})
	r.tr = &fakeTransmission{names: map[string]string{}}
	r.tr.names[fmt.Sprintf("%s/download/0.torrent", ny.URL)] = "[SubsPlease] Test Show - 01 (480p) [AAAA1111].mkv"
	trs := httptest.NewServer(r.tr)
	r.srvs = []*httptest.Server{ny, trs}
	t.Cleanup(func() {
		for _, s := range r.srvs {
			s.Close()
		}
	})
	r.ask = &fakeAsk{items: map[string]approvals.Item{}}
	r.tk = &Tracker{State: &State{DB: db, Now: func() time.Time { return now }}, Nyaa: &Nyaa{Base: ny.URL},
		Client: &Transmission{Base: trs.URL, Label: "noskill-anime", SavePath: r.downloads}, Ask: r.ask, Files: fakeFiles{r.trashDir},
		Now:         func() time.Time { return now },
		Settings:    Settings{DefaultDest: r.dest, AskFirst: askFirst, Naming: Naming{TitleFolders: true}},
		ResolveDest: func(p string) (string, error) { return p, nil }}
	if err := r.tk.State.PutTitle(ctx, TitleState{ID: 1, Title: "Test Show", Aliases: []string{"Test Show"}, Status: Approved, Dest: r.dest, Resolution: "480p"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// Added torrents stop seeding the moment they finish: a zero ratio limit for that torrent.
func TestAddedTorrentStopsAtCompletion(t *testing.T) {
	r := newPlanRig(t, false)
	if rep, err := r.tk.Run(context.Background(), "download"); err != nil || len(rep.Added) != 1 {
		t.Fatalf("%v %+v skipped %v problems %v", err, rep, rep.Skipped, rep.Problems)
	}
	var found bool
	for _, a := range r.tr.sets {
		if a["seedRatioMode"] == float64(1) && a["seedRatioLimit"] == float64(0) {
			found = true
		}
	}
	if !found {
		t.Errorf("no zero ratio limit was set: %v", r.tr.sets)
	}
}

// With ask_before_download a new release becomes a question; nothing is downloaded until the user
// says yes, a yes adds it once, and a no is remembered.
func TestAskBeforeDownload(t *testing.T) {
	ctx := context.Background()
	r := newPlanRig(t, true)

	rep, err := r.tk.Run(ctx, "download")
	if err != nil || len(rep.Planned) != 1 || len(rep.Added) != 0 || len(r.tr.torrents) != 0 {
		t.Fatalf("first run: %v %+v torrents %d", err, rep, len(r.tr.torrents))
	}
	if len(r.ask.items) != 1 || r.ask.items["a1"].Kind != "download" || !strings.Contains(r.ask.items["a1"].Summary, "Test Show") {
		t.Fatalf("approvals: %+v", r.ask.items)
	}
	// Asking again doesn't make another question.
	if rep, _ = r.tk.Run(ctx, "download"); len(r.ask.items) != 1 || len(rep.Added) != 0 || !strings.Contains(strings.Join(rep.Planned, " "), "waiting for you") {
		t.Fatalf("second run: %+v approvals %d", rep, len(r.ask.items))
	}
	// A dry run changes nothing and says it would ask.
	r.tk.DryRun = true
	if rep, _ = r.tk.Run(ctx, "download"); len(rep.Added) != 0 {
		t.Fatalf("dry run added: %+v", rep)
	}
	r.tk.DryRun = false

	// Yes: it's added, once.
	it := r.ask.items["a1"]
	it.Status = approvals.Approved
	r.ask.items["a1"] = it
	rep, err = r.tk.Run(ctx, "download")
	if err != nil || len(rep.Added) != 1 || len(r.tr.torrents) != 1 {
		t.Fatalf("after yes: %v %+v torrents %d", err, rep, len(r.tr.torrents))
	}
	if p, ok, _ := r.tk.State.PlanByKey(ctx, ReleaseKey("Test Show", "1")); !ok || p.Status != PlanAdded {
		t.Errorf("plan: %+v %v", p, ok)
	}
	if rep, _ = r.tk.Run(ctx, "download"); len(rep.Added) != 0 || len(r.tr.torrents) != 1 || len(r.ask.items) != 1 {
		t.Errorf("a third run did something: %+v", rep)
	}

	// No: never asked again, never added.
	r2 := newPlanRig(t, true)
	r2.tk.Run(ctx, "download")
	it = r2.ask.items["a1"]
	it.Status = approvals.Skipped
	r2.ask.items["a1"] = it
	rep, _ = r2.tk.Run(ctx, "download")
	if len(rep.Declined) != 1 || len(r2.tr.torrents) != 0 {
		t.Fatalf("after no: %+v", rep)
	}
	if rep, _ = r2.tk.Run(ctx, "download"); len(rep.Planned) != 0 || len(r2.ask.items) != 1 || len(r2.tr.torrents) != 0 {
		t.Errorf("a no wasn't remembered: %+v", rep)
	}

	// An unanswered question that expired is asked again.
	r3 := newPlanRig(t, true)
	r3.tk.Run(ctx, "download")
	it = r3.ask.items["a1"]
	it.Status = approvals.Expired
	r3.ask.items["a1"] = it
	r3.tk.Run(ctx, "download")
	if len(r3.ask.items) != 2 {
		t.Errorf("an expired question wasn't asked again: %d approvals", len(r3.ask.items))
	}
}

const btih = "0123456789abcdef0123456789abcdef01234567"

// A download the user adds by hand goes to the torrent app, and when it finishes every file goes to
// the folder they chose under the name they gave; it never leaves that folder.
func TestManualDownload(t *testing.T) {
	ctx := context.Background()
	r := newPlanRig(t, false)
	folder := filepath.Join(filepath.Dir(r.dest), "Mine")

	for _, bad := range []string{"", "file:///etc/passwd", "http://x.test/a.torrent", "https://x.test/page", "magnet:?dn=nohash", "javascript:alert(1)"} {
		if _, err := r.tk.AddManual(ctx, bad, folder, ""); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	item, err := r.tk.AddManual(ctx, "magnet:?xt=urn:btih:"+btih+"&dn=Some+Thing", folder, "My Thing")
	if err != nil {
		t.Fatal(err)
	}
	if item.Hash != btih || item.Status != PlanAdded || item.Name != "My Thing" || item.Dest != folder || len(r.tr.torrents) != 1 {
		t.Fatalf("%+v torrents %d", item, len(r.tr.torrents))
	}
	if g, _ := r.tk.State.Granted(ctx); len(g) != 1 || g[0] != folder {
		t.Errorf("the folder wasn't granted: %v", g)
	}
	if _, err := r.tk.AddManual(ctx, "magnet:?xt=urn:btih:"+btih, folder, ""); err == nil {
		t.Error("the same download was added twice")
	}

	// It finishes: a single file, under the name given.
	r.tr.mu.Lock()
	x := r.tr.torrents[0]
	x["hashString"] = btih
	os.WriteFile(filepath.Join(r.downloads, "some.thing.bin"), []byte("data"), 0o644)
	x["status"], x["percentDone"], x["doneDate"] = 0, 1.0, now.Unix()
	x["downloadDir"] = r.downloads
	x["name"] = "some.thing.bin"
	x["files"] = []map[string]any{{"name": "some.thing.bin", "length": 4, "bytesCompleted": 4}}
	r.tr.mu.Unlock()
	rep, err := r.tk.Run(ctx, "file")
	if err != nil || len(rep.Problems) != 0 || len(rep.Filed) != 1 {
		t.Fatalf("file: %v %+v", err, rep)
	}
	if b, err := os.ReadFile(filepath.Join(folder, "My Thing.bin")); err != nil || string(b) != "data" {
		t.Errorf("not filed: %v %q", err, b)
	}
	if len(r.tr.removed) != 1 || len(r.tr.torrents) != 0 {
		t.Errorf("torrent not removed: %v %d", r.tr.removed, len(r.tr.torrents))
	}
	if ents, _ := os.ReadDir(r.trashDir); len(ents) != 1 {
		t.Errorf("the download wasn't trashed: %d", len(ents))
	}
	if p, ok, _ := r.tk.State.PlanByKey(ctx, item.Key); !ok || p.Status != PlanFiled || !strings.HasSuffix(p.Path, "My Thing.bin") {
		t.Errorf("plan: %+v", p)
	}

	// A torrent of several files keeps its own folder and names, and a path that climbs out of the folder isn't filed.
	item2, err := r.tk.AddManual(ctx, "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20)+"&dn=Pack", folder, "")
	if err != nil {
		t.Fatal(err)
	}
	r.tr.mu.Lock()
	y := r.tr.torrents[0]
	y["hashString"] = item2.Hash
	os.MkdirAll(filepath.Join(r.downloads, "Pack"), 0o755)
	os.WriteFile(filepath.Join(r.downloads, "Pack", "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(r.downloads, "evil.txt"), []byte("e"), 0o644)
	y["status"], y["percentDone"], y["doneDate"], y["name"], y["downloadDir"] = 0, 1.0, now.Unix(), "Pack", r.downloads
	y["files"] = []map[string]any{{"name": "Pack/a.txt", "length": 1, "bytesCompleted": 1}, {"name": "../evil.txt", "length": 1, "bytesCompleted": 1}}
	r.tr.mu.Unlock()
	rep, _ = r.tk.Run(ctx, "file")
	if _, err := os.Stat(filepath.Join(folder, "Pack", "a.txt")); err != nil {
		t.Errorf("Pack/a.txt wasn't filed: %v %+v", err, rep)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(folder), "evil.txt")); err == nil {
		t.Error("a file left the folder")
	}
	if !strings.Contains(strings.Join(rep.Problems, " "), "would leave the folder") {
		t.Errorf("no problem reported: %+v", rep)
	}
	if len(r.tr.torrents) != 1 {
		t.Errorf("a torrent with an unfiled file was removed: %d", len(r.tr.torrents))
	}
	_ = item2
}
