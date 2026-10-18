package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/platform"
	"noskill/internal/scheduler"
	"noskill/internal/store"
)

func TestCheckEpisodeName(t *testing.T) {
	for name, want := range map[string]string{
		"Frieren S02E03 [480p].mkv":                                   "",
		"Frieren S02E03.mkv":                                          "",
		"Battle Through the Heavens EP 216 [480p].mkv":                "",
		"Mushoku Tensei Episode 7 [480p].mp4":                         "",
		"Some Movie (2024).mkv":                                       "",
		"Frieren S02E03 [480p].srt":                                   "",
		"Gamer wa [Dungeon Shuukatsu] wo Play Suru S01E02 [480p].mkv": "", // brackets in a real title
		"Frieren S02E03 [480p].mkv.part":                              "unfinished download",
		"[SubsPlease] Frieren - 03 (480p) [A1B2C3D4].mkv":             "release-group prefix [SubsPlease] still in the name",
		"Frieren S02E03 [Erai-raws] [480p].mkv":                       "release tag Erai-raws still in the name",
		"Frieren S02E03 SubsPlease [480p].mkv":                        "release tag SubsPlease still in the name",
		"Frieren S02E03 [A1B2C3D4].mkv":                               "release hash [A1B2C3D4] still in the name",
		"Frieren S02E03 1080p [480p].mkv":                             "resolution 1080p outside the final [resolution]",
		"video.mkv":                                                   "no title and episode in the expected form",
	} {
		got := CheckEpisodeName(name)
		if got != want {
			t.Errorf("%q: got %q, want %q", name, got, want)
		}
	}
}

// The audit says PASS when the job ran and every file in the destination folder is one noskill
// recorded with a proper name, and FAIL for a failed night, a bad name or a file nobody recorded.
func TestWeekAudit(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, trainFake(new(bool)), nil, "")
	a := e.app
	st := &scheduler.Store{DB: a.DB}
	j, err := st.Add(ctx, scheduler.Job{Name: "anime-nightly", TargetKind: "prompt", Target: "x", Schedule: "cron:30 2 * * *", Missed: "once", Overlap: "skip", Enabled: true, CreatedAt: time.Now().Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	run := func(id, status, errText string, ago time.Duration) {
		at := time.Now().Add(-ago).UnixMilli()
		if _, err := a.DB.ExecContext(ctx, `INSERT INTO job_runs (id, job_id, run_id, scheduled_for, started_at, ended_at, status, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, j.ID, id, at, at, at, status, errText); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := a.WeekAudit(ctx, 24*time.Hour, "nope"); err == nil {
		t.Error("an unknown job")
	}
	r, err := a.WeekAudit(ctx, 24*time.Hour, "")
	if err != nil || r.Pass || r.Checks[0].Status != "failed" {
		t.Fatalf("no run in the window should fail: %v %+v", err, r)
	}

	run("R1", "done", "", 2*time.Hour)
	dest := filepath.Join(t.TempDir(), "Frieren")
	os.MkdirAll(dest, 0o755)
	good := filepath.Join(dest, "Frieren S02E03 [480p].mkv")
	os.WriteFile(good, []byte("video"), 0o644)
	if err := a.DB.AppendUndo(ctx, store.UndoEntry{RunID: "anime-1", Op: "copy", From: "/dl/x.mkv", To: good}); err != nil {
		t.Fatal(err)
	}
	r, err = a.WeekAudit(ctx, 24*time.Hour, "")
	if err != nil || !r.Pass || r.Filed != 1 {
		t.Fatalf("a clean night: %v %+v", err, r)
	}
	for _, c := range r.Checks {
		if c.Status == "failed" {
			t.Errorf("%+v", c)
		}
	}

	// A file nobody recorded.
	stray := filepath.Join(dest, "surprise.txt")
	os.WriteFile(stray, []byte("x"), 0o644)
	r, _ = a.WeekAudit(ctx, 24*time.Hour, "")
	var found bool
	for _, c := range r.Checks {
		if c.Name == "unexpected writes" && c.Status == "failed" && len(c.Items) == 1 && c.Items[0] == stray {
			found = true
		}
	}
	if r.Pass || !found {
		t.Errorf("a stray file wasn't caught: %+v", r)
	}
	os.Remove(stray)

	// A wrongly named file.
	bad := filepath.Join(dest, "[SubsPlease] Frieren - 04 (480p).mkv")
	os.WriteFile(bad, []byte("video"), 0o644)
	a.DB.AppendUndo(ctx, store.UndoEntry{RunID: "anime-1", Op: "copy", From: "/dl/y.mkv", To: bad})
	r, _ = a.WeekAudit(ctx, 24*time.Hour, "")
	if r.Pass || !strings.Contains(strings.Join(r.Checks[1].Items, " "), "SubsPlease") {
		t.Errorf("a bad name wasn't caught: %+v", r.Checks)
	}

	// A failed night.
	run("R2", "failed", "model didn't answer", time.Hour)
	if r, _ = a.WeekAudit(ctx, 24*time.Hour, ""); r.Pass || r.Checks[0].Status != "failed" || !strings.Contains(r.Checks[0].Items[0], "model didn't answer") {
		t.Errorf("a failed run: %+v", r.Checks[0])
	}
	// Outside the window it doesn't count.
	if r, _ = a.WeekAudit(ctx, 30*time.Minute, ""); r.Checks[0].Status != "failed" || !strings.Contains(r.Checks[0].Detail, "no completed run") {
		t.Errorf("window: %+v", r.Checks[0])
	}
}

// noPlaceTrash puts a file aside but doesn't say where, like the Windows Recycle Bin.
type noPlaceTrash struct {
	dir string
	n   *int
}

func (u noPlaceTrash) Trash(p string) (string, error) {
	*u.n++
	return "", os.Rename(p, filepath.Join(u.dir, fmt.Sprintf("%d-%s", *u.n, filepath.Base(p))))
}

// The file self-test passes where the trash says where it put a file (everything is undone), and
// warns, but still passes, where it doesn't (the trash step can't be undone by noskill there).
func TestFilesSelfTest(t *testing.T) {
	ctx := context.Background()
	for name, mk := range map[string]func(dir string, n *int) platform.Trasher{
		"known place":   func(dir string, n *int) platform.Trasher { return uniqTrash{dir, n} },
		"no place told": func(dir string, n *int) platform.Trasher { return noPlaceTrash{dir, n} },
	} {
		t.Run(name, func(t *testing.T) {
			a := newApp(t, trainFake(new(bool)), nil, "").app
			n := 0
			a.Trash = mk(t.TempDir(), &n)
			steps := a.FilesSelfTest(ctx)
			var warns int
			for _, s := range steps {
				if s.Status == "failed" {
					t.Errorf("%+v", s)
				}
				if s.Status == "warn" {
					warns++
				}
			}
			last := steps[len(steps)-1]
			if last.Name != "tidy up" || last.Status != "ok" {
				t.Errorf("last step: %+v", last)
			}
			if want := map[string]int{"known place": 0, "no place told": 1}[name]; warns != want {
				t.Errorf("%d warnings, want %d: %+v", warns, want, steps)
			}
			if n < 2 {
				t.Errorf("trash used %d times, want at least 2 (c.txt and the folder)", n)
			}
		})
	}
}
