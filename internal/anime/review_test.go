package anime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/store"
)

// docs/REVIEW_BLOCK3.md: the file stage, failure point by failure point.

// flakyFiles is fakeFiles with failures on demand.
type flakyFiles struct {
	fakeFiles
	copyErr, trashErr *error
	trashed           *[]string
}

func (f flakyFiles) Copy(ctx context.Context, from, to string) error {
	if f.copyErr != nil && *f.copyErr != nil {
		return *f.copyErr
	}
	return f.fakeFiles.Copy(ctx, from, to)
}
func (f flakyFiles) TrashPath(ctx context.Context, p string) error {
	if f.trashErr != nil && *f.trashErr != nil {
		return *f.trashErr
	}
	if f.trashed != nil {
		*f.trashed = append(*f.trashed, p)
	}
	return f.fakeFiles.TrashPath(ctx, p)
}

// contentClient reports every torrent's content as one fixed path, and can fail removal.
type contentClient struct {
	Downloader
	content   string
	removeErr *error
}

func (c contentClient) Completed(ctx context.Context) ([]Torrent, error) {
	ts, err := c.Downloader.Completed(ctx)
	for i := range ts {
		if c.content != "" {
			ts[i].ContentPath = c.content
		}
	}
	return ts, err
}
func (c contentClient) RemoveTorrent(ctx context.Context, hash string) error {
	if c.removeErr != nil && *c.removeErr != nil {
		return *c.removeErr
	}
	return c.Downloader.RemoveTorrent(ctx, hash)
}

const ep2 = "[SubsPlease] Sousou no Frieren S2 - 02 (480p) [ABCD1234].mkv"

