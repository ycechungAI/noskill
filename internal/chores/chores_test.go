package chores

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/platform"
)

var monday = time.Date(2026, 10, 26, 8, 0, 0, 0, time.Local) // a Monday, 08:00

func TestParseReminder(t *testing.T) {
	cases := []struct {
		in, cron, every, msg string
		once                 bool
		at                   string // for once: "Mon 2 Jan 15:04"
	}{
		{in: "remind me to submit the report every Monday at nine", cron: "0 9 * * 1", msg: "Submit the report"},
		{in: "Every weekday at 8:30 remind me to stretch", cron: "30 8 * * 1-5", msg: "Stretch"},
		{in: "remind me to drink water every 2 hours", every: "2h", msg: "Drink water"},
		{in: "remind me to pay rent every month on the 1st at 9am", cron: "0 9 1 * *", msg: "Pay rent"},
		{in: "remind me to back up daily at 22:00", cron: "0 22 * * *", msg: "Back up"},
		{in: "call Sam tomorrow at 5pm", cron: "0 17 27 10 *", msg: "Call Sam", once: true},
		{in: "remind me in 20 minutes to check the oven", cron: "20 8 26 10 *", msg: "Check the oven", once: true},
		{in: "remind me on Friday at 3pm to send the invoice", cron: "0 15 30 10 *", msg: "Send the invoice", once: true},
		{in: "remind me to leave at 6", cron: "0 18 26 10 *", msg: "Leave", once: true}, // 6 with no am/pm is the afternoon
		{in: "remind me to wake up tomorrow at 7", cron: "0 7 27 10 *", msg: "Wake up", once: true},
		{in: "remind me about the meeting at noon", cron: "0 12 26 10 *", msg: "The meeting", once: true},
	}
	for _, c := range cases {
		r, err := ParseReminder(c.in, monday)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if r.Cron != c.cron || r.Every != c.every || r.Message != c.msg || r.Once != c.once {
			t.Errorf("%q: got cron %q every %q msg %q once %v (%s)", c.in, r.Cron, r.Every, r.Message, r.Once, r.When)
		}
		if r.When == "" {
			t.Errorf("%q: no description of what was understood", c.in)
		}
	}
}

func TestParseReminderAsksInsteadOfGuessing(t *testing.T) {
	for in, want := range map[string]string{
		"remind me to call Sam":           "when",
		"every Monday at nine":            "what should the reminder say",
		"remind me today to call Sam":     "today at what time",
		"remind me to nap every 1 minute": "at least 5",
		"":                                "say what",
	} {
		_, err := ParseReminder(in, monday)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("%q: err = %v, want it to mention %q", in, err, want)
		}
	}
}

func TestPlanTidy(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	week := now.Add(-3 * 24 * time.Hour)
	entries := []Entry{
		{Name: "report.pdf", ModTime: week},
		{Name: "photo.JPG", ModTime: week},
		{Name: "setup.dmg", ModTime: old},
		{Name: "setup-new.dmg", ModTime: week},
		{Name: "fresh.pdf", ModTime: now.Add(-time.Hour)},
		{Name: "movie.mkv", ModTime: week},
		{Name: "old-movie.mkv", ModTime: old},
		{Name: "big.iso.part", ModTime: old},
		{Name: "notes", IsDir: true, ModTime: old},
		{Name: ".hidden.pdf", ModTime: old},
		{Name: "mystery.xyz", ModTime: old},
		{Name: "link.pdf", Link: true, ModTime: old},
	}
	p := PlanTidy("/dl", entries, now, TidyOptions{})
	got := map[string]string{}
	for _, m := range p.Moves {
		got[filepath.Base(m.From)] = filepath.ToSlash(m.To)
	}
	want := map[string]string{
		"report.pdf":    "/dl/Sorted/Documents/report.pdf",
		"photo.JPG":     "/dl/Sorted/Images/photo.JPG",
		"setup-new.dmg": "/dl/Sorted/Installers/setup-new.dmg",
		"old-movie.mkv": "/dl/Sorted/Videos/old-movie.mkv",
	}
	if len(got) != len(want) {
		t.Errorf("moves = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
	if len(p.Trash) != 1 || filepath.Base(p.Trash[0]) != "setup.dmg" {
		t.Errorf("trash = %v, want only the 40-day-old installer", p.Trash)
	}
	reasons := map[string]string{}
	for _, k := range p.Kept {
		reasons[k.Name] = k.Reason
	}
	for _, n := range []string{"fresh.pdf", "movie.mkv", "big.iso.part", "mystery.xyz", "link.pdf"} {
		if reasons[n] == "" {
			t.Errorf("%s should be kept with a reason", n)
		}
	}
	if _, listed := reasons["notes"]; listed {
		t.Error("folders aren't listed")
	}
	if p.Summary["Documents"] != 1 {
		t.Errorf("summary = %v", p.Summary)
	}
}

func TestFindDuplicates(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string, age time.Duration) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
		return p
	}
	big := strings.Repeat("a", 2<<20)
	other := strings.Repeat("b", 2<<20) // same size as big, different content
	keep := write("Documents/film.mkv", big, 100*time.Hour)
	write("Downloads/film.mkv", big, time.Hour)
	write("Documents/film (1).mkv", big, 50*time.Hour)
	write("Documents/other.mkv", other, 100*time.Hour)
	write("Documents/tiny.txt", "same", time.Hour)
	write("Documents/tiny2.txt", "same", time.Hour)
	if err := os.Link(keep, filepath.Join(dir, "Documents", "hardlink.mkv")); err != nil {
		t.Log("no hard links here:", err)
	}
	write(".hidden/film.mkv", big, time.Hour)

	res, err := FindDuplicates(context.Background(), []string{dir}, DupeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("groups = %+v", res.Groups)
	}
	g := res.Groups[0]
	if len(g.Files) != 3 {
		t.Errorf("files = %+v (the copy in a hidden folder and the hard link don't count)", g.Files)
	}
	if g.Keep != keep {
		t.Errorf("keep = %s, want %s (not in Downloads, not named like a copy, oldest)", g.Keep, keep)
	}
	if res.WastedBytes != 2*(2<<20) {
		t.Errorf("wasted = %d", res.WastedBytes)
	}
}

