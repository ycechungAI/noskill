package chores

import (
	"path/filepath"
	"sort"
	"strings"

	"noskill/internal/anime"
)

// RenameOptions tunes PlanRenames.
type RenameOptions struct {
	Show     string // the show's name for every file; empty = read it from each file name
	Style    string // episode ("Show Episode 3"), ep ("Show EP 3") or sxxexx ("Show S01E03"); default episode
	Sanitize func(name, fs string) string
	FS       string
}

// RenamePlan is the renames PlanRenames proposes, for the files skill's move_many.
type RenamePlan struct {
	Moves []Move `json:"moves"`
	Kept  []Kept `json:"kept"`
}

var videoExts = map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".m4v": true, ".webm": true, ".ts": true, ".mov": true}

// PlanRenames gives the video files of one folder one naming pattern, reading the show and episode
// out of each name with the anime tracker's own parser (so the library and the tracker agree). Files
// it can't read, batches and files already named that way are left alone; nothing is moved out of
// the folder and a name that would collide is skipped.
func PlanRenames(dir string, names []string, o RenameOptions) RenamePlan {
	p := RenamePlan{Moves: []Move{}, Kept: []Kept{}}
	style := o.Style
	if style == "" {
		style = "episode"
	}
	nm := anime.Naming{Episodes: style, Sanitize: o.Sanitize, FS: o.FS}
	if style == "sxxexx" {
		nm.Episodes = ""
	}
	type item struct {
		name string
		r    anime.Release
	}
	var items []item
	var maxEp float64
	sort.Strings(names)
	for _, n := range names {
		if !videoExts[strings.ToLower(filepath.Ext(n))] {
			continue
		}
		r := anime.ParseRelease(n)
		switch {
		case strings.TrimSpace(o.Show) == "" && r.Title == "":
			p.Kept = append(p.Kept, Kept{n, "can't tell the show"})
		case r.Kind == "batch" || r.EpisodeEnd > r.Episode && r.Episode > 0:
			p.Kept = append(p.Kept, Kept{n, "several episodes in one file"})
		case r.Kind == "episode" && r.Episode == 0:
			p.Kept = append(p.Kept, Kept{n, "can't tell the episode"})
		case r.Kind == "other":
			p.Kept = append(p.Kept, Kept{n, "doesn't look like an episode"})
		default:
			items = append(items, item{n, r})
			maxEp = max(maxEp, r.Episode)
		}
	}
	taken := map[string]bool{}
	for _, it := range items {
		show := strings.TrimSpace(o.Show)
		if show == "" {
			show = it.r.Title
		}
		t := anime.TitleState{Title: show, Season: it.r.Season, Episodes: int(maxEp)}
		to := nm.PlanName(dir, t, it.r)
		from := filepath.Join(dir, it.name)
		switch {
		case to == from:
			p.Kept = append(p.Kept, Kept{it.name, "already named this way"})
		case taken[to]:
			p.Kept = append(p.Kept, Kept{it.name, "another file would get the same name"})
		default:
			taken[to] = true
			p.Moves = append(p.Moves, Move{From: from, To: to})
		}
	}
	return p
}
