package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/backendtest"
	"noskill/internal/scheduler"
)

// FR-J6: back up the jobs, lose them (a new account), restore them.
func TestJobBackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	add := func(s JobSpec) {
		t.Helper()
		if _, err := e.app.AddJob(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	add(JobSpec{Target: "anime:download,file", Cron: "0 14,16 * * 6,0", When: []string{"volume:RED"}, Timeout: "1h"})
	add(JobSpec{Target: "diskwatch", Cron: "30 6 * * *", Paused: true})
	add(JobSpec{Target: "files", Every: "6h", Prompt: "tidy the inbox", Allow: []string{"write:~/Inbox"}})

	info, err := e.app.BackupJobs(ctx, "")
	if err != nil || info.Jobs != 3 || info.Unchanged || filepath.Dir(info.Path) != e.app.JobBackupsDir() {
		t.Fatalf("%+v %v", info, err)
	}
	if fi, err := os.Stat(info.Path); err != nil || fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("backup should be private to the user: %v %v", fi.Mode(), err)
	}
	// Nothing changed: no second file.
	again, err := e.app.BackupJobs(ctx, "")
	if err != nil || !again.Unchanged || again.Name != info.Name {
		t.Errorf("unchanged jobs wrote a new backup: %+v %v", again, err)
	}

	// A fresh account: same backup file, no jobs.
	e2 := newApp(t, &backendtest.Fake{}, noRules, "")
	data, _ := os.ReadFile(info.Path)
	os.MkdirAll(e2.app.JobBackupsDir(), 0o700)
	os.WriteFile(filepath.Join(e2.app.JobBackupsDir(), info.Name), data, 0o600)
	list, err := e2.app.ListJobBackups()
	if err != nil || len(list) != 1 || list[0].Jobs != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	rep, err := e2.app.RestoreJobBackup(ctx, info.Name)
	if err != nil || len(rep.Added) != 3 || len(rep.Skipped) != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	js, _ := (&scheduler.Store{DB: e2.app.DB}).List(ctx)
	byName := map[string]scheduler.Job{}
	for _, j := range js {
		byName[j.Name] = j
	}
	a := byName["anime-download-file"]
	if a.TargetKind != "anime" || a.Target != "download,file" || a.Schedule != "cron:0 14,16 * * 6,0" || a.Timeout.Hours() != 1 ||
		len(a.Preconditions) != 1 || !a.Enabled || a.NextRunAt.IsZero() {
		t.Errorf("anime job: %+v", a)
	}
	if d := byName["diskwatch"]; d.Enabled {
		t.Error("a paused job came back running")
	}
	if f := byName["files"]; f.Args.Prompt != "tidy the inbox" || len(f.Args.Allow) != 1 {
		t.Errorf("skill job: %+v", f)
	}

	// Restoring twice adds nothing and replaces nothing.
	rep, err = e2.app.RestoreJobBackup(ctx, info.Name)
	if err != nil || len(rep.Added) != 0 || len(rep.Skipped) != 3 || !strings.Contains(rep.Skipped[0].Why, "already exists") {
		t.Errorf("%+v %v", rep, err)
	}
}

func TestJobRestoreSkipsWhatItCantRun(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	b := JobBackup{Format: 1, Jobs: []BackedUpJob{
		{Name: "gone", TargetKind: "skill", Target: "no-such-skill", Schedule: "every:6h"},
		{Name: "odd", TargetKind: "teleport", Schedule: "every:6h"},
		{Name: "never", TargetKind: "brief", Target: "24", Schedule: "cron:0 0 31 2 *"},
		{Name: "", TargetKind: "brief", Target: "24", Schedule: "every:1d"},
		{Name: "ok", TargetKind: "brief", Target: "24", Schedule: "every:1d", Missed: "bogus", Timeout: "nonsense", Enabled: true},
	}}
	rep, err := e.app.RestoreJobs(ctx, b)
	if err != nil || len(rep.Added) != 1 || rep.Added[0] != "ok" || len(rep.Skipped) != 4 {
		t.Fatalf("%+v %v", rep, err)
	}
	j, _ := (&scheduler.Store{DB: e.app.DB}).Get(ctx, "ok")
	if j.Missed != e.app.Cfg.Scheduler.MissedPolicy || j.Timeout != e.app.Cfg.Scheduler.DefaultTimeout.Duration {
		t.Errorf("bad fields should take the defaults: %+v", j)
	}
}

func TestJobBackupFilesAreChecked(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	for _, bad := range []string{"not json", `{"jobs": []}`, `{"noskill_jobs_backup": 99, "jobs": []}`} {
		var ue *UserError
		if _, err := ParseJobBackup([]byte(bad)); !errors.As(err, &ue) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, name := range []string{"../state.db", "", ".hidden", "missing.json"} {
		if _, err := e.app.RestoreJobBackup(ctx, name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	// A stray file in the folder isn't listed.
	os.MkdirAll(e.app.JobBackupsDir(), 0o700)
	os.WriteFile(filepath.Join(e.app.JobBackupsDir(), "notes.json"), []byte(`{"a":1}`), 0o600)
	if list, _ := e.app.ListJobBackups(); len(list) != 0 {
		t.Errorf("%+v", list)
	}
}

func TestBackupJobKind(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	j, err := e.app.AddJob(ctx, JobSpec{Target: "backup", Cron: "0 4 * * *"})
	if err != nil || j.TargetKind != "backup" || j.Name != "jobs-backup" || len(j.Notify) != 0 {
		t.Fatalf("%+v %v", j, err)
	}
	if _, err := e.app.AddJob(ctx, JobSpec{Target: "backup:everything", Cron: "0 4 * * *", Name: "x"}); err == nil {
		t.Error("backup took an argument")
	}
	res := Jobs{A: e.app}.Start(ctx, j)
	if res.Status != "done" || !strings.Contains(res.Summary, "backed up 1 jobs") {
		t.Fatalf("%+v", res)
	}
	if res = (Jobs{A: e.app}).Start(ctx, j); res.Status != "done" || !res.Quiet {
		t.Errorf("an unchanged backup should be quiet: %+v", res)
	}
	list, _ := e.app.ListJobBackups()
	var b JobBackup
	data, _ := os.ReadFile(list[0].Path)
	if len(list) != 1 || json.Unmarshal(data, &b) != nil || b.Jobs[0].TargetKind != "backup" {
		t.Errorf("%+v %s", list, data)
	}
}
