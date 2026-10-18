package perms

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"noskill/internal/platform"
	"noskill/internal/store"
)

func mkfile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tree: root/allowed/{a.txt}, root/secret/key, root/allowed/link → root/secret
func tree(t *testing.T) (root string, s Set) {
	root = t.TempDir()
	mkfile(t, filepath.Join(root, "allowed", "a.txt"), "a")
	mkfile(t, filepath.Join(root, "secret", "key"), "k")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "secret"), filepath.Join(root, "allowed", "link")); err != nil {
			t.Fatal(err)
		}
		os.Symlink(filepath.Join(root, "nowhere", "x"), filepath.Join(root, "allowed", "dangling"))
	}
	sc, err := Resolver{}.Compile([]string{"write:" + filepath.Join(root, "allowed")})
	if err != nil {
		t.Fatal(err)
	}
	return root, Set{Files: sc}
}

func TestPathEscapes(t *testing.T) {
	root, s := tree(t)
	if _, err := s.CheckRead(filepath.Join(root, "allowed", "a.txt")); err != nil {
		t.Fatalf("allowed read refused: %v", err)
	}
	if _, err := s.CheckWrite(filepath.Join(root, "allowed", "new", "b.txt")); err != nil {
		t.Fatalf("new file inside scope refused: %v", err)
	}
	escapes := []string{
		filepath.Join(root, "secret", "key"),
		filepath.Join(root, "allowed", "..", "secret", "key"),
		filepath.Join(root, "allowedx", "f"), // prefix without a separator
		root,
	}
	if runtime.GOOS != "windows" {
		escapes = append(escapes,
			filepath.Join(root, "allowed", "link", "key"),
			filepath.Join(root, "allowed", "dangling"),
		)
	}
	for _, p := range escapes {
		if _, err := s.CheckRead(p); !IsDenied(err) {
			t.Errorf("%s: err = %v, want denied", p, err)
		}
	}
}

func TestCaseVariants(t *testing.T) {
	if !platform.CaseInsensitive() {
		t.Skip("case-sensitive filesystem")
	}
	root, s := tree(t)
	variant := filepath.Join(root, "ALLOWED", "A.TXT")
	if _, err := s.CheckRead(variant); err != nil {
		t.Errorf("same folder in other case refused: %v", err)
	}
	own := filepath.Join(root, "allowed")
	if _, err := ValidateApproved(filepath.Join(root, "ALLOWED"), []string{own}); err == nil || !strings.Contains(err.Error(), "noskill's own") {
		t.Errorf("case variant of noskill's folder approved: %v", err)
	}
}

func TestWindowsForms(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	for _, p := range []string{`\\server\share\x`, `\\?\C:\x`, `C:\x\file.txt:stream`, `//server/share`} {
		if _, err := Canonical(p); !IsDenied(err) {
			t.Errorf("%s accepted", p)
		}
	}
}

type fakeVolumes struct{ mount, uuid string }

func (f fakeVolumes) Resolve(label, uuid string) (string, string, error) {
	if label != "RED" {
		return "", "", platform.ErrVolumeNotMounted
	}
	return f.mount, f.uuid, nil
}

func TestExpandTokens(t *testing.T) {
	dir := t.TempDir()
	r := Resolver{
		Config: func(k string) (string, bool, bool) {
			switch k {
			case "dest":
				return dir, false, true
			case "evil":
				return "/", true, true
			}
			return "", false, false
		},
		Approved:    map[string]string{"save": filepath.Join(dir, "picked")},
		Volumes:     fakeVolumes{mount: dir, uuid: "AAAA"},
		VolumeUUIDs: map[string]string{"RED": "aaaa"},
	}
	for in, want := range map[string]string{
		"${config.dest}/sub":   filepath.Join(dir, "sub"),
		"${approved.save}":     filepath.Join(dir, "picked"),
		"volume:RED/ANIME2026": filepath.Join(dir, "ANIME2026"),
	} {
		if got, err := r.Expand(in); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", in, got, err, want)
		}
	}
	if h, err := r.Expand("~/x"); err != nil || !filepath.IsAbs(h) {
		t.Errorf("~ = %q, %v", h, err)
	}
	for _, bad := range []string{"${config.evil}", "${config.unset}", "${approved.other}", "$HOME/x", "relative/path", "volume:BLUE/x"} {
		if got, err := r.Expand(bad); err == nil {
			t.Errorf("%s accepted as %q", bad, got)
		}
	}
	r.VolumeUUIDs["RED"] = "BBBB" // same label, different drive
	if _, err := r.Expand("volume:RED/x"); !errors.Is(err, platform.ErrVolumeNotMounted) {
		t.Errorf("spoofed volume: %v", err)
	}
	if _, err := r.Compile([]string{"exec:/bin"}); err == nil {
		t.Error("bad access accepted")
	}
}

