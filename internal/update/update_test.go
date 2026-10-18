package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tarGz(name string, content []byte) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "noskill_x/" + name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return b.Bytes()
}

type fixture struct {
	srv *httptest.Server
	pub string
	bin []byte
}

// newRelease serves a fake GitHub release v9.9.9 for this platform, signed with a fresh key.
func newRelease(t *testing.T, tamper func(archive, sums, sig *[]byte)) *fixture {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	script := []byte("#!/bin/sh\necho noskill 9.9.9\n")
	name := ArchiveName("9.9.9", runtime.GOOS, runtime.GOARCH)
	bin := "noskill"
	if runtime.GOOS == "windows" {
		bin = "noskill.exe"
	}
	archive := tarGz(bin, script)
	h := sha256.Sum256(archive)
	sums := []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(h[:]), name))
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)))
	if tamper != nil {
		tamper(&archive, &sums, &sig)
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/n/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":%q,"browser_download_url":"%s/a"},{"name":"checksums.txt","browser_download_url":"%s/s"},{"name":"checksums.txt.ed25519","browser_download_url":"%s/g"}]}`, name, srv.URL, srv.URL, srv.URL)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/s", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	mux.HandleFunc("/g", func(w http.ResponseWriter, r *http.Request) { w.Write(sig) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, pub: base64.StdEncoding.EncodeToString(pub), bin: script}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"0.1.0", "0.2.0", -1}, {"v1.0.0", "1.0.0", 0}, {"0.10.0", "0.9.9", 1}, {"0.2.0-beta.1", "0.2.0", -1}, {"1.0.0", "1.0.0-rc1", 1}, {"dev", "0.1.0", -1}} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d", c.a, c.b, got)
		}
	}
}

func TestFetchVerifiesBeforeTrusting(t *testing.T) {
	ctx := context.Background()
	old := PublicKey
	defer func() { PublicKey = old }()
	f := newRelease(t, nil)
	c := &Client{Repo: "o/n", API: f.srv.URL}
	rel, err := c.Latest(ctx, "stable")
	if err != nil || rel.Version != "9.9.9" {
		t.Fatalf("%+v %v", rel, err)
	}
	dir := t.TempDir()
	PublicKey = ""
	if _, err := c.Fetch(ctx, rel, dir); err == nil || !strings.Contains(err.Error(), "no release key") {
		t.Errorf("a build without a key must refuse: %v", err)
	}
	PublicKey = f.pub
	p, err := c.Fetch(ctx, rel, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, f.bin) {
		t.Errorf("extracted %q", b)
	}
	// Tampering with any of the three parts is refused, and nothing is written.
	for name, tamper := range map[string]func(a, s, g *[]byte){
		"archive":   func(a, s, g *[]byte) { *a = append([]byte{}, *a...); (*a)[len(*a)-9] ^= 0xff },
		"checksums": func(a, s, g *[]byte) { *s = []byte(strings.Replace(string(*s), "0", "1", 1)) },
		"signature": func(a, s, g *[]byte) { *g = []byte(base64.StdEncoding.EncodeToString(make([]byte, 64))) },
	} {
		ft := newRelease(t, tamper)
		PublicKey = ft.pub
		d := t.TempDir()
		ct := &Client{Repo: "o/n", API: ft.srv.URL}
		r, _ := ct.Latest(ctx, "stable")
		if _, err := ct.Fetch(ctx, r, d); err == nil {
			t.Errorf("tampered %s accepted", name)
		}
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("tampered %s left files: %v", name, ents)
		}
	}
	// A different key (an attacker's release) is refused.
	other := newRelease(t, nil)
	PublicKey = f.pub
	co := &Client{Repo: "o/n", API: other.srv.URL}
	r, _ := co.Latest(ctx, "stable")
	if _, err := co.Fetch(ctx, r, t.TempDir()); err == nil {
		t.Error("a release signed with another key was accepted")
	}
	if _, err := (&Client{}).Latest(ctx, "stable"); err == nil || !strings.Contains(err.Error(), "update.repo") {
		t.Errorf("no repo: %v", err)
	}
}

func TestSwapSmokeTestAndRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the binary")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "noskill")
	os.WriteFile(exe, []byte("#!/bin/sh\necho noskill 1.0.0\n"), 0o755)
	nb := filepath.Join(dir, ".noskill.new")
	os.WriteFile(nb, []byte("#!/bin/sh\necho noskill 9.9.9\n"), 0o755)
	ctx := context.Background()
	if err := SmokeTest(ctx, nb, "9.9.9"); err != nil {
		t.Fatal(err)
	}
	if err := SmokeTest(ctx, nb, "8.0.0"); err == nil {
		t.Error("wrong version passed the smoke test")
	}
	os.WriteFile(filepath.Join(dir, "broken"), []byte("not a program"), 0o755)
	if err := SmokeTest(ctx, filepath.Join(dir, "broken"), "9.9.9"); err == nil {
		t.Error("a broken binary passed the smoke test")
	}
	backup, err := Swap(exe, nb)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "9.9.9") {
		t.Error("new binary not in place")
	}
	if err := Rollback(exe, backup); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "1.0.0") {
		t.Error("rollback didn't restore the old binary")
	}
}

func TestInstalledViaAndRestoreDB(t *testing.T) {
	for p, want := range map[string]string{"/opt/homebrew/bin/noskill": "Homebrew", "/opt/homebrew/Cellar/noskill/0.1/bin/noskill": "Homebrew",
		`C:\Users\me\scoop\apps\noskill\current\noskill.exe`: "Scoop", "/home/me/.local/bin/noskill": ""} {
		if m, _ := InstalledVia(p); m != want {
			t.Errorf("%s → %q", p, m)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.db"), []byte("migrated"), 0o600)
	os.WriteFile(filepath.Join(dir, "state.db.bak-1"), []byte("before"), 0o600)
	trash := t.TempDir()
	err := RestoreDB(dir, 1, func(p string) (string, error) {
		to := filepath.Join(trash, filepath.Base(p))
		return to, os.Rename(p, to)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state.db")); string(b) != "before" {
		t.Errorf("db = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(trash, "state.db")); string(b) != "migrated" {
		t.Error("the migrated database should be in the trash, not deleted")
	}
	if err := RestoreDB(dir, 7, nil); err == nil {
		t.Error("restoring a backup that doesn't exist")
	}
}
