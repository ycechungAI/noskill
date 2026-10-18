package registry

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func skillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		p = filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", "---\nname: notes\ndescription: Keeps notes.\n---\nWrite notes.\n")
	write("scripts/add.star", "print(1)\n")
	write(".git/HEAD", "ref: x\n")
	return dir
}

// A signature covers every file but .git and itself; changing, adding or removing a file breaks it,
// and so does a different name or version (FR-S10, ADR-0016).
func TestSignAndVerify(t *testing.T) {
	priv := key(t)
	dir := skillDir(t)
	m, err := BuildManifest(dir, "notes", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files["scripts/add.star"] == "" {
		t.Fatalf("%v", m.Files)
	}
	text := string(m.Text())
	if !strings.HasPrefix(text, "noskill-skill-v1\nname: notes\nversion: 1.0\n") || strings.Index(text, "SKILL.md") > strings.Index(text, "scripts/") {
		t.Errorf("manifest:\n%s", text)
	}
	s := Sign(m, priv, time.Now())
	if err := os.WriteFile(filepath.Join(dir, SigFile), MarshalSignature(s), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadSignature(dir)
	if err != nil || !ok || got.KeyID != KeyID(priv.Public().(ed25519.PublicKey)) || len(got.KeyID) != 16 {
		t.Fatalf("%v %v %+v", err, ok, got)
	}
	if err := Verify(dir, got, "notes", "1.0"); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := Verify(dir, got, "notes", "1.1"); err == nil || !strings.Contains(err.Error(), "signed as notes 1.0") {
		t.Errorf("version: %v", err)
	}

	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("other"), 0o600)
	if err := Verify(dir, got, "notes", "1.0"); err != nil {
		t.Errorf(".git counted: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "scripts", "add.star"), []byte("print(2)\n"), 0o600)
	var ce *ChangedError
	if err := Verify(dir, got, "notes", "1.0"); !errors.As(err, &ce) || strings.Join(ce.Files, ",") != "scripts/add.star" {
		t.Errorf("edited: %v", err)
	}
	now, _ := BuildManifest(dir, "notes", "1.0")
	if c := Changed(m, now); len(c) != 1 || c[0] != "scripts/add.star" {
		t.Errorf("changed: %v", c)
	}
	os.WriteFile(filepath.Join(dir, "scripts", "add.star"), []byte("print(1)\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "extra.md"), []byte("x"), 0o600)
	if err := Verify(dir, got, "notes", "1.0"); !errors.As(err, &ce) || ce.Files[0] != "extra.md" {
		t.Errorf("an added file: %v", err)
	}
	listed := got
	listed.Files = map[string]string{"SKILL.md": got.Files["SKILL.md"]} // a file list cut short to hide one
	if err := Verify(dir, listed, "notes", "1.0"); err == nil || !strings.Contains(err.Error(), "file list") {
		t.Errorf("edited list: %v", err)
	}
	os.Remove(filepath.Join(dir, "extra.md"))

	forged := got
	forged.Key = EncodePublicKey(key(t).Public().(ed25519.PublicKey))
	if err := Verify(dir, forged, "notes", "1.0"); err == nil || !strings.Contains(err.Error(), "key ID") {
		t.Errorf("swapped key: %v", err)
	}
	forged.KeyID = KeyID(must(ParsePublicKey(forged.Key)))
	if err := Verify(dir, forged, "notes", "1.0"); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("swapped key and ID: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestManifestRefusesLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	dir := skillDir(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := BuildManifest(dir, "notes", "1.0"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("%v", err)
	}
}

func TestIndexSignVerifySearch(t *testing.T) {
	priv := key(t)
	pub := priv.Public().(ed25519.PublicKey)
	ix := Index{Name: "mine", Made: time.Now(), Entries: []Entry{
		{Name: "weather", Version: "1", Description: "Forecasts for a city", Path: "weather", Digest: "sha256:aa"},
		{Name: "notes", Version: "2", Description: "Keeps notes in a folder", Path: "skills/notes", Digest: "sha256:bb"},
	}}
	data, sig, err := SignIndex(ix, priv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyIndex(data, sig, pub)
	if err != nil || got.Entries[0].Name != "notes" || got.KeyID != KeyID(pub) {
		t.Fatalf("%v %+v", err, got)
	}
	if e, ok := got.Find("weather"); !ok || e.Digest != "sha256:aa" {
		t.Errorf("find: %+v", e)
	}
	if r := got.Search([]string{"NOTES", "folder"}); len(r) != 1 || r[0].Name != "notes" {
		t.Errorf("search: %+v", r)
	}
	if _, err := VerifyIndex(append([]byte(" "), data...), sig, pub); err == nil {
		t.Error("a changed index passed")
	}
	if _, err := VerifyIndex(data, sig, key(t).Public().(ed25519.PublicKey)); err == nil {
		t.Error("another key passed")
	}
	for _, p := range []string{"../x", "/etc", "a/../../b", `a\b`, "."} {
		bad := ix
		bad.Entries = []Entry{{Name: "x", Path: p}}
		d, s, _ := SignIndex(bad, priv)
		if _, err := VerifyIndex(d, s, pub); err == nil || !strings.Contains(err.Error(), "leaves the registry") {
			t.Errorf("%q: %v", p, err)
		}
	}
}
