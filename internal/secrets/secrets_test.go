package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newMgr(t *testing.T, env map[string]string) (*Manager, *Mem) {
	k := &Mem{}
	m := NewManager(k, filepath.Join(t.TempDir(), "secrets.names"))
	m.Environ = func(n string) (string, bool) { v, ok := env[n]; return v, ok }
	return m, k
}

func TestSetGetListDelete(t *testing.T) {
	m, k := newMgr(t, nil)
	if err := m.Set("BRAVE_API_KEY", "abc123"); err != nil {
		t.Fatal(err)
	}
	m.Set("NTFY_TOKEN", "tk")
	if v, ok := m.Get("BRAVE_API_KEY"); !ok || v != "abc123" {
		t.Errorf("%q %v", v, ok)
	}
	names, _ := m.List()
	if strings.Join(names, ",") != "BRAVE_API_KEY,NTFY_TOKEN" {
		t.Errorf("names = %v", names)
	}
	b, _ := os.ReadFile(m.IndexPath)
	if strings.Contains(string(b), "abc123") {
		t.Fatal("a secret value reached the index file")
	}
	if fi, _ := os.Stat(m.IndexPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("index mode = %v", fi.Mode())
	}
	if err := m.Delete("NTFY_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("NTFY_TOKEN"); ok {
		t.Error("deleted secret still readable")
	}
	if names, _ := m.List(); strings.Join(names, ",") != "BRAVE_API_KEY" {
		t.Errorf("after delete: %v", names)
	}
	if err := m.Delete("NTFY_TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
	if has, err := m.Has("BRAVE_API_KEY"); err != nil || !has {
		t.Errorf("Has: %v %v", has, err)
	}
	if has, _ := m.Has("NOPE"); has {
		t.Error("Has(NOPE)")
	}
	_ = k
}

func TestEnvironmentOverrideAndValidation(t *testing.T) {
	m, _ := newMgr(t, map[string]string{"NOSKILL_SECRET_BRAVE_API_KEY": "from-env", "NOSKILL_SECRET_MY_KEY": "dotted"})
	m.Set("BRAVE_API_KEY", "from-keychain")
	if v, _ := m.Get("BRAVE_API_KEY"); v != "from-env" {
		t.Errorf("env must win: %q", v)
	}
	if v, ok := m.Get("my.key"); !ok || v != "dotted" {
		t.Errorf("dots and dashes map to underscores: %q %v", v, ok)
	}
	for _, bad := range []string{"", "1abc", "has space", "semi;colon", strings.Repeat("a", 65), "../x"} {
		if err := m.Set(bad, "v"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if err := m.Set("OK", ""); err == nil {
		t.Error("empty value accepted")
	}
	none := NewManager(nil, filepath.Join(t.TempDir(), "n"))
	none.Environ = func(string) (string, bool) { return "", false }
	if err := none.Set("A", "v"); err == nil || !strings.Contains(err.Error(), "NOSKILL_SECRET_A") {
		t.Errorf("no keychain: %v", err)
	}
	if _, err := none.Has("A"); err == nil {
		t.Error("Has without a keychain should say it can't check")
	}
}

// TestLiveKeychain writes one throwaway item to the real OS keychain and removes it again.
func TestLiveKeychain(t *testing.T) {
	if os.Getenv("NOSKILL_LIVE_KEYCHAIN") == "" {
		t.Skip("set NOSKILL_LIVE_KEYCHAIN=1 to use the real keychain")
	}
	k := OSKeychain()
	if k == nil {
		t.Skip("no keychain here")
	}
	var b [6]byte
	rand.Read(b[:])
	name := "noskill-test-" + hex.EncodeToString(b[:])
	value := `p@ss "quoted" \back 'single' $HOME ünï ` + hex.EncodeToString(b[:])
	t.Cleanup(func() { k.Delete(name) })
	if err := k.Set(name, value); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(name)
	if err != nil || got != value {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if err := k.Set(name, value+"2"); err != nil { // update in place
		t.Fatal(err)
	}
	if got, _ := k.Get(name); got != value+"2" {
		t.Errorf("update: %q", got)
	}
	if err := k.Delete(name); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(name); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if err := k.Delete(name); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
