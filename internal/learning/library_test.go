package learning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type fakeTrash struct{ got []string }

func (f *fakeTrash) Trash(p string) (string, error) {
	f.got = append(f.got, p)
	dst := p + ".trashed"
	return dst, os.Rename(p, dst)
}

func writeSkill(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + filepath.Base(dir) + "\ndescription: Sort the downloads folder.\nlicense: MIT\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryLifecycle(t *testing.T) {
	tr := &fakeTrash{}
	clock := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	l := &Library{Dir: t.TempDir(), Trash: tr, Now: func() time.Time { clock = clock.Add(time.Minute); return clock }}

	d, err := l.NewDraftDir("organise-downloads")
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, d, "1. List the folder.\n")
	v, err := l.Publish(d, "Learned from run 01ABC.")
	if err != nil || v != "1.0.0" {
		t.Fatalf("publish: %v %v", v, err)
	}
	if ver, status, _ := readVersion(l.SkillDir("organise-downloads")); ver != "1.0.0" || status != "candidate" {
		t.Fatalf("version %q status %q", ver, status)
	}
	raw, _ := os.ReadFile(filepath.Join(l.SkillDir("organise-downloads"), "SKILL.md"))
	if !strings.Contains(string(raw), "license: MIT") || !strings.HasSuffix(string(raw), "---\n1. List the folder.\n") {
		t.Fatalf("front-matter or body lost:\n%s", raw)
	}
	if err := l.SetStatus("organise-downloads", "active", ""); err != nil {
		t.Fatal(err)
	}

	// A refinement waits in .next; the active version stays in place.
	d, _ = l.NewDraftDir("organise-downloads")
	writeSkill(t, d, "1. List the folder.\n2. Move archives too.\n")
	if v, err = l.Publish(d, "Handles .zip files."); err != nil || v != "1.1.0" {
		t.Fatalf("refine: %v %v", v, err)
	}
	if ver, _, _ := readVersion(l.SkillDir("organise-downloads")); ver != "1.0.0" {
		t.Fatalf("current changed before adoption: %s", ver)
	}
	vs, _ := l.Versions("organise-downloads")
	if len(vs) != 2 || !vs[0].Current || !vs[1].Next {
		t.Fatalf("versions %+v", vs)
	}
	diff, err := DiffDirs(vs[0].Dir, vs[1].Dir, "1.0.0", "1.1.0")
	if err != nil || !strings.Contains(diff, "+2. Move archives too.") || !strings.Contains(diff, "-  version: 1.0.0") {
		t.Fatalf("diff %v:\n%s", err, diff)
	}

	if v, err = l.AdoptNext("organise-downloads"); err != nil || v != "1.1.0" {
		t.Fatalf("adopt: %v %v", v, err)
	}
	ver, status, _ := readVersion(l.SkillDir("organise-downloads"))
	if ver != "1.1.0" || status != "active" {
		t.Fatalf("after adopt: %s %s", ver, status)
	}
	if _, err := os.Stat(l.NextDir("organise-downloads")); !os.IsNotExist(err) {
		t.Fatalf(".next still there: %v", err)
	}

	// Rolling back keeps 1.1.0 and the status; rolling forward again trashes only the duplicate.
	if err := l.Rollback("organise-downloads", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if ver, status, _ = readVersion(l.SkillDir("organise-downloads")); ver != "1.0.0" || status != "active" {
		t.Fatalf("after rollback: %s %s", ver, status)
	}
	if err := l.Rollback("organise-downloads", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if len(tr.got) != 1 || filepath.Base(tr.got[0]) != "SKILL.md" {
		t.Fatalf("trashed %v", tr.got)
	}
	vs, _ = l.Versions("organise-downloads")
	if len(vs) != 3 || vs[0].Version != "1.0.0" || vs[1].Version != "1.1.0" || !vs[2].Current {
		t.Fatalf("versions %+v", vs)
	}
	log, _ := os.ReadFile(filepath.Join(l.SkillDir("organise-downloads"), changelog))
	if !strings.Contains(string(log), "Handles .zip files.") || !strings.Contains(string(log), "Rolled back from 1.0.0.") {
		t.Fatalf("changelog:\n%s", log)
	}

	// A failed draft goes to .rejected with its reason.
	d, _ = l.NewDraftDir("bad-skill")
	writeSkill(t, d, "x")
	got, err := l.Reject(d, "tests failed")
	if err != nil || !strings.Contains(got, filepath.Join(rejectedDir, "bad-skill-")) {
		t.Fatalf("reject %q %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(got, "REJECTED.txt")); string(b) != "tests failed\n" {
		t.Fatalf("reason %q", b)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := splitLines("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n")
	b := splitLines("a\nB\nc\nd\ne\nf\ng\nh\ni\nJ\n")
	got := unified(a, b, 1)
	want := "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n@@ -9,2 +9,2 @@\n i\n-j\n+J\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func mapFS(files map[string][]byte) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: v}
	}
	return m
}
