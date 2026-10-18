package anime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseSizeAndHD(t *testing.T) {
	for in, want := range map[string]int64{"385.3 MiB": 404016332, "1.5 GiB": 1610612736, "500 MiB": 500 << 20,
		"900 KiB": 900 << 10, "1,024 MiB": 1 << 30, "": -1, "big": -1} {
		if got := ParseSize(in); got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for res, hd := range map[string]bool{"480p": false, "576p": false, "720p": true, "1080p": true, "2160p": true, "4K": true, "": false} {
		if IsHD(res) != hd {
			t.Errorf("IsHD(%q) = %v", res, !hd)
		}
	}
}

// "Don't download 720p or higher unless the file is 500 MB or less."
func TestHDOnlyWhenSmall(t *testing.T) {
	for _, c := range []struct {
		res, size string
		big       bool
	}{
		{"480p", "2 GiB", false},   // 480p is never held back
		{"720p", "400 MiB", false}, // small enough
		{"720p", "500 MiB", false}, // the limit itself is fine
		{"720p", "501 MiB", true},  // one MiB over
		{"1080p", "1.1 GiB", true}, // too big
		{"1080p", "", true},        // unknown size: not proven small
		{"", "3 GiB", false},       // no resolution tag: not HD
	} {
		if got := tooBig(c.res, c.size, 500); got != c.big {
			t.Errorf("tooBig(%q, %q) = %v", c.res, c.size, got)
		}
	}
	if tooBig("1080p", "9 GiB", 0) {
		t.Error("0 means no limit")
	}
}

func TestEnglishSubs(t *testing.T) {
	for name, ok := range map[string]bool{
		"[SubsPlease] Show - 03 (480p) [ABCD1234].mkv":              true,
		"[Erai-raws] Show - 03 [480p][Multiple Subtitle][ENG][POR]": true,
		"[Group] Show - 03 [1080p] [CHS]":                           false,
		"[Group] Show - 03 [1080p] [ENG+CHS]":                       true,
		"[Group] Show - 03 [BIG5][720p]":                            false,
		"[Group] Show - 03 Portuguese hardsub":                      false,
		"[Group] Show - 03 (Chinese Doll) [480p]":                   false, // tagged only as a language: dropped
		"[Group] Strawberry Panic - 03 [480p]":                      true,
		"[Group] Show - 03 [480p] english":                          true,
	} {
		if EnglishSubs(name) != ok {
			t.Errorf("EnglishSubs(%q) = %v", name, !ok)
		}
	}
}

// Erai-raws first at 480p; another group only when Erai-raws has no 480p.
func TestPreferredGroupWinsAtTheSameResolution(t *testing.T) {
	mk := func(group string, seeds int, trusted bool) match {
		return match{item: NyaaItem{Seeders: seeds, Trusted: trusted, Published: now}, rel: Release{Group: group, Resolution: "480p"}}
	}
	tk := &Tracker{Now: func() time.Time { return now }, Settings: Settings{PreferGroups: []string{"Erai-raws"}}.withDefaults()}
	got, _ := tk.pick([]match{mk("SubsPlease", 900, true), mk("Erai-raws", 5, false), mk("Other", 100, true)}, "480p")
	if got == nil || got.rel.Group != "Erai-raws" {
		t.Fatalf("picked %+v", got)
	}
	got, _ = tk.pick([]match{mk("SubsPlease", 50, false), mk("Other", 100, false)}, "480p")
	if got == nil || got.rel.Group != "Other" {
		t.Errorf("without Erai-raws the seeders decide: %+v", got)
	}
}

// releases() drops big HD files and non-English uploads before anything is chosen.
func TestReleasesAreFilteredBySizeAndLanguage(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><rss xmlns:nyaa="https://nyaa.si/xmlns/nyaa" version="2.0"><channel>`)
		for i, it := range []struct{ title, size string }{
			{"[SubsPlease] Sousou no Frieren S2 - 01 (480p) [AAAA1111].mkv", "150 MiB"},
			{"[SubsPlease] Sousou no Frieren S2 - 02 (720p) [BBBB1111].mkv", "400 MiB"},
			{"[SubsPlease] Sousou no Frieren S2 - 03 (1080p) [CCCC1111].mkv", "1.3 GiB"},
			{"[Group] Sousou no Frieren S2 - 04 (480p) [CHS] [DDDD1111].mkv", "120 MiB"},
			{"[Erai-raws] Sousou no Frieren S2 - 05 [720p][Multiple Subtitle]", ""},
		} {
			fmt.Fprintf(w, `<item><title>%s</title><link>%s/download/%d.torrent</link><guid>%s/view/%d</guid><pubDate>%s</pubDate>
<nyaa:seeders>10</nyaa:seeders><nyaa:infoHash>%040d</nyaa:infoHash><nyaa:size>%s</nyaa:size><nyaa:categoryId>1_2</nyaa:categoryId><nyaa:trusted>Yes</nyaa:trusted><nyaa:remake>No</nyaa:remake></item>`,
				it.title, srv.URL, i, srv.URL, i, now.Add(-time.Hour).Format("Mon, 02 Jan 2006 15:04:05 -0700"), i, it.size)
		}
		fmt.Fprint(w, `</channel></rss>`)
	}))
	defer srv.Close()
	tk := &Tracker{Nyaa: &Nyaa{Base: srv.URL}, Now: func() time.Time { return now }, Settings: Settings{HDMaxMB: 500}.withDefaults()}
	ts := TitleState{ID: 2, Title: "Sousou no Frieren 2nd Season", Aliases: []string{"Sousou no Frieren 2nd Season"}, Season: 2}
	eps, err := tk.releases(context.Background(), ts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range eps {
		got = append(got, k)
	}
	if len(got) != 2 || eps["1"] == nil || eps["2"] == nil {
		t.Errorf("kept episodes %v (want 1 at 480p and 2 at 720p/400 MiB only)", got)
	}
	tk.Settings.HDMaxMB = 0
	if eps, _ = tk.releases(context.Background(), ts); eps["3"] == nil || eps["5"] == nil || eps["4"] != nil {
		t.Errorf("with no limit the big and unknown-size HD files are kept, the CHS one still isn't: %v", eps)
	}
}

// Whichever group a file comes from, it is renamed the same way: title, season and episode, resolution.
func TestEveryGroupGetsTheSameFileName(t *testing.T) {
	ts := TitleState{ID: 2, Title: "Sousou no Frieren 2nd Season", Aliases: []string{"Sousou no Frieren 2nd Season"}, Season: 2}
	n := Naming{TitleFolders: true}
	var names []string
	for _, rel := range []string{
		"[SubsPlease] Sousou no Frieren S2 - 03 (480p) [ABCD1234].mkv",
		"[Erai-raws] Sousou no Frieren 2nd Season - 03 [480p][Multiple Subtitle][EEEE5555].mkv",
	} {
		r, c := ParseNamed(rel, ts.Aliases, ts.Season)
		if c < MatchThreshold {
			t.Fatalf("%s doesn't match its title (%.2f)", rel, c)
		}
		names = append(names, n.PlanName("/lib", ts, r))
	}
	if names[0] != names[1] || names[0] != "/lib/Sousou no Frieren 2nd Season/Sousou no Frieren 2nd Season S02E03 [480p].mkv" {
		t.Errorf("names differ: %q", names)
	}
}

func TestSeasonEnd(t *testing.T) {
	for in, want := range map[time.Time]string{
		time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC):  "2026-12-31 23:59:59",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC):    "2026-03-31 23:59:59",
		time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC):   "2026-06-30 23:59:59",
		time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC):   "2026-09-30 23:59:59",
		time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC): "2026-12-31 23:59:59",
	} {
		if got := seasonEnd(in).Format("2006-01-02 15:04:05"); got != want {
			t.Errorf("seasonEnd(%v) = %s, want %s", in, got, want)
		}
	}
	// Late on the last day there is still a day to answer.
	if d := untilSeasonEnd(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)); d < 24*time.Hour {
		t.Errorf("got %v", d)
	}
	if d := untilSeasonEnd(time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)); d < 70*24*time.Hour {
		t.Errorf("fall card open for only %v", d)
	}
}
