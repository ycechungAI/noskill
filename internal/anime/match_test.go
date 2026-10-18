package anime

import (
	"path/filepath"
	"strings"
	"testing"
)

var frieren2 = TitleState{ID: 182255, Title: "Frieren: Beyond Journey's End Season 2", Season: 2, Episodes: 12,
	Aliases: []string{"Frieren: Beyond Journey's End Season 2", "Sousou no Frieren 2nd Season", "Frieren S2"}}

var frieren1 = TitleState{ID: 154587, Title: "Frieren: Beyond Journey's End", Episodes: 28,
	Aliases: []string{"Frieren: Beyond Journey's End", "Sousou no Frieren", "Frieren"}}

func TestConfidence(t *testing.T) {
	cases := []struct {
		name string
		t    TitleState
		ok   bool
	}{
		{"[SubsPlease] Sousou no Frieren S2 - 05 (480p) [0F0F0F0F].mkv", frieren2, true},
		{"[Erai-raws] Frieren 2nd Season - 01 [480p][Multiple Subtitle].mkv", frieren2, true},
		{"[SubsPlease] Sousou no Frieren - 05 (480p) [0F0F0F0F].mkv", frieren2, false}, // season 1
		{"[SubsPlease] Sousou no Frieren - 05 (480p) [0F0F0F0F].mkv", frieren1, true},
		{"[SubsPlease] Sousou no Frieren S2 - 05 (480p) [0F0F0F0F].mkv", frieren1, false},
		{"[SubsPlease] Frieren no Mahou - 01 (480p).mkv", frieren1, false}, // a look-alike
		{"Frieren.S01E03.480p.WEB.H264-VARYG.mkv", frieren1, true},
	}
	for _, c := range cases {
		r := ParseRelease(c.name)
		got := Confidence(r.Title, r.Season, c.t.Aliases, c.t.Season)
		if (got >= MatchThreshold) != c.ok {
			t.Errorf("%s vs %s: confidence %.2f", c.name, c.t.Title, got)
		}
	}
}

func TestBuildRuleMatches(t *testing.T) {
	r := BuildRule(frieren2, "480p", []string{"HEVC 10bit"}, nil)
	for name, want := range map[string]bool{
		"[SubsPlease] Sousou no Frieren S2 - 05 (480p) [0F0F0F0F].mkv":            true,
		"[Erai-raws] Sousou no Frieren 2nd Season - 05 [480p][Multiple Subtitle]": true,
		"[SubsPlease] Sousou no Frieren S2 - 05 (720p) [0F0F0F0F].mkv":            false, // resolution
		"[SubsPlease] Sousou no Frieren - 05 (480p) [0F0F0F0F].mkv":               false, // season 1
		"[Group] Sousou no Frieren S2 - 05 [480p HEVC 10bit]":                     false, // avoided
		"[Group] Sousou no Frieren S2 - 01-12 (Batch) [480p]":                     false,
		"[Group] Sousou no Frieren S2 (01-12) [480p]":                             false,
		"Sousou.no.Frieren.S02E05.480p.WEB.H264-X":                                true,
	} {
		got, err := RuleMatches(r, name)
		if err != nil || got != want {
			t.Errorf("%s: %v %v\n%s\n%s", name, got, err, r.MustContain, r.MustNotContain)
		}
	}
	one := BuildRule(frieren1, "480p", nil, nil)
	if ok, _ := RuleMatches(one, "[SubsPlease] Sousou no Frieren S2 - 05 (480p)"); ok {
		t.Error("first-season rule matched season 2")
	}
	if ok, _ := RuleMatches(one, "[SubsPlease] Sousou no Frieren - 05 (480p)"); !ok {
		t.Error("first-season rule missed its episode")
	}
	// An episode that fell back to 720p is excluded, so its 480p release isn't downloaded later.
	f := frieren1
	f.Excluded = []float64{5}
	ex := BuildRule(f, "480p", nil, nil)
	if ok, _ := RuleMatches(ex, "[SubsPlease] Sousou no Frieren - 05 (480p)"); ok {
		t.Error("excluded episode still matched")
	}
	if ok, _ := RuleMatches(ex, "[SubsPlease] Sousou no Frieren - 15 (480p)"); !ok {
		t.Error("exclusion of 5 hit 15")
	}
	if ok, _ := RuleMatches(ex, "Sousou.no.Frieren.S01E05.480p.WEB"); ok {
		t.Error("excluded S01E05 matched")
	}
}