// A torrent made without a folder reports the download folder itself as its content. The folder
// must never be trashed; only the video that was placed is.
func TestFolderlessTorrentNeverTrashesTheDownloadFolder(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	downloads := tk.Client.(*Transmission).SavePath
	other := filepath.Join(downloads, "my-other-download.zip")
	os.WriteFile(other, []byte("not noskill's"), 0o644)
	var trashed []string
	tk.Files = flakyFiles{fakeFiles: tk.Files.(fakeFiles), trashed: &trashed}
	tk.Client = contentClient{Downloader: tk.Client, content: downloads}

	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, p := range trashed {
		if p == downloads || strings.HasPrefix(downloads, p) {
			t.Fatalf("trashed %s", p)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another file in the download folder is gone: %v", err)
	}
	if len(trashed) != 1 || filepath.Base(trashed[0]) != "Sousou no Frieren 2nd Season S02E02 [480p].mkv" || len(tr.removed) != 1 {
		t.Errorf("trashed %v, removed %v", trashed, tr.removed)
	}
}

// The user re-encoded or remuxed the filed episode: a fixed release must not replace their file.
func TestEditedFileIsNotReplaced(t *testing.T) {
	tk, _, _, oldPath, _ := filingFixture(t, "[SubsPlease] Sousou no Frieren S2 - 01v2 (480p) [ABCD1234].mkv")
	os.Chtimes(oldPath, now.Add(3*time.Hour), now.Add(3*time.Hour))
	var trashed []string
	tk.Files = flakyFiles{fakeFiles: tk.Files.(fakeFiles), trashed: &trashed}
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if b, _ := os.ReadFile(oldPath); string(b) != "v1" {
		t.Errorf("the user's file was replaced: %q", b)
	}
	for _, p := range trashed {
		if p == oldPath {
			t.Error("the user's file was trashed")
		}
	}
	if len(rep.Skipped) == 0 || !strings.Contains(strings.Join(rep.Skipped, "\n"), "changed after noskill filed it") {
		t.Errorf("skipped: %v", rep.Skipped)
	}
}

func TestCopyFailureKeepsTheDownloadAndTheTorrent(t *testing.T) {
	tk, tr, _, _, trash := filingFixture(t, ep2)
	boom := errors.New("drive went away")
	tk.Files = flakyFiles{fakeFiles: tk.Files.(fakeFiles), copyErr: &boom}
	rep, _ := tk.Run(context.Background(), "file")
	if len(rep.Filed) != 0 || len(rep.Problems) != 1 || len(tr.removed) != 0 {
		t.Fatalf("%+v removed %v", rep, tr.removed)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 0 {
		t.Errorf("trashed %d files", len(ents))
	}
	boom = nil // the drive is back: the next run files it
	if rep, _ = tk.Run(context.Background(), "file"); len(rep.Filed) != 1 || len(rep.Problems) != 0 || len(tr.removed) != 1 {
		t.Errorf("retry: %+v removed %v", rep, tr.removed)
	}
}

// The episode is stored and the torrent removed, but the download couldn't be trashed: the file
// stays where it is, once, and the problem is reported. Nothing is lost and nothing is filed twice.
func TestTrashFailureAfterFilingLosesNothing(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	boom := errors.New("no trash on this drive")
	tk.Files = flakyFiles{fakeFiles: tk.Files.(fakeFiles), trashErr: &boom}
	rep, _ := tk.Run(context.Background(), "file")
	if len(rep.Filed) != 1 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "couldn't move the download to the trash") || len(tr.removed) != 1 {
		t.Fatalf("%+v removed %v", rep, tr.removed)
	}
	if _, err := os.Stat(filepath.Join(tk.Client.(*Transmission).SavePath, "Sousou no Frieren 2nd Season S02E02 [480p].mkv")); err != nil {
		t.Errorf("the download vanished: %v", err)
	}
	ts, _ := tk.State.Titles(context.Background())
	if ts[0].Filed["2"].Path == "" {
		t.Errorf("not recorded as filed: %+v", ts[0].Filed)
	}
}

// The torrent app didn't take the removal: the download is left alone, and the next run (which
// sees the episode already stored) finishes the clean-up without copying again.
func TestTorrentRemovalFailureIsRetriedWithoutRefiling(t *testing.T) {
	tk, tr, _, _, trash := filingFixture(t, ep2)
	boom := errors.New("app busy")
	tk.Client = contentClient{Downloader: tk.Client, removeErr: &boom}
	var copies int
	tk.Files = countingCopies{flakyFiles{fakeFiles: tk.Files.(fakeFiles)}, &copies}
	rep, _ := tk.Run(context.Background(), "file")
	if len(rep.Filed) != 1 || len(rep.Problems) != 1 || copies != 1 {
		t.Fatalf("%+v copies %d", rep, copies)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 0 {
		t.Errorf("trashed before the torrent was removed: %d", len(ents))
	}
	boom = nil
	rep, _ = tk.Run(context.Background(), "file")
	if copies != 1 || len(tr.removed) != 1 || len(rep.Problems) != 0 {
		t.Errorf("retry: %+v copies %d removed %v", rep, copies, tr.removed)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 1 {
		t.Errorf("the finished download should be in the trash once: %d", len(ents))
	}
}

type countingCopies struct {
	flakyFiles
	n *int
}

func (c countingCopies) Copy(ctx context.Context, from, to string) error {
	*c.n++
	return c.flakyFiles.Copy(ctx, from, to)
}

// v2 replacement: the old file is trashed first. If the copy then fails, the old file is in the
// trash (and the report says so), the torrent stays, and the next run files the fixed release.
func TestReplacementCopyFailureIsRetriedAndOldIsInTheTrash(t *testing.T) {
	tk, tr, _, oldPath, trash := filingFixture(t, "[SubsPlease] Sousou no Frieren S2 - 01v2 (480p) [ABCD1234].mkv")
	boom := errors.New("disk full")
	tk.Files = flakyFiles{fakeFiles: tk.Files.(fakeFiles), copyErr: &boom}
	rep, _ := tk.Run(context.Background(), "file")
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "is in the trash") || len(tr.removed) != 0 {
		t.Fatalf("%+v removed %v", rep, tr.removed)
	}
	if b, _ := os.ReadFile(filepath.Join(trash, filepath.Base(oldPath))); string(b) != "v1" {
		t.Errorf("old file is not in the trash: %q", b)
	}
	boom = nil
	rep, _ = tk.Run(context.Background(), "file")
	if len(rep.Problems) != 0 || len(rep.Filed) != 1 {
		t.Fatalf("retry: %+v", rep)
	}
	if b, _ := os.ReadFile(oldPath); string(b) != "v2 with fixes" {
		t.Errorf("filed episode is %q", b)
	}
}

// A share that stops answering is left out of the library scan and named, instead of stalling the run.
func TestHungFolderIsSkippedByTheLibraryScan(t *testing.T) {
	good := t.TempDir()
	os.WriteFile(filepath.Join(good, "[Grp] Some Show - 01 (480p).mkv"), []byte("x"), 0o644)
	hung := t.TempDir()
	release := make(chan struct{})
	defer close(release)
	oldWalk, oldTimeout := walkDir, ScanTimeout
	defer func() { walkDir, ScanTimeout = oldWalk, oldTimeout }()
	ScanTimeout = 100 * time.Millisecond
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		if root == filepath.Clean(hung) {
			<-release
			return nil
		}
		return oldWalk(root, fn)
	}
	start := time.Now()
	lib := ScanLibrary([]string{hung, good}, 0)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the scan waited for the hung folder")
	}
	if len(lib.Files) != 1 || len(lib.Slow) != 1 || lib.Slow[0] != filepath.Clean(hung) {
		t.Errorf("files %d, slow %v", len(lib.Files), lib.Slow)
	}
}