func TestPlanRenames(t *testing.T) {
	names := []string{
		"[SubsPlease] Mortal Journey - 197 (480p) [ABCD1234].mkv",
		"[SubsPlease] Mortal Journey - 198 (480p) [ABCD1235].mkv",
		"Mortal Journey Episode 199 [480p].mkv",
		"readme.txt",
		"random video.mkv",
	}
	p := PlanRenames("/lib/Mortal", append([]string(nil), names...), RenameOptions{Show: "A Mortal's Journey to Immortality"})
	if len(p.Moves) != 3 {
		t.Fatalf("moves = %+v kept = %+v", p.Moves, p.Kept)
	}
	have := map[string]bool{}
	for _, m := range p.Moves {
		have[filepath.Base(m.To)] = true
	}
	for _, ep := range []string{"197", "198", "199"} {
		if n := "A Mortal's Journey to Immortality Episode " + ep + " [480p].mkv"; !have[n] {
			t.Errorf("no rename to %q in %v", n, have)
		}
	}
	for _, m := range p.Moves {
		if filepath.Dir(m.To) != "/lib/Mortal" {
			t.Errorf("%s leaves the folder", m.To)
		}
	}
	kept := map[string]bool{}
	for _, k := range p.Kept {
		kept[k.Name] = true
	}
	if !kept["random video.mkv"] || kept["readme.txt"] {
		t.Errorf("kept = %+v (an unreadable video is kept; non-video files aren't mentioned)", p.Kept)
	}
	// Running it again on the new names changes nothing.
	var again []string
	for _, m := range p.Moves {
		again = append(again, filepath.Base(m.To))
	}
	if p2 := PlanRenames("/lib/Mortal", again, RenameOptions{Show: "A Mortal's Journey to Immortality"}); len(p2.Moves) != 0 {
		t.Errorf("second pass proposes %+v", p2.Moves)
	}
}

func TestCheckVolumesAndAlert(t *testing.T) {
	vs := []platform.VolumeInfo{
		{Label: "Macintosh HD", Mount: "/", FreeBytes: 500e9, TotalBytes: 1000e9},
		{Label: "RED", Mount: "/Volumes/RED", FreeBytes: 12e9, TotalBytes: 4000e9, Removable: true},
		{Label: "BLUE", Mount: "/Volumes/BLUE", FreeBytes: 150e9, TotalBytes: 4000e9, Removable: true},
		{Label: "Lockdown 1.0", Mount: "/Library/Lockdown", FreeBytes: 0, TotalBytes: 80e6}, // a disk image: never reported
	}
	r := CheckVolumes(vs, DiskOptions{})
	if len(r) != 3 || r[0].Label != "RED" || !r[0].Low {
		t.Fatalf("report = %+v", r)
	}
	if !r[1].Low { // BLUE: 3.75% free is under the 5% limit although 150 GB is plenty
		t.Errorf("BLUE should be low by percentage: %+v", r[1])
	}
	if got := Alert(r); !strings.Contains(got, "RED") || strings.Contains(got, "Macintosh") {
		t.Errorf("alert = %q", got)
	}
	if only := CheckVolumes(vs, DiskOptions{Only: []string{"red"}}); len(only) != 1 {
		t.Errorf("only red: %+v", only)
	}
}

func TestBriefText(t *testing.T) {
	now := time.Date(2026, 10, 25, 7, 0, 0, 0, time.Local)
	f := BriefFacts{Since: now.Add(-9 * time.Hour)}
	if got := f.Text(now); !strings.HasPrefix(got, "Nothing needs you") {
		t.Errorf("empty digest = %q", got)
	}
	f = BriefFacts{Since: now.Add(-9 * time.Hour), JobsRan: 3, JobsFailed: []string{"anime-follow"}, Filed: []string{"A 1", "B 2"},
		ApprovalsWaiting: 1, LowVolumes: "RED: 12 GB free", Next: []string{"anime-follow today 14:00"}}
	got := f.Text(now)
	for _, want := range []string{"3 scheduled jobs ran", "1 failed (anime-follow)", "2 episodes filed: A 1, B 2", "1 approval waiting", "Low disk space: RED", "Next: anime-follow"} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q:\n%s", want, got)
		}
	}
}

func TestUsageFindsBigFoldersAndIdleFiles(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string, n int, age time.Duration) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, n), 0o644)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	mk("big/a.bin", 3000, 400*24*time.Hour)
	mk("big/b.bin", 3000, time.Hour)
	mk("small/c.bin", 100, time.Hour)
	folders, old, trunc := Usage(context.Background(), dir, 5, 1000, 365*24*time.Hour, time.Minute)
	if trunc || len(folders) != 2 || filepath.Base(folders[0].Path) != "big" || folders[0].Bytes != 6000 {
		t.Errorf("folders = %+v", folders)
	}
	if len(old) != 1 || filepath.Base(old[0].Path) != "a.bin" {
		t.Errorf("old = %+v", old)
	}
}
