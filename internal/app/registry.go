package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"noskill/internal/registry"
	"noskill/internal/skills"
)

// Signed skills and registries (FR-S10, ADR-0016). Your signing key lives in the OS keychain;
// registries you add are kept with the public key you gave for each, in <config>/registries.json,
// and the last index seen from each is kept in <data>/registry/<name>.json.

// RegistryKeySecret is the keychain name of your registry signing key's seed.
const RegistryKeySecret = "registry-signing-key"

// RegistrySource is a registry you added.
type RegistrySource struct {
	Name  string    `json:"name"`
	Where string    `json:"where"` // a folder or a git URL
	Key   string    `json:"key"`   // base64 ed25519 public key, as you passed it
	KeyID string    `json:"key_id"`
	Added time.Time `json:"added"`
}

// RegistryInfo is a source with the last index seen from it.
type RegistryInfo struct {
	RegistrySource
	Index   *registry.Index `json:"index,omitempty"`
	Fetched time.Time       `json:"fetched,omitempty"`
	Commit  string          `json:"commit,omitempty"`
}

// cachedIndex is <data>/registry/<name>.json: the exact bytes that were checked.
type cachedIndex struct {
	Data    []byte    `json:"index"`
	Sig     []byte    `json:"sig"`
	Commit  string    `json:"commit,omitempty"`
	Fetched time.Time `json:"fetched"`
}

var registryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// IsGitURL says whether an install or registry source is a git URL rather than a folder.
func IsGitURL(s string) bool {
	if strings.HasPrefix(s, "git@") || strings.HasSuffix(s, ".git") {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "ssh" || u.Scheme == "git" || u.Scheme == "file")
}

// GitFetch makes a shallow clone of a URL in a scratch folder. done removes the folder.
func GitFetch(ctx context.Context, src string) (dir, commit string, done func(), err error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", "", nil, &UserError{"fetching from a URL needs git, which isn't installed"}
	}
	tmp, err := os.MkdirTemp("", "noskill-fetch-")
	if err != nil {
		return "", "", nil, err
	}
	done = func() { os.RemoveAll(tmp) } // our own scratch clone in the temp directory
	dir = filepath.Join(tmp, "src")
	clone := exec.CommandContext(ctx, git, "clone", "--depth", "1", "--quiet", "--", src, dir)
	clone.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := clone.CombinedOutput(); err != nil {
		done()
		return "", "", nil, fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(out)))
	}
	rev, _ := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "HEAD").Output()
	return dir, strings.TrimSpace(string(rev)), done, nil
}

// --- your key ---

