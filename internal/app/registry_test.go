package app

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/registry"
)

func writeSkill(t *testing.T, dir, name, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: Keeps " + name + " in a folder.\nnoskill:\n  version: " + version +
		"\n  permissions:\n    network: [example.com]\n---\n\n# " + name + "\n\n1. Write them down.\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600)
	os.WriteFile(filepath.Join(dir, "scripts", "add.star"), []byte("print(1)\n"), 0o600)
}

func myKey(t *testing.T, a *App) string {
	t.Helper()
	if _, err := a.RegistryKeyNew(false); err != nil {
		t.Fatal(err)
	}
	pub, err := a.RegistryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return registry.EncodePublicKey(pub)
}

// Publish signs every skill and the index; a registry added with that key can be searched, and
// its skills install only while index, folder and signature agree (FR-S10, ADR-0016).
func TestRegistryPublishAddInstall(t *testing.T) {
	ctx := context.Background()
	pub := newApp(t, trainFake(new(bool)), nil, "").app // the publisher
	use := newApp(t, trainFake(new(bool)), nil, "").app // someone who adds the registry
	key := myKey(t, pub)
	if _, err := pub.RegistryKeyNew(false); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Errorf("second key: %v", err)
	}

	reg := t.TempDir()
	writeSkill(t, filepath.Join(reg, "notes"), "notes", "1.0.0")
	writeSkill(t, filepath.Join(reg, "more", "recipes"), "recipes", "0.2.0")
	os.MkdirAll(filepath.Join(reg, ".git"), 0o700)
	p, err := pub.RegistryPublish(reg, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Index.Entries) != 2 || p.Index.Entries[0].Path != "notes" || p.Index.Entries[1].Path != "more/recipes" ||
		p.Index.Entries[0].Permissions[0] != "network: example.com" {
		t.Fatalf("%+v", p.Index)
	}
	if _, err := os.Stat(filepath.Join(reg, "notes", registry.SigFile)); err != nil {
		t.Fatal(err)
	}

	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := use.RegistryAdd(ctx, "shared", reg, registry.EncodePublicKey(other)); err == nil || !strings.Contains(err.Error(), "isn't signed by the key") {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := use.RegistryAdd(ctx, "Bad Name", reg, key); err == nil {
		t.Error("a bad name was added")
	}
	info, err := use.RegistryAdd(ctx, "shared", reg, key)
	if err != nil || len(info.Index.Entries) != 2 {
		t.Fatalf("%v %+v", err, info)
	}
	hits, _ := use.RegistrySearch([]string{"recipes"})
	if len(hits) != 1 || hits[0].Registry != "shared" || hits[0].Version != "0.2.0" {
		t.Errorf("search: %+v", hits)
	}

	if r, s, ok := use.RegistryRef("shared/notes"); !ok || r != "shared" || s != "notes" {
		t.Errorf("ref: %v %v %v", r, s, ok)
	}
	if _, _, ok := use.RegistryRef("elsewhere/notes"); ok {
		t.Error("an unknown registry was taken as a ref")
	}
	pick, err := use.RegistryFetchSkill(ctx, "shared", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pick.From, "registry:shared/notes@1.0.0 sha256:") {
		t.Errorf("from: %s", pick.From)
	}
	sk, err := use.InstallSkill(ctx, pick.Dir, pick.From)
	pick.Close()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := use.VerifySkill("notes")
	if err != nil || !sc.Trusted || sc.By != "registry shared" {
		t.Errorf("verify: %v %+v", err, sc)
	}
	os.WriteFile(filepath.Join(sk.Dir, "scripts", "add.star"), []byte("print(666)\n"), 0o600)
	if sc, _ := use.VerifySkill("notes"); sc.Err == "" || len(sc.Changed) != 1 || sc.Changed[0] != "scripts/add.star" {
		t.Errorf("edited after install: %+v", sc)
	}

	// A file changed after publishing is refused, naming it.
	os.WriteFile(filepath.Join(reg, "more", "recipes", "SKILL.md"), []byte("---\nname: recipes\ndescription: x\nnoskill:\n  version: 0.2.0\n---\nsteal\n"), 0o600)
	if _, err := use.RegistryFetchSkill(ctx, "shared", "recipes"); err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Errorf("tampered: %v", err)
	}
	if _, err := use.RegistryFetchSkill(ctx, "shared", "nope"); err == nil || !strings.Contains(err.Error(), "no such skill") {
		t.Errorf("missing: %v", err)
	}

	// An older index than the one already seen is a rollback.
	oldIx, _ := os.ReadFile(filepath.Join(reg, registry.IndexFile))
	oldSig, _ := os.ReadFile(filepath.Join(reg, registry.IndexSigFile))
	writeSkill(t, filepath.Join(reg, "more", "recipes"), "recipes", "0.3.0")
	p2, err := pub.RegistryPublish(reg, "")
	if err != nil || p2.Index.Name != "shared" || !p2.Index.Made.After(p.Index.Made) {
		t.Fatalf("republish: %v %+v", err, p2)
	}
	if us, _ := use.RegistryUpdate(ctx, ""); len(us) != 1 || us[0].Err != "" {
		t.Fatalf("update: %+v", us)
	}
	os.WriteFile(filepath.Join(reg, registry.IndexFile), oldIx, 0o644)
	os.WriteFile(filepath.Join(reg, registry.IndexSigFile), oldSig, 0o644)
	if us, _ := use.RegistryUpdate(ctx, "shared"); len(us) != 1 || !strings.Contains(us[0].Err, "roll a skill back") {
		t.Errorf("rollback: %+v", us)
	}

	if err := use.RegistryRemove("shared"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := use.Registries(); len(rs) != 0 {
		t.Errorf("still there: %+v", rs)
	}
	if sc, _ := use.VerifySkill("notes"); sc.Trusted {
		t.Errorf("trusted after removing the registry: %+v", sc)
	}
}

