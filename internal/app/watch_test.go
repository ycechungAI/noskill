package app

import (
	"context"
	"strings"
	"testing"

	"noskill/internal/backendtest"
	"noskill/internal/scheduler"
	"noskill/internal/watch"
)

// skills/watcher: each watch is a scheduler job, and removing the watch removes the job.
func TestWatchIsAScheduledJob(t *testing.T) {
	ctx := context.Background()
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	svc := &runServices{a: e.app}
	w, err := svc.WatchAdd(ctx, watch.Watch{Name: "ollama", Kind: watch.GitHub, Target: "ollama/ollama", Schedule: "every:6h", Topic: "releases"})
	if err != nil || w.JobID == "" {
		t.Fatalf("%+v %v", w, err)
	}
	st := &scheduler.Store{DB: e.app.DB}
	j, err := st.Get(ctx, "watch: ollama")
	if err != nil || j.TargetKind != "skill" || j.Target != "watcher" || j.Schedule != "every:6h0m0s" && j.Schedule != "every:6h" ||
		!strings.Contains(j.Args.Prompt, `"ollama"`) || !j.Enabled || j.NextRunAt.IsZero() {
		t.Fatalf("job %+v %v", j, err)
	}
	got, err := svc.WatchGet(ctx, "ollama")
	if err != nil || got.JobID != j.ID || got.Topic != "releases" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := svc.WatchAdd(ctx, watch.Watch{Name: "ollama", Kind: watch.GitHub, Target: "ollama/ollama", Schedule: "every:6h"}); err == nil {
		t.Error("a second watch with the same name was accepted")
	}
	if _, err := svc.WatchAdd(ctx, watch.Watch{Name: "bad", Kind: watch.Page, Target: "https://x.test", Schedule: "nope"}); err == nil {
		t.Error("a bad schedule was accepted")
	}
	if ws, _ := svc.WatchList(ctx); len(ws) != 1 {
		t.Errorf("a failed add left a watch behind: %+v", ws)
	}
	fp := watch.Fingerprint("a")
	svc.WatchRecord(ctx, "ollama", []watch.Item{{Fingerprint: fp, Title: "v1"}}, `"e1"`, "")
	if seen, _ := svc.WatchSeen(ctx, "ollama"); !seen[fp] {
		t.Error("an item wasn't remembered")
	}
	if g, _ := svc.WatchGet(ctx, "ollama"); g.ETag != `"e1"` || g.LastChecked.IsZero() {
		t.Errorf("validators %+v", g)
	}
	if err := svc.WatchRemove(ctx, "ollama"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "watch: ollama"); err == nil {
		t.Error("the job survived")
	}
	if _, err := svc.WatchGet(ctx, "ollama"); err == nil {
		t.Error("the watch survived")
	}
}