func (a *App) registryKey() (ed25519.PrivateKey, error) {
	v, ok := a.Secrets.Get(RegistryKeySecret)
	if !ok {
		return nil, &UserError{"you have no registry key yet: `noskill registry key new` makes one"}
	}
	seed, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, &UserError{"the registry key in the keychain isn't valid; `noskill registry key new --replace` makes a new one"}
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// RegistryKeyNew makes your signing key. The seed goes to the keychain and is never shown.
func (a *App) RegistryKeyNew(replace bool) (ed25519.PublicKey, error) {
	if _, err := a.registryKey(); err == nil && !replace {
		return nil, &UserError{"you already have a registry key (`noskill registry key show`). --replace makes a new one; everyone who added your registry then has to add it again with the new key"}
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	if err := a.Secrets.Set(RegistryKeySecret, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		return nil, fmt.Errorf("couldn't save the key in the keychain: %w", err)
	}
	return pub, nil
}

// RegistryPublicKey is your key, to share.
func (a *App) RegistryPublicKey() (ed25519.PublicKey, error) {
	priv, err := a.registryKey()
	if err != nil {
		return nil, err
	}
	return priv.Public().(ed25519.PublicKey), nil
}

// --- signing ---

// SignSkill writes .noskill-sig.json into a skill folder, signed with your key.
func (a *App) SignSkill(dir string) (*registry.Signature, error) {
	priv, err := a.registryKey()
	if err != nil {
		return nil, err
	}
	sk, err := loadForSigning(dir)
	if err != nil {
		return nil, err
	}
	return signDir(dir, sk, priv)
}

func loadForSigning(dir string) (*skills.Skill, error) {
	sk, issues := LoadSkillDir(dir)
	if sk == nil {
		msg := "not a valid skill"
		if len(issues) > 0 {
			msg = issues[len(issues)-1].String()
		}
		return nil, &UserError{dir + ": " + msg}
	}
	return sk, nil
}

func signDir(dir string, sk *skills.Skill, priv ed25519.PrivateKey) (*registry.Signature, error) {
	m, err := registry.BuildManifest(dir, sk.Name, sk.Meta.Version)
	if err != nil {
		return nil, &UserError{fmt.Sprintf("can't sign %s: %v", sk.Name, err)}
	}
	s := registry.Sign(m, priv, time.Now())
	if err := os.WriteFile(filepath.Join(dir, registry.SigFile), registry.MarshalSignature(s), 0o644); err != nil {
		return nil, err
	}
	return &s, nil
}

// SigCheck is what a skill folder's signature says.
type SigCheck struct {
	Signed  bool     `json:"signed"`
	Trusted bool     `json:"trusted"`      // valid, by your key or a registry you added
	By      string   `json:"by,omitempty"` // "you" or "registry <name>"
	KeyID   string   `json:"key_id,omitempty"`
	Changed []string `json:"changed,omitempty"` // files that differ from what was signed
	Err     string   `json:"error,omitempty"`   // set when it's signed but doesn't check out
}

// CheckSignature checks a skill folder's .noskill-sig.json, if it has one. A valid signature by a
// key you haven't added is reported but not trusted; an invalid one has Err set.
func (a *App) CheckSignature(dir string, sk *skills.Skill) SigCheck {
	s, ok, err := registry.ReadSignature(dir)
	if !ok && err == nil {
		return SigCheck{}
	}
	c := SigCheck{Signed: true, KeyID: s.KeyID}
	if err == nil {
		err = registry.Verify(dir, s, sk.Name, sk.Meta.Version)
	}
	if err != nil {
		var ce *registry.ChangedError
		if errors.As(err, &ce) {
			c.Changed = ce.Files
		}
		c.Err = err.Error()
		return c
	}
	c.By = a.trustedKeys()[s.Key]
	c.Trusted = c.By != ""
	return c
}

// trustedKeys maps each trusted public key to who it belongs to.
func (a *App) trustedKeys() map[string]string {
	out := map[string]string{}
	rs, _ := a.Registries()
	for _, r := range rs {
		out[r.Key] = "registry " + r.Name
	}
	if pub, err := a.RegistryPublicKey(); err == nil {
		out[registry.EncodePublicKey(pub)] = "you"
	}
	return out
}

// VerifySkill checks an installed skill against its signature.
func (a *App) VerifySkill(name string) (SigCheck, error) {
	sk, ok := a.Skills.Get(name)
	if !ok {
		return SigCheck{}, &UserError{fmt.Sprintf("no skill named %q", name)}
	}
	if sk.Source == skills.SourceBuiltin {
		return SigCheck{}, &UserError{name + " is built in: it's part of noskill itself, which `noskill update` checks"}
	}
	if sk.Dir == "" {
		return SigCheck{}, &UserError{"can't find " + name + "'s folder"}
	}
	return a.CheckSignature(sk.Dir, sk), nil
}

// --- publishing ---

// Published is what `registry publish` wrote.
type Published struct {
	Dir     string         `json:"dir"`
	Index   registry.Index `json:"index"`
	Skipped []string       `json:"skipped,omitempty"`
}

// RegistryPublish signs every skill under dir with your key and writes index.json and its
// signature. name is the registry's name (default: the one already there, or the folder's name).
func (a *App) RegistryPublish(dir, name string) (*Published, error) {
	priv, err := a.registryKey()
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var old registry.Index
	if b, err := os.ReadFile(filepath.Join(dir, registry.IndexFile)); err == nil {
		json.Unmarshal(b, &old) // only for its name and time; it's rewritten below
	}
	ix := registry.Index{Name: name, Made: time.Now().UTC().Truncate(time.Second)}
	if ix.Name == "" {
		ix.Name = old.Name
	}
	if ix.Name == "" {
		ix.Name = filepath.Base(dir)
	}
	if !ix.Made.After(old.Made) {
		ix.Made = old.Made.Add(time.Second) // a slow clock mustn't make the new index look older
	}
	p := &Published{Dir: dir}
	seen := map[string]string{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == dir {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		sk, err := loadForSigning(path)
		if err != nil {
			p.Skipped = append(p.Skipped, err.Error())
			return fs.SkipDir
		}
		if was, ok := seen[sk.Name]; ok {
			return &UserError{fmt.Sprintf("two skills are named %s (%s and %s)", sk.Name, was, rel)}
		}
		seen[sk.Name] = rel
		s, err := signDir(path, sk, priv)
		if err != nil {
			return err
		}
		ix.Entries = append(ix.Entries, registry.Entry{Name: sk.Name, Version: sk.Meta.Version, Description: sk.Description,
			Path: rel, Digest: s.Digest, Permissions: permSummary(sk)})
		return fs.SkipDir // a skill's own subfolders aren't skills
	})
	if err != nil {
		return nil, err
	}
	if len(ix.Entries) == 0 {
		return nil, &UserError{"no skill folders (with a SKILL.md) under " + dir}
	}
	data, sig, err := registry.SignIndex(ix, priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, registry.IndexFile), data, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, registry.IndexSigFile), sig, 0o644); err != nil {
		return nil, err
	}
	json.Unmarshal(data, &p.Index)
	return p, nil
}

