package anime

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---- only new shows ----

var seasonIndex = map[string]int{"WINTER": 0, "SPRING": 1, "SUMMER": 2, "FALL": 3}

// IsNew says whether a show is new enough to suggest: it airs this season or next, or it started
// last season and is still airing (a two-cour show). Finished and older shows are never suggested,
// and neither are shows with no season on AniList.
func IsNew(m Media, now time.Time) bool {
	if m.Status == "FINISHED" || m.Status == "CANCELLED" || m.Year == 0 {
		return false
	}
	i, ok := seasonIndex[m.Season]
	if !ok {
		return false
	}
	cur, y, _, _ := SeasonOf(now)
	diff := (m.Year-y)*4 + i - seasonIndex[cur] // seasons from now
	switch {
	case diff == 0 || diff == 1:
		return true
	case diff == -1:
		return m.Status == "RELEASING"
	}
	return false
}

// FreshItems keeps feed items published within maxAge that are single episodes or specials:
// batches are old seasons repackaged, and old uploads are re-seeds of episodes already out.
func FreshItems(items []NyaaItem, maxAge time.Duration, now time.Time) []NyaaItem {
	var out []NyaaItem
	for _, it := range items {
		if it.Remake || it.Published.IsZero() || now.Sub(it.Published) > maxAge {
			continue
		}
		switch ParseRelease(strings.Split(it.Title, " | ")[0]).Kind {
		case "episode", "special":
			out = append(out, it)
		}
	}
	return out
}

// ---- not already stored ----

// Library is what the user already has: video files in their anime folders.
type Library struct {
	Files []Owned
	Slow  []string // folders that didn't answer in time and were left out
}

// Owned is one video file the user has.
type Owned struct {
	Path    string  `json:"path"`
	Release Release `json:"release"`
}

// skipDirs are system and app folders that never hold the user's shows.
var skipDirs = map[string]bool{"$recycle.bin": true, "system volume information": true, "node_modules": true,
	"library": true, "applications": true, "photos library.photoslibrary": true, "@eadir": true, "lost+found": true, "backups.backupdb": true, ".timemachine": true}

// ScanTimeout is how long one folder may take to scan. A network share that stopped answering
// blocks its reads for minutes or for ever; it is left out instead of stalling the whole run.
var ScanTimeout = 45 * time.Second

// walkDir is filepath.WalkDir; tests swap it for one that hangs.
var walkDir = filepath.WalkDir

// ScanLibrary lists video files under dirs, a few levels deep (Title/Season 01/file.mkv), skipping
// hidden folders. A missing folder (an unplugged drive) is skipped, not an error; one that doesn't
// answer within ScanTimeout is skipped and named in Library.Slow.
func ScanLibrary(dirs []string, limit int) Library {
	if limit <= 0 {
		limit = 50000
	}
	var lib Library
	for _, root := range dirs {
		root = filepath.Clean(root)
		room := limit - len(lib.Files)
		if room <= 0 {
			break
		}
		var stop atomic.Bool
		done := make(chan []Owned, 1)
		walk := walkDir // read here: a walk that never returns must not touch shared state later
		go func() { done <- scanRoot(root, room, &stop, walk) }()
		select {
		case files := <-done:
			lib.Files = append(lib.Files, files...)
		case <-time.After(ScanTimeout):
			stop.Store(true) // the walk stops at its next step, if it ever gets one
			lib.Slow = append(lib.Slow, root)
		}
	}
	return lib
}

func scanRoot(root string, limit int, stop *atomic.Bool, walk func(string, fs.WalkDirFunc) error) []Owned {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	var files []Owned
	walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(files) >= limit || stop.Load() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[strings.ToLower(d.Name())] ||
				strings.Count(strings.TrimPrefix(p, root), string(filepath.Separator)) > 4) {
				return filepath.SkipDir
			}
			return nil
		}
		if !videoExt.MatchString(d.Name()) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		r := ParseRelease(d.Name())
		if r.Title == "" || len(strings.Fields(Normalize(r.Title))) == 0 {
			// "03.mkv" in a title folder: the folder names the show.
			r.Title = ParseRelease(filepath.Base(filepath.Dir(p))).Title
		}
		files = append(files, Owned{Path: p, Release: r})
		return nil
	})
	return files
}

// Has returns the episodes of t the user already has, by episode key: on disk, or filed earlier.
func (l Library) Has(t TitleState) map[string]string {
	out := map[string]string{}
	for k, f := range t.Filed {
		out[k] = f.Path
	}
	for _, o := range l.Files {
		if Confidence(o.Release.Title, o.Release.Season, t.Aliases, t.Season) >= MatchThreshold {
			if k := EpisodeKey(o.Release); out[k] == "" {
				out[k] = o.Path
			}
		}
	}
	return out
}

// HasShow says whether the user already has any of a show: it isn't suggested again.
func (l Library) HasShow(m Media) bool {
	season := 0
	if len(m.Prequels) > 0 {
		season = -1 // a sequel's season number isn't known from AniList; only its own names count
	}
	for _, o := range l.Files {
		for _, a := range m.Aliases() {
			_, as := splitSeason(Normalize(a))
			if season < 0 && as == 0 {
				continue
			}
			if Confidence(o.Release.Title, o.Release.Season, []string{a}, max(season, 0)) >= MatchThreshold {
				return true
			}
		}
	}
	return false
}

// Downloading says whether the torrent app already has this release (any label), by info hash or name.
func Downloading(all []Torrent, it NyaaItem) bool {
	for _, t := range all {
		if (it.InfoHash != "" && strings.EqualFold(t.Hash, it.InfoHash)) || strings.EqualFold(t.Name, it.Title) {
			return true
		}
	}
	return false
}

// Show is one show found in the user's folders: a title and season with the episodes on disk.
type Show struct {
	Title    string   `json:"title"`
	Season   int      `json:"season,omitempty"`
	Episodes []string `json:"episodes"` // episode keys, in order
	Folders  []string `json:"folders"`  // where its files are
	Files    int      `json:"files"`
	AniList  int      `json:"anilist_id,omitempty"` // 0 = not confirmed as anime
	Name     string   `json:"anilist_title,omitempty"`
}

// Shows groups a library's files by show and season. Files without an episode number (films, home
// videos) are left out unless they're marked as anime releases ([Group] tags).
func (l Library) Shows() []Show {
	by := map[string]*Show{}
	var order []string
	for _, o := range l.Files {
		r := o.Release
		if r.Title == "" || (r.Episode == 0 && r.Kind != "special" && r.Group == "") {
			continue
		}
		n, s := splitSeason(Normalize(r.Title))
		if r.Season > 0 {
			s = r.Season
		}
		key := n + "|" + strconv.Itoa(max(s, 1))
		sh := by[key]
		if sh == nil {
			sh = &Show{Title: strings.TrimSpace(r.Title), Season: s}
			by[key] = sh
			order = append(order, key)
		}
		sh.Files++
		if k := EpisodeKey(r); r.Kind != "movie" && !contains(sh.Episodes, k) {
			sh.Episodes = append(sh.Episodes, k)
		}
		if d := filepath.Dir(o.Path); !contains(sh.Folders, d) {
			sh.Folders = append(sh.Folders, d)
		}
	}
	out := make([]Show, 0, len(order))
	for _, k := range order {
		sh := by[k]
		sort.Slice(sh.Episodes, func(i, j int) bool { return episodeLess(sh.Episodes[i], sh.Episodes[j]) })
		out = append(out, *sh)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}