func TestPlanName(t *testing.T) {
	n := Naming{TitleFolders: true}
	ep := ParseRelease("[SubsPlease] Sousou no Frieren S2 - 03 (480p) [0F0F0F0F].mkv")
	got := n.PlanName("/Volumes/RED/ANIME2026", frieren2, ep)
	want := filepath.Join("/Volumes/RED/ANIME2026", "Frieren: Beyond Journey's End Season 2", "Frieren: Beyond Journey's End Season 2 S02E03 [480p].mkv")
	if got != want {
		t.Errorf("simple: %s", got)
	}
	n.Sanitize = func(s, _ string) string { return strings.ReplaceAll(s, ":", " -") }
	if got := n.PlanName("/d", frieren2, ep); !strings.Contains(got, "Frieren - Beyond") {
		t.Errorf("sanitized: %s", got)
	}
	for style, want := range map[string]string{"": "Frieren S01E05 [480p].mkv", "ep": "Frieren EP 5 [480p].mkv", "episode": "Frieren Episode 5 [480p].mkv"} {
		f := TitleState{Title: "Frieren", Episodes: 28, Aliases: frieren1.Aliases}
		got := (Naming{Episodes: style}).PlanName("/d", f, ParseRelease("[SubsPlease] Sousou no Frieren - 05 (480p) [AB].mkv"))
		if got != filepath.Join("/d", want) {
			t.Errorf("%q: %s", style, got)
		}
	}
	op := TitleState{Title: "One Piece", Episodes: 1100}
	if got := (Naming{TitleFolders: true}).PlanName("/d", op, ParseRelease("[SubsPlease] One Piece - 1122 (480p).mkv")); got != filepath.Join("/d", "One Piece", "One Piece S01E1122 [480p].mkv") {
		t.Errorf("long: %s", got)
	}
	plex := Naming{Preset: "plex-jellyfin"}
	f := frieren2
	f.Year = 2026
	if got := plex.PlanName("/d", f, ep); got != filepath.Join("/d", "Frieren: Beyond Journey's End Season 2 (2026)", "Season 02", "Frieren: Beyond Journey's End Season 2 - S02E03 [480p].mkv") {
		t.Errorf("plex: %s", got)
	}
}

// Every naming style reads back as the same show and episode, so files noskill filed count as
// "already have it" on the next run, and a special is never taken for episode 1.
func TestNamesReadBack(t *testing.T) {
	shows := []TitleState{
		{Title: "Frieren", Aliases: []string{"Frieren"}, Episodes: 28},
		{Title: "Frieren Season 2", Aliases: []string{"Frieren Season 2", "Sousou no Frieren 2nd Season"}, Season: 2, Episodes: 12},
		{Title: "Kaijuu 8-gou", Aliases: []string{"Kaijuu 8-gou"}, Episodes: 12},
	}
	rels := []string{"[G] X - 03 (480p).mkv", "[G] X - 12.5 (480p).mkv", "[G] X - SP2 (480p).mkv"}
	for _, style := range []string{"", "ep", "episode"} {
		for _, sh := range shows {
			for _, rn := range rels {
				r := ParseRelease(rn)
				p := (Naming{Episodes: style, TitleFolders: true}).PlanName("/d", sh, r)
				back := ParseRelease(filepath.Base(p))
				lib := Library{Files: []Owned{{Path: p, Release: back}}}
				have := lib.Has(sh)
				if have[EpisodeKey(r)] == "" || len(have) != 1 {
					t.Errorf("%q %s: %s read back as %+v (have %v, want %s)", style, sh.Title, filepath.Base(p), back, have, EpisodeKey(r))
				}
			}
		}
	}
}