// permSummary is a skill's permissions in short form, for the index.
func permSummary(sk *skills.Skill) []string {
	p := sk.Meta.Permissions
	out := []string{"network: " + orNoneList(p.Network), "files: " + orNoneList(p.Filesystem)}
	if p.Exec {
		out = append(out, "exec")
	}
	if p.Subprocess {
		out = append(out, "subprocess")
	}
	var sec []string
	for _, s := range p.SecretList() {
		sec = append(sec, s.Name)
	}
	if len(sec) > 0 {
		out = append(out, "secrets: "+strings.Join(sec, ", "))
	}
	return out
}

func orNoneList(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// --- sources ---

func (a *App) registriesFile() string { return filepath.Join(a.Dirs.Config, "registries.json") }
func (a *App) registryCache(name string) string {
	return filepath.Join(a.Dirs.Data, "registry", name+".json")
}

// Registries lists the registries you added.
func (a *App) Registries() ([]RegistrySource, error) {
	b, err := os.ReadFile(a.registriesFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []RegistrySource
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("%s: %w", a.registriesFile(), err)
	}
	return rs, nil
}

func (a *App) registrySource(name string) (RegistrySource, error) {
	rs, err := a.Registries()
	if err != nil {
		return RegistrySource{}, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
	}
	return RegistrySource{}, &UserError{fmt.Sprintf("no registry named %q (`noskill registry list`)", name)}
}

// RegistryInfos lists the registries with the last index seen from each.
func (a *App) RegistryInfos() ([]RegistryInfo, error) {
	rs, err := a.Registries()
	var out []RegistryInfo
	for _, r := range rs {
		info := RegistryInfo{RegistrySource: r}
		if c, ix, ok := a.cached(r); ok {
			info.Index, info.Fetched, info.Commit = ix, c.Fetched, c.Commit
		}
		out = append(out, info)
	}
	return out, err
}

// cached reads and re-checks the last index kept for a registry.
func (a *App) cached(r RegistrySource) (cachedIndex, *registry.Index, bool) {
	var c cachedIndex
	b, err := os.ReadFile(a.registryCache(r.Name))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, nil, false
	}
	pub, err := registry.ParsePublicKey(r.Key)
	if err != nil {
		return c, nil, false
	}
	ix, err := registry.VerifyIndex(c.Data, c.Sig, pub)
	if err != nil {
		return c, nil, false
	}
	return c, &ix, true
}