func TestNarrow(t *testing.T) {
	dir := t.TempDir()
	owner := Set{Files: []Scope{{Access: Write, Root: dir}}, Network: []string{"*"}, Exec: true}
	caller := Set{Files: []Scope{{Access: Write, Root: filepath.Join(dir, "sub")}, {Access: Write, Root: "/elsewhere"}},
		Network: []string{"api.example.com"}}
	n := Narrow(owner, caller)
	if len(n.Files) != 1 || n.Files[0].Root != filepath.Join(dir, "sub") || n.Exec {
		t.Fatalf("files = %+v exec=%v", n.Files, n.Exec)
	}
	if n.CheckHost("api.example.com") != nil || n.CheckHost("evil.example.com") == nil {
		t.Errorf("network = %v", n.Network)
	}
	// Read-only owner, write caller: read only.
	n = Narrow(Set{Files: []Scope{{Access: Read, Root: dir}}}, Set{Files: []Scope{{Access: Write, Root: dir}}})
	if n.Files[0].Access != Read {
		t.Errorf("widened to %s", n.Files[0].Access)
	}
}

func TestCheckHost(t *testing.T) {
	s := Set{Network: []string{"graphql.anilist.co", "*.github.com", "localhost"}}
	for h, ok := range map[string]bool{
		"graphql.anilist.co":  true,
		"GraphQL.AniList.co.": true,
		"api.github.com":      true,
		"github.com":          false, // *.x is subdomains only
		"evilgithub.com":      false,
		"localhost":           true,
		"127.0.0.1":           true,
		"[::1]":               true,
		"10.0.0.5":            false,
		"anilist.co":          false,
	} {
		if err := s.CheckHost(h); (err == nil) != ok {
			t.Errorf("%s: err = %v", h, err)
		}
	}
	any := Set{Network: []string{"*"}}
	for h, ok := range map[string]bool{"example.com": true, "localhost": false, "x.localhost": false, "93.184.216.34": false} {
		if err := any.CheckHost(h); (err == nil) != ok {
			t.Errorf("* %s: err = %v", h, err)
		}
	}
}

func TestValidateApproved(t *testing.T) {
	home, _ := os.UserHomeDir()
	own := t.TempDir()
	for _, p := range []string{home, "/", filepath.Join(own, "x"), "relative", platform.SystemDirs()[0]} {
		if _, err := ValidateApproved(p, []string{own}); err == nil {
			t.Errorf("%s approved", p)
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := ValidateApproved("/Volumes/RED", nil); !IsDenied(err) {
			t.Errorf("drive root approved: %v", err)
		}
	}
}

type memUndo struct{ e []store.UndoEntry }

func (m *memUndo) AppendUndo(_ context.Context, e store.UndoEntry) error {
	m.e = append(m.e, e)
	return nil
}

type fakeTrash struct{ dir string }

func (f fakeTrash) Trash(p string) (string, error) {
	to := filepath.Join(f.dir, filepath.Base(p))
	return to, os.Rename(p, to)
}

func TestBrokerOps(t *testing.T) {
	root, s := tree(t)
	undo := &memUndo{}
	b := &Broker{RunID: "r1", Perms: s, Undo: undo, Trash: fakeTrash{t.TempDir()}}
	ctx := context.Background()
	a := filepath.Join(root, "allowed")

	if err := b.Mkdir(ctx, filepath.Join(a, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, filepath.Join(a, "x", "y", "n.txt"), []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, filepath.Join(a, "a.txt"), []byte("clobber")); !errors.Is(err, ErrExists) {
		t.Errorf("overwrite: %v", err)
	}
	if err := b.Copy(ctx, filepath.Join(a, "a.txt"), filepath.Join(a, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := b.Move(ctx, filepath.Join(a, "c.txt"), filepath.Join(a, "a.txt")); !errors.Is(err, ErrExists) {
		t.Errorf("move over existing: %v", err)
	}
	if err := b.Move(ctx, filepath.Join(a, "c.txt"), filepath.Join(a, "x", "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := b.Move(ctx, filepath.Join(a, "a.txt"), filepath.Join(root, "secret", "a.txt")); !IsDenied(err) {
		t.Errorf("move out of scope: %v", err)
	}
	if err := b.Copy(ctx, filepath.Join(root, "secret", "key"), filepath.Join(a, "key")); !IsDenied(err) {
		t.Errorf("copy from outside: %v", err)
	}
	if err := b.TrashPath(ctx, filepath.Join(a, "x", "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(ctx, filepath.Join(root, "outside", "d")); !IsDenied(err) {
		t.Errorf("mkdir outside: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(a, "a.txt")); string(b) != "a" {
		t.Errorf("a.txt changed: %q", b)
	}
	var ops []string
	for _, e := range undo.e {
		ops = append(ops, e.Op)
	}
	if got := strings.Join(ops, ","); got != "mkdir,mkdir,create,copy,move,trash" {
		t.Errorf("undo ops = %s", got)
	}
}