// recordFiles notes the order of file operations.
type recordFiles struct {
	flakyFiles
	ops *[]string
}

func (r recordFiles) Rename(ctx context.Context, from, to string) error {
	*r.ops = append(*r.ops, "rename "+filepath.Base(from)+" -> "+filepath.Base(to))
	return r.flakyFiles.Rename(ctx, from, to)
}
func (r recordFiles) Copy(ctx context.Context, from, to string) error {
	*r.ops = append(*r.ops, "copy "+filepath.Base(from))
	return r.flakyFiles.Copy(ctx, from, to)
}

// The file gets its final name in the download folder first, and the copy is of the renamed file.
func TestFileIsRenamedBeforeItIsMoved(t *testing.T) {
	tk, _, _, _, _ := filingFixture(t, ep2)
	var ops []string
	tk.Files = recordFiles{flakyFiles{fakeFiles: tk.Files.(fakeFiles)}, &ops}
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	const planned = "Sousou no Frieren 2nd Season S02E02 [480p].mkv"
	if len(ops) != 2 || ops[0] != "rename "+ep2+" -> "+planned || ops[1] != "copy "+planned {
		t.Errorf("operations: %v", ops)
	}
}

// If the rename can't happen (the name is taken in the download folder), the copy still names the file.
func TestRenameFailureStillFilesUnderTheRightName(t *testing.T) {
	tk, _, _, _, _ := filingFixture(t, ep2)
	downloads := tk.Client.(*Transmission).SavePath
	os.WriteFile(filepath.Join(downloads, "Sousou no Frieren 2nd Season S02E02 [480p].mkv"), []byte("someone else's"), 0o644)
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if !strings.HasSuffix(rep.Filed[0], "Sousou no Frieren 2nd Season S02E02 [480p].mkv") {
		t.Errorf("filed %q", rep.Filed[0])
	}
}