// A plain install folder's signature is checked: yours is trusted, a stranger's is shown but not
// trusted, and a broken one is reported.
func TestCheckSignature(t *testing.T) {
	a := newApp(t, trainFake(new(bool)), nil, "").app
	dir := filepath.Join(t.TempDir(), "notes")
	writeSkill(t, dir, "notes", "1.0.0")
	sk, _ := LoadSkillDir(dir)
	if sc := a.CheckSignature(dir, sk); sc.Signed {
		t.Errorf("unsigned: %+v", sc)
	}
	if _, err := a.SignSkill(dir); err == nil {
		t.Error("signed with no key")
	}
	myKey(t, a)
	if _, err := a.SignSkill(dir); err != nil {
		t.Fatal(err)
	}
	if sc := a.CheckSignature(dir, sk); !sc.Trusted || sc.By != "you" {
		t.Errorf("mine: %+v", sc)
	}

	_, stranger, _ := ed25519.GenerateKey(nil)
	m, _ := registry.BuildManifest(dir, "notes", "1.0.0")
	os.WriteFile(filepath.Join(dir, registry.SigFile), registry.MarshalSignature(registry.Sign(m, stranger, time.Now())), 0o600)
	if sc := a.CheckSignature(dir, sk); !sc.Signed || sc.Trusted || sc.Err != "" {
		t.Errorf("stranger: %+v", sc)
	}
	os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o600)
	if sc := a.CheckSignature(dir, sk); sc.Err == "" || sc.Changed[0] != "extra.txt" {
		t.Errorf("changed: %+v", sc)
	}
}

// A registry in a git repository is fetched with a shallow clone and pinned to its commit.
func TestRegistryFromGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	ctx := context.Background()
	a := newApp(t, trainFake(new(bool)), nil, "").app
	key := myKey(t, a)
	repo := t.TempDir()
	writeSkill(t, filepath.Join(repo, "notes"), "notes", "1.0.0")
	if _, err := a.RegistryPublish(repo, "mine"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "skills"}} {
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	u := "file://" + filepath.ToSlash(repo)
	if !strings.HasPrefix(u, "file:///") {
		u = "file:///" + filepath.ToSlash(repo) // Windows: C:/…
	}
	if _, err := a.RegistryAdd(ctx, "mine", u, key); err != nil {
		t.Fatal(err)
	}
	pick, err := a.RegistryFetchSkill(ctx, "mine", "notes")
	if err != nil {
		t.Fatal(err)
	}
	defer pick.Close()
	if !strings.Contains(pick.From, " git:"+u+"@") || len(pick.From) < 40 {
		t.Errorf("from: %s", pick.From)
	}
	b, _ := os.ReadFile(a.registryCache("mine"))
	var c cachedIndex
	if json.Unmarshal(b, &c) != nil || c.Commit == "" {
		t.Errorf("cache: %s", b)
	}
}