// RegistryAdd fetches a registry, checks its index against key, and keeps it.
func (a *App) RegistryAdd(ctx context.Context, name, where, key string) (*RegistryInfo, error) {
	if !registryName.MatchString(name) {
		return nil, &UserError{fmt.Sprintf("%q can't be a registry name: use lower-case letters, digits, '.', '-' or '_'", name)}
	}
	pub, err := registry.ParsePublicKey(key)
	if err != nil {
		return nil, &UserError{"--key: " + err.Error()}
	}
	rs, err := a.Registries()
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Name == name {
			return nil, &UserError{fmt.Sprintf("a registry named %s is already added (%s); remove it first", name, r.Where)}
		}
	}
	if !IsGitURL(where) {
		abs, err := filepath.Abs(where)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, &UserError{where + " isn't a folder or a git URL"}
		}
		where = abs
	}
	src := RegistrySource{Name: name, Where: where, Key: registry.EncodePublicKey(pub), KeyID: registry.KeyID(pub), Added: time.Now().UTC()}
	f, err := a.fetchRegistry(ctx, src)
	if err != nil {
		return nil, err
	}
	f.done()
	if err := writeJSON(a.registriesFile(), append(rs, src)); err != nil {
		return nil, err
	}
	return &RegistryInfo{RegistrySource: src, Index: &f.index, Fetched: time.Now().UTC(), Commit: f.commit}, nil
}

// RegistryRemove forgets a registry; the index kept for it goes to the trash.
func (a *App) RegistryRemove(name string) error {
	rs, err := a.Registries()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rs, func(r RegistrySource) bool { return r.Name == name })
	if i < 0 {
		return &UserError{fmt.Sprintf("no registry named %q", name)}
	}
	if err := writeJSON(a.registriesFile(), slices.Delete(rs, i, i+1)); err != nil {
		return err
	}
	if _, err := os.Stat(a.registryCache(name)); err == nil {
		a.Trash.Trash(a.registryCache(name))
	}
	return nil
}

// RegistryUpdated is one registry's `registry update` result.
type RegistryUpdated struct {
	Name   string    `json:"name"`
	Skills int       `json:"skills"`
	Made   time.Time `json:"made"`
	Err    string    `json:"error,omitempty"`
}

// RegistryUpdate fetches one registry, or all of them, again.
func (a *App) RegistryUpdate(ctx context.Context, name string) ([]RegistryUpdated, error) {
	rs, err := a.Registries()
	if err != nil {
		return nil, err
	}
	if name != "" {
		r, err := a.registrySource(name)
		if err != nil {
			return nil, err
		}
		rs = []RegistrySource{r}
	}
	var out []RegistryUpdated
	for _, r := range rs {
		u := RegistryUpdated{Name: r.Name}
		if f, err := a.fetchRegistry(ctx, r); err != nil {
			u.Err = err.Error()
		} else {
			f.done()
			u.Skills, u.Made = len(f.index.Entries), f.index.Made
		}
		out = append(out, u)
	}
	return out, nil
}

// RegistryHit is one search result.
type RegistryHit struct {
	Registry string `json:"registry"`
	registry.Entry
}

// RegistrySearch searches the indexes kept from `registry add` and `update` (no network).
func (a *App) RegistrySearch(words []string) ([]RegistryHit, error) {
	rs, err := a.Registries()
	var out []RegistryHit
	for _, r := range rs {
		if _, ix, ok := a.cached(r); ok {
			for _, e := range ix.Search(words) {
				out = append(out, RegistryHit{r.Name, e})
			}
		}
	}
	return out, err
}

type fetchedRegistry struct {
	dir, commit string
	index       registry.Index
	done        func()
}

// fetchRegistry gets a registry's files, checks index.json against the key you added, refuses one
// older than the last seen (a rollback), and keeps the new one.
func (a *App) fetchRegistry(ctx context.Context, r RegistrySource) (*fetchedRegistry, error) {
	f := &fetchedRegistry{dir: r.Where, done: func() {}}
	if IsGitURL(r.Where) {
		dir, commit, done, err := GitFetch(ctx, r.Where)
		if err != nil {
			return nil, err
		}
		f.dir, f.commit, f.done = dir, commit, done
	}
	fail := func(err error) (*fetchedRegistry, error) {
		f.done()
		return nil, &UserError{fmt.Sprintf("registry %s: %v", r.Name, err)}
	}
	data, err := os.ReadFile(filepath.Join(f.dir, registry.IndexFile))
	if err != nil {
		return fail(fmt.Errorf("no %s (was it published with `noskill registry publish`?)", registry.IndexFile))
	}
	sig, err := os.ReadFile(filepath.Join(f.dir, registry.IndexSigFile))
	if err != nil {
		return fail(fmt.Errorf("%s isn't signed (no %s)", registry.IndexFile, registry.IndexSigFile))
	}
	pub, err := registry.ParsePublicKey(r.Key)
	if err != nil {
		return fail(err)
	}
	if f.index, err = registry.VerifyIndex(data, sig, pub); err != nil {
		return fail(err)
	}
	if _, old, ok := a.cached(r); ok && f.index.Made.Before(old.Made) {
		return fail(fmt.Errorf("its index (made %s) is older than the one already seen (made %s); someone may be serving an old copy to roll a skill back",
			f.index.Made.Format(time.RFC3339), old.Made.Format(time.RFC3339)))
	}
	{
		if err := os.MkdirAll(filepath.Dir(a.registryCache(r.Name)), 0o700); err != nil {
			return fail(err)
		}
		if err := writeJSON(a.registryCache(r.Name), cachedIndex{Data: data, Sig: sig, Commit: f.commit, Fetched: time.Now().UTC()}); err != nil {
			return fail(err)
		}
	}
	return f, nil
}