// A finished torrent is stopped from seeding and filed at once, even with a share limit still to
// reach, which used to make filing wait.
func TestFinishedTorrentIsStoppedFromSeedingAndFiledAtOnce(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	tr.torrents[0]["status"] = 6
	tr.torrents[0]["seedRatioMode"] = 1
	tr.torrents[0]["seedRatioLimit"] = 2.0
	tr.torrents[0]["uploadRatio"] = 0.1
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(rep.Stopped) != 1 || len(tr.stopped) != 1 || tr.stopped[0] != "cafe" || len(tr.removed) != 1 {
		t.Errorf("stopped %v / %v, removed %v", rep.Stopped, tr.stopped, tr.removed)
	}
	if !strings.Contains(rep.String(), "Stopped seeding:") {
		t.Errorf("report:\n%s", rep)
	}
}

func TestDryRunReportsButDoesNotStopSeeding(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	tr.torrents[0]["status"] = 6
	tk.DryRun = true
	rep, _ := tk.Run(context.Background(), "file")
	if len(tr.stopped) != 0 || len(rep.Stopped) != 1 || !strings.Contains(rep.Stopped[0], "dry run") {
		t.Errorf("stopped %v, report %v", tr.stopped, rep.Stopped)
	}
}

// qBittorrent 5 has "stop", 4 only "pause"; and only noskill's own category may be stopped.
func TestQbitStopFallsBackToPauseAndRefusesOthers(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, strings.TrimPrefix(r.URL.Path, "/api/v2/"))
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			cat := "noskill-anime"
			if r.URL.Query().Get("hashes") == "beef" {
				cat = "mine"
			}
			fmt.Fprintf(w, `[{"hash":%q,"name":"x","category":%q,"progress":1}]`, r.URL.Query().Get("hashes"), cat)
		case "/api/v2/torrents/stop":
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, "Ok.")
		}
	}))
	defer srv.Close()
	q := &Qbit{Base: srv.URL, Category: "noskill-anime"}
	if err := q.Stop(context.Background(), "cafe"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "torrents/info,torrents/stop,torrents/pause" {
		t.Errorf("calls: %s", got)
	}
	calls = nil
	if err := q.Stop(context.Background(), "beef"); !errors.Is(err, ErrNotOurs) {
		t.Errorf("a torrent that isn't noskill's: %v", err)
	}
	if strings.Contains(strings.Join(calls, ","), "pause") || strings.Contains(strings.Join(calls, ","), "stop") {
		t.Errorf("stopped someone else's torrent: %v", calls)
	}
}

// The download folder is scanned for shows the user already has, and the episode being filed sits in
// it. That must not count as "already have it": the file has to be filed, not trashed with the torrent.
func TestFileInDownloadsIsNotMistakenForTheLibraryCopy(t *testing.T) {
	tk, tr, _, _, trash := filingFixture(t, ep2)
	downloads := tk.Client.(*Transmission).SavePath
	// The scan sees the finished download, as the real one does ($DOWNLOADS is in its folders).
	tk.Settings.LibraryDirs = []string{downloads}
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	ts, _ := tk.State.Titles(context.Background())
	got := ts[0].Filed["2"].Path
	if b, _ := os.ReadFile(got); string(b) != "v2 with fixes" {
		t.Errorf("episode 2 isn't in the library at %q: %q", got, b)
	}
	if len(tr.removed) != 1 {
		t.Errorf("removed %v", tr.removed)
	}
	if ents, _ := os.ReadDir(trash); len(ents) != 1 {
		t.Errorf("only the finished download should be in the trash: %d", len(ents))
	}
}

type timeoutAsk struct {
	*fakeAsk
	timeouts []time.Duration
}

func (a *timeoutAsk) Create(ctx context.Context, n approvals.New) (approvals.Item, error) {
	a.timeouts = append(a.timeouts, n.Timeout)
	return a.fakeAsk.Create(ctx, n)
}

