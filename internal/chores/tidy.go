// Package chores holds the logic of the housekeeping skills: downloads-tidy, duplicate-finder,
// disk-watch, media-renamer and daily-brief. It is plain code that makes plans and reports; the
// tools in internal/tools/builtin do the permission checks and the moves, and a model only starts a
// step and reads the result (the anime lesson: the steps run in Go, the model's part stays small).
package chores

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one item of a folder listing, as the caller read it.
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
	Link    bool // a symlink: never touched
}

// Move is one planned move; the plan goes to the files skill's move_many.
type Move struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Kept is a file the plan leaves alone, and why.
type Kept struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// TidyPlan is what PlanTidy proposes. Nothing is applied here.
type TidyPlan struct {
	Moves []Move   `json:"moves"`
	Trash []string `json:"trash"` // old installers: they go to the OS trash, never deleted
	Kept  []Kept   `json:"kept"`
	// Summary counts the moves per folder.
	Summary map[string]int `json:"summary"`
}

// TidyOptions tunes PlanTidy. Zero values take the defaults.
type TidyOptions struct {
	MinAge       time.Duration // a file touched more recently is left alone (24h): it may still be downloading
	VideoAge     time.Duration // videos wait longer (14 days): the anime tracker downloads here and files them itself
	InstallerAge time.Duration // an installer this old goes to the trash (30 days)
	Folder       string        // where sorted files go, inside the folder ("Sorted")
	Only         []string      // act on these categories only; empty = all
}

func (o TidyOptions) withDefaults() TidyOptions {
	if o.MinAge <= 0 {
		o.MinAge = 24 * time.Hour
	}
	if o.VideoAge <= 0 {
		o.VideoAge = 14 * 24 * time.Hour
	}
	if o.InstallerAge <= 0 {
		o.InstallerAge = 30 * 24 * time.Hour
	}
	if o.Folder == "" {
		o.Folder = "Sorted"
	}
	return o
}

var categories = map[string][]string{
	"Images":       {"jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "tiff", "svg", "raw", "cr2", "nef"},
	"Documents":    {"pdf", "doc", "docx", "odt", "rtf", "txt", "md", "epub", "pages", "ppt", "pptx", "key"},
	"Spreadsheets": {"xls", "xlsx", "csv", "ods", "numbers", "tsv"},
	"Archives":     {"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz"},
	"Installers":   {"dmg", "pkg", "exe", "msi", "deb", "rpm", "appimage"},
	"Videos":       {"mkv", "mp4", "avi", "mov", "m4v", "webm", "wmv", "flv", "ts"},
	"Audio":        {"mp3", "flac", "wav", "m4a", "aac", "ogg", "opus"},
	"Torrents":     {"torrent"},
}

var byExt = func() map[string]string {
	m := map[string]string{}
	for cat, exts := range categories {
		for _, e := range exts {
			m[e] = cat
		}
	}
	return m
}()

// Partial downloads and temporary files: never touched.
var partialExt = map[string]bool{"part": true, "crdownload": true, "download": true, "tmp": true, "opdownload": true, "aria2": true, "!qb": true, "resume": true}

// Category names the folder a file belongs in, or "" when it has no obvious one.
func Category(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return byExt[ext]
}

// PlanTidy sorts the loose files of root into <root>/<Folder>/<Category>/. Folders, links, hidden
// files, partial downloads, recent files and files with no obvious category stay where they are.
func PlanTidy(root string, entries []Entry, now time.Time, o TidyOptions) TidyPlan {
	o = o.withDefaults()
	only := map[string]bool{}
	for _, c := range o.Only {
		only[strings.ToLower(c)] = true
	}
	p := TidyPlan{Moves: []Move{}, Trash: []string{}, Kept: []Kept{}, Summary: map[string]int{}}
	keep := func(e Entry, why string) { p.Kept = append(p.Kept, Kept{e.Name, why}) }
	taken := map[string]bool{}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, e := range entries {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Name), "."))
		cat := Category(e.Name)
		age := now.Sub(e.ModTime)
		switch {
		case e.IsDir:
			continue // folders (a torrent's folder, a project) are the user's: not listed
		case e.Link:
			keep(e, "a link")
		case strings.HasPrefix(e.Name, "."):
			continue
		case partialExt[ext]:
			keep(e, "a download in progress")
		case cat == "":
			keep(e, "no obvious folder for it")
		case len(only) > 0 && !only[strings.ToLower(cat)]:
			continue
		case age < o.MinAge:
			keep(e, "changed in the last "+shortDur(o.MinAge))
		case cat == "Videos" && age < o.VideoAge:
			keep(e, "a video newer than "+shortDur(o.VideoAge)+" (the anime tracker files these)")
		case cat == "Installers" && age >= o.InstallerAge:
			p.Trash = append(p.Trash, filepath.Join(root, e.Name))
		default:
			to := filepath.Join(root, o.Folder, cat, e.Name)
			if taken[to] {
				keep(e, "another file would take its place")
				continue
			}
			taken[to] = true
			p.Moves = append(p.Moves, Move{From: filepath.Join(root, e.Name), To: to})
			p.Summary[cat]++
		}
	}
	return p
}

func shortDur(d time.Duration) string {
	if d >= 48*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + " days"
	}
	return strconv.Itoa(int(d/time.Hour)) + " hours"
}
