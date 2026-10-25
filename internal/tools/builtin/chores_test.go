package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/perms"
	"noskill/internal/tools"
)

func callChores(t *testing.T, id string, env *tools.Env, args any) (map[string]any, error) {
	t.Helper()
	for _, b := range Chores() {
		if b.ID == id {
			raw, _ := json.Marshal(args)
			out, err := b.Run(context.Background(), env, raw)
			if err != nil {
				return nil, err
			}
			var m map[string]any
			json.Unmarshal([]byte(out), &m)
			return m, nil
		}
	}
	t.Fatalf("no tool %s", id)
	return nil, nil
}

func (f *fx) age(p string, d time.Duration) {
	mt := time.Now().Add(-d)
	os.Chtimes(p, mt, mt)
}

func TestTidyPlanThenApply(t *testing.T) {
	f := newFx(t)
	pdf := f.write("report.pdf", "pdf")
	f.age(pdf, 72*time.Hour)
	fresh := f.write("fresh.pdf", "new")
	old := f.write("setup.dmg", "installer")
	f.age(old, 60*24*time.Hour)

	out, err := callChores(t, "tidy_plan", f.env, map[string]any{"folder": f.root})
	if err != nil {
		t.Fatal(err)
	}
	if out["moves"] != float64(1) || out["old_installers_to_trash"] != float64(1) {
		t.Fatalf("plan = %v", out)
	}
	if _, err := os.Stat(pdf); err != nil {
		t.Fatal("tidy_plan must change nothing")
	}
	res, err := callChores(t, "tidy_apply", f.env, map[string]any{"folder": f.root})
	if err != nil {
		t.Fatal(err)
	}
	if res["moved"] != float64(1) || len(res["trashed"].([]any)) != 1 || len(res["skipped"].([]any)) != 0 {
		t.Fatalf("apply = %v", res)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Sorted", "Documents", "report.pdf")); err != nil {
		t.Error("the file didn't move")
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old installer is still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh.pdf must stay")
	}
}

func TestDupesTrashChecksTheContentAgain(t *testing.T) {
	f := newFx(t)
	body := strings.Repeat("x", 2<<20)
	keep := f.write("keep/film.mkv", body)
	same := f.write("copy/film.mkv", body)
	differs := f.write("copy/other.mkv", strings.Repeat("y", 2<<20))

	out, err := callChores(t, "dupes_find", f.env, map[string]any{"folders": []string{f.root}})
	if err != nil {
		t.Fatal(err)
	}
	if groups := out["groups"].([]any); len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	res, err := callChores(t, "dupes_trash", f.env, map[string]any{"keep": keep, "paths": []string{same, differs, keep}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res["trashed"].([]any); len(got) != 1 || filepath.Base(got[0].(string)) != "film.mkv" || !strings.Contains(got[0].(string), "copy") {
		t.Errorf("trashed = %v", got)
	}
	if got := res["refused"].([]any); len(got) != 2 {
		t.Errorf("refused = %v (different content and the keeper itself)", got)
	}
	for _, p := range []string{keep, differs} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must stay", p)
		}
	}
}

func TestToolsRefuseOutsideTheirScope(t *testing.T) {
	f := newFx(t)
	for id, args := range map[string]any{
		"tidy_plan":   map[string]any{"folder": f.outside},
		"dupes_find":  map[string]any{"folders": []string{f.outside}},
		"rename_plan": map[string]any{"folder": f.outside},
		"disk_report": map[string]any{"path": f.outside},
	} {
		if _, err := callChores(t, id, f.env, args); err == nil {
			t.Errorf("%s read a folder outside the granted scope", id)
		}
	}
}

func TestRenamePlanTool(t *testing.T) {
	f := newFx(t)
	f.write("[Grp] Show Name - 01 (480p) [AB12CD34].mkv", "a")
	f.write("[Grp] Show Name - 02 (480p) [AB12CD35].mkv", "b")
	out, err := callChores(t, "rename_plan", f.env, map[string]any{"folder": f.root, "show": "Show Name"})
	if err != nil {
		t.Fatal(err)
	}
	if out["renames"] != float64(2) {
		t.Fatalf("plan = %v", out)
	}
	res, err := callChores(t, "rename_apply", f.env, map[string]any{"folder": f.root, "show": "Show Name"})
	if err != nil || res["renamed"] != float64(2) {
		t.Fatalf("apply = %v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Show Name Episode 1 [480p].mkv")); err != nil {
		t.Error("the file wasn't renamed")
	}
	// A second apply finds nothing left to do.
	if res, _ := callChores(t, "rename_apply", f.env, map[string]any{"folder": f.root, "show": "Show Name"}); res["renamed"] != float64(0) {
		t.Errorf("second apply = %v", res)
	}
}

type fakeChores struct {
	tools.Services
	added []string
}

func (c *fakeChores) Brief(context.Context, int) (string, error) { return "brief", nil }
func (c *fakeChores) AddReminder(_ context.Context, text string) (string, error) {
	c.added = append(c.added, text)
	return `{"name":"x"}`, nil
}
func (c *fakeChores) ListReminders(context.Context) (string, error) { return "[]", nil }
func (c *fakeChores) RemoveReminder(context.Context, string) error  { return nil }

func TestReminderToolsNeedTheService(t *testing.T) {
	f := newFx(t)
	if _, err := callChores(t, "reminder_add", f.env, map[string]any{"text": "call Sam tomorrow at 5pm"}); err == nil {
		t.Error("without the service the tool should say it isn't available")
	}
	svc := &fakeChores{}
	f.env.Svc = svc
	if _, err := callChores(t, "reminder_add", f.env, map[string]any{"text": "call Sam tomorrow at 5pm"}); err != nil || len(svc.added) != 1 {
		t.Errorf("added = %v err = %v", svc.added, err)
	}
}

// Looking at a folder takes read access; only applying a plan needs write.
func TestPlanToolsNeedOnlyReadAccess(t *testing.T) {
	f := newFx(t)
	f.write("report.pdf", "pdf")
	f.write("[Grp] Show - 01 (480p) [AB12CD31].mkv", "a")
	canon, _ := perms.Canonical(f.root)
	f.env.Perms = perms.Set{Files: []perms.Scope{{Access: perms.Read, Root: canon}}}
	f.env.Broker = &perms.Broker{RunID: "r", Perms: f.env.Perms, Trash: trashRecorder{t.TempDir()}}
	for id, args := range map[string]any{
		"tidy_plan":   map[string]any{"folder": f.root},
		"rename_plan": map[string]any{"folder": f.root, "show": "Show"},
	} {
		if _, err := callChores(t, id, f.env, args); err != nil {
			t.Errorf("%s with read access: %v", id, err)
		}
	}
	for id, args := range map[string]any{
		"tidy_apply":   map[string]any{"folder": f.root},
		"rename_apply": map[string]any{"folder": f.root, "show": "Show"},
	} {
		if _, err := callChores(t, id, f.env, args); err == nil {
			t.Errorf("%s changed files with read-only access", id)
		}
	}
}