// A title's approval card stays open to the end of the season, not for a day.
func TestApprovalCardLastsTheSeason(t *testing.T) {
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
	ask := &timeoutAsk{fakeAsk: &fakeAsk{items: map[string]approvals.Item{}}}
	tk := &Tracker{State: &State{DB: db, Now: func() time.Time { return now }}, AniList: &AniList{URL: al.URL}, Nyaa: &Nyaa{Base: ny.URL},
		Ask: ask, Now: func() time.Time { return now }, Settings: Settings{Lang: "romaji"}}
	if rep, err := tk.Run(ctx, "suggest"); err != nil || len(rep.Offered) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC).Sub(now)
	if len(ask.timeouts) != 1 || ask.timeouts[0] != want {
		t.Errorf("timeout %v, want %v (to 31 December)", ask.timeouts, want)
	}
}

// aniListWithIDs answers by-id lookups (earlier seasons) from a map.
func aniListWithIDs(t *testing.T, byID map[int]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		out := []map[string]any{}
		for _, v := range req.Variables["ids"].([]any) {
			if m, ok := byID[int(v.(float64))]; ok {
				out = append(out, m)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"Page": map[string]any{"media": out}}})
	}))
}

func batchFixture(t *testing.T, items []rss) (*Tracker, *timeoutAsk, *fakeTransmission, *httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s1 := media(1, "Sousou no Frieren", "", "FALL", 2023, "FINISHED", 90, 300000)
	s1["episodes"] = 28
	s2 := media(2, "Sousou no Frieren 2nd Season", "", "FALL", 2026, "RELEASING", 85, 200000)
	s2["relations"] = map[string]any{"edges": []map[string]any{{"relationType": "PREQUEL", "node": map[string]any{"id": 1}}}}
	al := aniListWithIDs(t, map[int]map[string]any{1: s1, 2: s2})
	t.Cleanup(al.Close)
	ny := fakeNyaa(items)
	t.Cleanup(ny.Close)
	tr := &fakeTransmission{names: map[string]string{}}
	for i, it := range items {
		tr.names[fmt.Sprintf("%s/download/%d.torrent", ny.URL, i)] = it.title
	}
	trs := httptest.NewServer(tr)
	t.Cleanup(trs.Close)
	dest := filepath.Join(dir, "Anime")
	os.MkdirAll(dest, 0o755)
	st := &State{DB: db, Now: func() time.Time { return now }}
	if err := st.PutTitle(ctx, TitleState{ID: 2, Title: "Sousou no Frieren 2nd Season", Aliases: []string{"Sousou no Frieren 2nd Season"}, Season: 2,
		Status: Approved, Dest: dest, Resolution: "480p"}); err != nil {
		t.Fatal(err)
	}
	ask := &timeoutAsk{fakeAsk: &fakeAsk{items: map[string]approvals.Item{}}}
	tk := &Tracker{State: st, AniList: &AniList{URL: al.URL}, Nyaa: &Nyaa{Base: ny.URL}, Ask: ask, Now: func() time.Time { return now },
		Client: &Transmission{Base: trs.URL, Label: "noskill-anime", SavePath: filepath.Join(dir, "Downloads")}, Files: fakeFiles{filepath.Join(dir, "Trash")},
		Settings: Settings{Lang: "romaji"}}
	return tk, ask, tr, ny, dest
}

