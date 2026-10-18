package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"noskill/internal/perms"
	"noskill/internal/tools"
)

type trashRecorder struct{ dir string }

func (t trashRecorder) Trash(p string) (string, error) {
	to := filepath.Join(t.dir, filepath.Base(p))
	return to, os.Rename(p, to)
}

type fx struct {
	t             *testing.T
	root, outside string
	env           *tools.Env
}

func newFx(t *testing.T) *fx {
	f := &fx{t: t, root: t.TempDir(), outside: t.TempDir()}
	canon, _ := perms.Canonical(f.root)
	set := perms.Set{Files: []perms.Scope{{Access: perms.Write, Root: canon}}}
	f.env = &tools.Env{Perms: set, Expand: perms.Resolver{}.Expand,
		Broker: &perms.Broker{RunID: "r", Perms: set, Trash: trashRecorder{t.TempDir()}}}
	return f
}

func (f *fx) write(rel, s string) string {
	p := filepath.Join(f.root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func call(t *testing.T, id string, env *tools.Env, args any) (map[string]any, error) {
	t.Helper()
	var b tools.Builtin
	for _, x := range Files() {
		if x.ID == id {
			b = x
		}
	}
	raw, _ := json.Marshal(args)
	out, err := b.Run(context.Background(), env, raw)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil { // arrays
		return map[string]any{"raw": out}, nil
	}
	return m, nil
}

func TestListDir(t *testing.T) {
	f := newFx(t)
	f.write("a.mkv", "1234")
	f.write("b.txt", "x")
	f.write("sub/c.MKV", "xyz")
	f.write("sub/deep/d.txt", "d")
	if runtime.GOOS != "windows" {
		os.Symlink(f.outside, filepath.Join(f.root, "escape"))
	}
	m, err := call(t, "list_dir", f.env, map[string]any{"path": f.root})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range m["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string)+":"+e.(map[string]any)["type"].(string))
	}
	want := "a.mkv:file,b.txt:file,sub:folder"
	if runtime.GOOS != "windows" {
		want = "a.mkv:file,b.txt:file,escape:link,sub:folder" // a link is shown, never followed
	}
	if strings.Join(names, ",") != want {
		t.Errorf("flat = %v", names)
	}
	m, _ = call(t, "list_dir", f.env, map[string]any{"path": f.root, "glob": "*.mkv", "recursive": true})
	names = nil
	for _, e := range m["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "a.mkv,sub/c.MKV" {
		t.Errorf("recursive glob (case-insensitive) = %v", names)
	}
	m, _ = call(t, "list_dir", f.env, map[string]any{"path": f.root, "recursive": true, "max_entries": 2})
	if len(m["entries"].([]any)) != 2 || m["more"] != true {
		t.Errorf("cap: %v", m)
	}
	for _, p := range []string{f.outside, filepath.Join(f.root, "..", "x")} {
		if _, err := call(t, "list_dir", f.env, map[string]any{"path": p}); !perms.IsDenied(err) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := call(t, "list_dir", f.env, map[string]any{"path": f.root, "glob": "[a"}); err == nil {
		t.Error("bad glob accepted")
	}
}

func TestReadAndWriteText(t *testing.T) {
	f := newFx(t)
	p := f.write("notes.txt", "héllo wörld")
	m, err := call(t, "read_text", f.env, map[string]any{"path": p, "max_chars": 100})
	if err != nil || m["text"] != "héllo wörld" || m["truncated"] != false {
		t.Fatalf("%v %v", m, err)
	}
	m, _ = call(t, "read_text", f.env, map[string]any{"path": p, "max_chars": 5})
	if m["text"] != "héllo" || m["truncated"] != true {
		t.Errorf("cut: %v", m)
	}
	bin := f.write("blob.bin", "ab\x00cd")
	if _, err := call(t, "read_text", f.env, map[string]any{"path": bin, "max_chars": 100}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary: %v", err)
	}
	if _, err := call(t, "read_text", f.env, map[string]any{"path": f.root}); err == nil || !strings.Contains(err.Error(), "list_dir") {
		t.Errorf("folder: %v", err)
	}
	if _, err := call(t, "write_text", f.env, map[string]any{"path": filepath.Join(f.root, "new", "x.txt"), "content": "hi"}); err == nil {
		t.Log("(parent folders are created with mkdir, not by write_text)")
	}
	if _, err := call(t, "write_text", f.env, map[string]any{"path": filepath.Join(f.root, "x.txt"), "content": "hi"}); err != nil {
		t.Fatal(err)
	}
	_, err = call(t, "write_text", f.env, map[string]any{"path": filepath.Join(f.root, "x.txt"), "content": "again"})
	if err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Errorf("overwrite: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "x.txt")); string(got) != "hi" {
		t.Errorf("file changed: %q", got)
	}
	if _, err := call(t, "write_text", f.env, map[string]any{"path": filepath.Join(f.outside, "x.txt"), "content": "no"}); !perms.IsDenied(err) {
		t.Errorf("outside: %v", err)
	}
}

func TestMoveManyDryRunThenApply(t *testing.T) {
	f := newFx(t)
	a := f.write("in/a.mkv", "A")
	b := f.write("in/b.mkv", "B")
	f.write("out/taken.mkv", "old")
	plan := []map[string]string{
		{"from": a, "to": filepath.Join(f.root, "out", "Show - 01.mkv")},
		{"from": b, "to": filepath.Join(f.root, "out", "taken.mkv")},                                    // exists
		{"from": filepath.Join(f.root, "in", "ghost.mkv"), "to": filepath.Join(f.root, "out", "g.mkv")}, // no source
		{"from": a, "to": filepath.Join(f.root, "out", "Show - 01.mkv")},                                // same target twice
		{"from": b, "to": filepath.Join(f.outside, "b.mkv")},                                            // out of scope
	}
	m, err := call(t, "move_many", f.env, map[string]any{"plan": plan, "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	sum := m["summary"].(map[string]any)
	if sum["would_move"] != 1.0 || sum["skipped"] != 4.0 {
		t.Fatalf("summary = %v", sum)
	}
	reasons := ""
	for _, it := range m["items"].([]any) {
		r, _ := it.(map[string]any)["reason"].(string)
		reasons += r + "|"
	}
	for _, want := range []string{"target already exists", "source doesn't exist", "same target as item 1", "permission denied"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("missing reason %q in %s", want, reasons)
		}
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal("a dry run moved a file")
	}
	m, _ = call(t, "move_many", f.env, map[string]any{"plan": plan, "dry_run": false})
	if m["summary"].(map[string]any)["moved"] != 1.0 {
		t.Errorf("apply: %v", m["summary"])
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "out", "Show - 01.mkv")); string(got) != "A" {
		t.Errorf("moved file: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(f.root, "out", "taken.mkv")); string(got) != "old" {
		t.Errorf("existing file overwritten: %q", got)
	}
	if _, err := os.Stat(b); err != nil {
		t.Error("a skipped item was moved anyway")
	}
}

func TestMoveManyRefusesTargetInsideMovedFolder(t *testing.T) {
	f := newFx(t)
	f.write("src/x.txt", "x")
	f.write("other/y.txt", "y")
	plan := []map[string]string{
		{"from": filepath.Join(f.root, "src"), "to": filepath.Join(f.root, "dst")},
		{"from": filepath.Join(f.root, "other", "y.txt"), "to": filepath.Join(f.root, "src", "y.txt")},
	}
	m, _ := call(t, "move_many", f.env, map[string]any{"plan": plan, "dry_run": true})
	items := m["items"].([]any)
	if reason, _ := items[1].(map[string]any)["reason"].(string); items[1].(map[string]any)["status"] != "skipped" || !strings.Contains(reason, "inside a folder") {
		t.Errorf("%v", items[1])
	}
}

func TestCopyVerifyMoveTrash(t *testing.T) {
	f := newFx(t)
	src := f.write("a.txt", "payload")
	dst := filepath.Join(f.root, "copy.txt")
	if _, err := call(t, "copy_verify", f.env, map[string]any{"from": src, "to": dst}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "payload" {
		t.Errorf("copy = %q", got)
	}
	if _, err := os.Stat(dst + ".part"); err == nil {
		t.Error("a .part file was left behind")
	}
	if _, err := call(t, "copy_verify", f.env, map[string]any{"from": src, "to": dst}); err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Errorf("copy over existing: %v", err)
	}
	if _, err := call(t, "move", f.env, map[string]any{"from": dst, "to": filepath.Join(f.root, "moved.txt")}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, "trash", f.env, map[string]any{"path": filepath.Join(f.root, "moved.txt")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "moved.txt")); err == nil {
		t.Error("trashed file still there")
	}
	if _, err := call(t, "trash", f.env, map[string]any{"path": filepath.Join(f.outside, "x")}); !perms.IsDenied(err) {
		t.Errorf("trash outside: %v", err)
	}
	if _, err := call(t, "move", f.env, map[string]any{"from": src, "to": "relative/path"}); err == nil {
		t.Error("relative target accepted")
	}
}

func TestSanitizeName(t *testing.T) {
	for _, tc := range []struct{ in, fs, want string }{
		{`Show: Season 2 / Ep 1?.mkv`, "auto", `Show - Season 2 - Ep 1_.mkv`},
		{`Show: Season 2.mkv`, "apfs", `Show: Season 2.mkv`},
		{`a/b`, "apfs", `a-b`},
		{"tab\there\x00.txt", "ntfs", "tabhere-.txt"},
		{"trailing dots...", "ntfs", "trailing dots"},
		{"CON.txt", "fat32", "_CON.txt"},
		{"nul", "auto", "_nul"},
		{"  lead", "auto", "lead"},
		{"..", "auto", "_"},
		{"", "auto", "_"},
		{strings.Repeat("é", 200) + ".mkv", "auto", ""},
	} {
		got := SanitizeName(tc.in, tc.fs)
		if tc.want == "" { // length case
			if len(got) > 255 || !strings.HasSuffix(got, ".mkv") || !strings.HasPrefix(got, "é") {
				t.Errorf("long name = %d bytes %q", len(got), got[:20])
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%q (%s) = %q, want %q", tc.in, tc.fs, got, tc.want)
		}
	}
}