// --- installing from a registry ---

// RegistryRef splits <registry>/<skill> when the first part names a registry you added.
func (a *App) RegistryRef(s string) (reg, skill string, ok bool) {
	reg, skill, found := strings.Cut(s, "/")
	if !found || skill == "" || strings.ContainsAny(skill, `/\`) || IsGitURL(s) {
		return "", "", false
	}
	if _, err := a.registrySource(reg); err != nil {
		return "", "", false
	}
	return reg, skill, true
}

// RegistryPick is a skill fetched from a registry and checked, ready for the install screen.
type RegistryPick struct {
	Registry RegistrySource
	Entry    registry.Entry
	Dir      string
	From     string // what installed_from records
	Skill    *skills.Skill
	done     func()
}

// Close removes the scratch clone, if there is one.
func (p *RegistryPick) Close() { p.done() }

// RegistryFetchSkill fetches a registry, finds the skill and checks it: the index against the key
// you added, the folder against the index's digest and against its own signature by that key.
func (a *App) RegistryFetchSkill(ctx context.Context, reg, name string) (*RegistryPick, error) {
	r, err := a.registrySource(reg)
	if err != nil {
		return nil, err
	}
	f, err := a.fetchRegistry(ctx, r)
	if err != nil {
		return nil, err
	}
	fail := func(format string, args ...any) (*RegistryPick, error) {
		f.done()
		return nil, &UserError{fmt.Sprintf("%s/%s: ", reg, name) + fmt.Sprintf(format, args...)}
	}
	e, ok := f.index.Find(name)
	if !ok {
		return fail("no such skill in the registry (`noskill registry search`)")
	}
	dir := filepath.Join(f.dir, filepath.FromSlash(e.Path))
	if err := inside(f.dir, dir); err != nil {
		return fail("%v", err)
	}
	s, signed, err := registry.ReadSignature(dir)
	switch {
	case err != nil:
		return fail("%v", err)
	case !signed:
		return fail("its folder isn't signed")
	case s.Key != r.Key:
		return fail("it's signed by key %s, not the registry's key %s", s.KeyID, r.KeyID)
	}
	if err := registry.Verify(dir, s, e.Name, e.Version); err != nil {
		return fail("%v", err)
	}
	if s.Digest != e.Digest {
		return fail("its folder isn't the copy the index lists (digest %s, the index says %s)", s.Digest, e.Digest)
	}
	sk, err := loadForSigning(dir)
	if err != nil {
		return fail("%v", err)
	}
	if sk.Name != e.Name || sk.Meta.Version != e.Version {
		return fail("its SKILL.md says %s %s, the index says %s %s", sk.Name, sk.Meta.Version, e.Name, e.Version)
	}
	from := fmt.Sprintf("registry:%s/%s@%s %s", r.Name, e.Name, e.Version, e.Digest)
	if f.commit != "" {
		from += " git:" + r.Where + "@" + f.commit
	}
	return &RegistryPick{Registry: r, Entry: e, Dir: dir, From: from, Skill: sk, done: f.done}, nil
}

// inside refuses a skill folder that a link takes outside the registry.
func inside(root, dir string) error {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("its folder is missing")
	}
	if rel, err := filepath.Rel(rr, rd); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("its folder is outside the registry")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("its folder is a link")
	}
	return nil
}