// A complete 480p batch of an earlier season of a followed show gets an approval card, once, with the
// season-long expiry and the later season's folder prefilled; nothing downloads until it is approved.
func TestEarlierSeasonBatchIsOfferedThenDownloadedAfterApproval(t *testing.T) {
	ctx := context.Background()
	tk, ask, tr, _, dest := batchFixture(t, []rss{
		{"[Group] Sousou no Frieren (01-28) [480p] [Batch]", 24 * time.Hour, 50},
		{"[Group] Sousou no Frieren (01-14) [480p] [Batch]", 24 * time.Hour, 90},      // half a season
		{"[Group] Sousou no Frieren (01-28) [1080p] [Batch]", 24 * time.Hour, 200},    // not 480p
		{"[Group] Sousou no Frieren (01-28) [480p] [Batch]", 90 * 24 * time.Hour, 70}, // old upload
	})
	rep, err := tk.Run(ctx, "batches")
	if err != nil || len(rep.Offered) != 1 || len(rep.Problems) != 0 || !strings.Contains(rep.Offered[0], "complete season: Sousou no Frieren") {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(ask.items) != 1 || ask.items["a1"].Fields[0].Prefill != dest {
		t.Fatalf("cards %+v", ask.items)
	}
	if want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC).Sub(now); ask.timeouts[0] != want {
		t.Errorf("expiry %v", ask.timeouts[0])
	}
	if len(tr.torrents) != 0 {
		t.Fatal("downloaded before approval")
	}
	// A second run doesn't ask again, and still downloads nothing.
	if rep, _ = tk.Run(ctx, "batches"); len(rep.Offered) != 0 || len(ask.items) != 1 {
		t.Errorf("asked twice: %+v", rep)
	}
	// Approved: the next run adds the batch, once.
	it := ask.items["a1"]
	it.Status = approvals.Approved
	ask.items["a1"] = it
	if rep, _ = tk.Run(ctx, "answers"); len(rep.Approved) != 1 {
		t.Fatalf("answers: %+v", rep)
	}
	rep, _ = tk.Run(ctx, "download")
	if len(rep.Problems) != 0 || len(tr.torrents) != 1 || !strings.Contains(strings.Join(rep.Added, " "), "complete season") {
		t.Fatalf("download: %+v torrents %d", rep, len(tr.torrents))
	}
	if rep, _ = tk.Run(ctx, "download"); len(tr.torrents) != 1 {
		t.Errorf("added twice: %+v", rep)
	}
}

// If the user already has that show, no card is sent.
func TestNoBatchOfferForAShowTheUserHas(t *testing.T) {
	tk, ask, _, _, _ := batchFixture(t, []rss{{"[Group] Sousou no Frieren (01-28) [480p] [Batch]", 24 * time.Hour, 50}})
	lib := t.TempDir()
	os.WriteFile(filepath.Join(lib, "[Group] Sousou no Frieren - 03 (480p).mkv"), []byte("x"), 0o644)
	tk.Settings.LibraryDirs = []string{lib}
	tk.Settings.Lang = "romaji"
	if rep, _ := tk.Run(context.Background(), "batches"); len(rep.Offered) != 0 || len(ask.items) != 0 {
		t.Errorf("offered although the show is in the library: %+v", rep)
	}
}

// The torrent app's "download finished" hook files that one torrent (FR-J1 hook, SKILL.md D).
func TestHookFilesOnlyTheTorrentItNames(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	tk.Only = "CAFE" // hashes compare without case
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 1 || len(rep.Problems) != 0 || len(tr.removed) != 1 {
		t.Fatalf("%+v %v removed %v", rep, err, tr.removed)
	}
}

func TestHookLeavesOtherTorrentsAlone(t *testing.T) {
	tk, tr, _, _, _ := filingFixture(t, ep2)
	tk.Only = "beef"
	rep, err := tk.Run(context.Background(), "file")
	if err != nil || len(rep.Filed) != 0 || len(tr.removed) != 0 || len(tr.stopped) != 0 {
		t.Fatalf("%+v %v removed %v stopped %v", rep, err, tr.removed, tr.stopped)
	}
	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "beef") {
		t.Errorf("skipped %v", rep.Skipped)
	}
}

// While another run is filing, the report says so, and says it with FilingBusy: the hook waits on it.
func TestFilingBusyIsReported(t *testing.T) {
	tk, _, _, _, _ := filingFixture(t, ep2)
	ok, err := tk.State.Lock(context.Background(), "someone-else", time.Hour)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	rep, _ := tk.Run(context.Background(), "file")
	if len(rep.Skipped) != 1 || rep.Skipped[0] != FilingBusy || len(rep.Filed) != 0 {
		t.Errorf("%+v", rep)
	}
}
