package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Tuned built-in skills (FR-L9, ADR-0014): the optimiser's rewrite of a built-in skill's
// instructions. <dir>/<name>/.versions/<n>/SKILL.md holds each version (written once, never
// replaced); TUNED.json says which one is current and whether it's in use. An active version
// replaces the embedded SKILL.md when skills load, as long as it was made from the same embedded
// file: a noskill update that changes the built-in makes it stale.

// Tuned statuses.
const (
	TunedCandidate = "candidate" // made by `skills optimise`, waiting for `adopt`
	TunedActive    = "active"    // replaces the embedded instructions
	TunedReverted  = "reverted"  // the embedded instructions are used again
)

const tunedFile = "TUNED.json"

// TunedVersion is one saved rewrite.
type TunedVersion struct {
	N        int       `json:"n"`
	At       time.Time `json:"at"`
	BaseHash string    `json:"base_hash"` // sha256 of the embedded SKILL.md it was made from
	Note     string    `json:"note,omitempty"`
	Train    Score     `json:"train"`
	Held     Score     `json:"held"`
	Base     Score     `json:"base_train"` // the original instructions on the same tasks
	BaseHeld Score     `json:"base_held"`
}

// TunedInfo is a tuned skill's TUNED.json.
type TunedInfo struct {
	Status   string         `json:"status"`
	Current  int            `json:"current"`
	Versions []TunedVersion `json:"versions"`
}

// CurrentVersion is the version TUNED.json points at.
func (i TunedInfo) CurrentVersion() (TunedVersion, bool) {
	for _, v := range i.Versions {
		if v.N == i.Current {
			return v, true
		}
	}
	return TunedVersion{}, false
}

// Tuned is the folder of tuned built-ins (<data>/tuned).
type Tuned struct {
	Dir string
	Now func() time.Time
}

func (t *Tuned) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tuned) SkillDir(name string) string { return filepath.Join(t.Dir, name) }

func (t *Tuned) versionFile(name string, n int) string {
	return filepath.Join(t.SkillDir(name), versionsDir, strconv.Itoa(n), "SKILL.md")
}

// Info reads a tuned skill's state; ok is false when it has never been tuned.
func (t *Tuned) Info(name string) (TunedInfo, bool, error) {
	var info TunedInfo
	b, err := os.ReadFile(filepath.Join(t.SkillDir(name), tunedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return info, false, nil
	}
	if err != nil {
		return info, false, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, false, fmt.Errorf("%s: %w", filepath.Join(t.SkillDir(name), tunedFile), err)
	}
	return info, true, nil
}

func (t *Tuned) writeInfo(name string, info TunedInfo) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(t.SkillDir(name), tunedFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p) // TUNED.json is noskill's own state; every version stays in .versions
}

// VersionMD reads one saved version's SKILL.md.
func (t *Tuned) VersionMD(name string, n int) ([]byte, error) {
	return os.ReadFile(t.versionFile(name, n))
}

// Save stores a new rewrite as the current version, waiting for adoption.
func (t *Tuned) Save(name string, md []byte, v TunedVersion) (int, error) {
	info, _, err := t.Info(name)
	if err != nil {
		return 0, err
	}
	v.N = 1
	for _, o := range info.Versions {
		v.N = max(v.N, o.N+1)
	}
	if v.At.IsZero() {
		v.At = t.now()
	}
	p := t.versionFile(name, v.N)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(md); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	info.Versions = append(info.Versions, v)
	info.Current, info.Status = v.N, TunedCandidate
	if err := t.writeInfo(name, info); err != nil {
		return 0, err
	}
	return v.N, t.changelog(name, fmt.Sprintf("Version %d made: %s", v.N, orNone(v.Note)))
}

// SetStatus adopts (active) or reverts the current version.
func (t *Tuned) SetStatus(name, status, note string) error {
	info, ok, err := t.Info(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no tuned instructions", name)
	}
	info.Status = status
	if err := t.writeInfo(name, info); err != nil {
		return err
	}
	return t.changelog(name, fmt.Sprintf("Version %d %s: %s", info.Current, status, orNone(note)))
}

// UseVersion makes an older saved version current again (with the given status).
func (t *Tuned) UseVersion(name string, n int, status, note string) error {
	info, ok, err := t.Info(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s has no tuned instructions", name)
	}
	if _, err := os.Stat(t.versionFile(name, n)); err != nil {
		return fmt.Errorf("%s has no tuned version %d", name, n)
	}
	info.Current, info.Status = n, status
	if err := t.writeInfo(name, info); err != nil {
		return err
	}
	return t.changelog(name, fmt.Sprintf("Version %d %s: %s", n, status, orNone(note)))
}

func (t *Tuned) changelog(name, line string) error {
	p := filepath.Join(t.SkillDir(name), changelog)
	old, _ := os.ReadFile(p)
	head := "# Changelog\n"
	rest := strings.TrimPrefix(string(old), head)
	entry := fmt.Sprintf("\n- %s %s\n", t.now().Format("2006-01-02 15:04"), line)
	return os.WriteFile(p, []byte(head+entry+rest), 0o600)
}

// HashMD is the base hash of an embedded SKILL.md.
func HashMD(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Active returns the SKILL.md that replaces the embedded one for this skill, if any: the current
// version, adopted, made from this exact embedded file.
func (t *Tuned) Active(name string, embedded []byte) ([]byte, int, bool) {
	info, ok, err := t.Info(name)
	if err != nil || !ok || info.Status != TunedActive {
		return nil, 0, false
	}
	v, ok := info.CurrentVersion()
	if !ok || v.BaseHash != HashMD(embedded) {
		return nil, 0, false
	}
	md, err := t.VersionMD(name, v.N)
	if err != nil {
		return nil, 0, false
	}
	return md, v.N, true
}

// Stale says whether the current version was made from another embedded SKILL.md than this one.
func (t *Tuned) Stale(name string, embedded []byte) bool {
	info, ok, _ := t.Info(name)
	if !ok {
		return false
	}
	v, ok := info.CurrentVersion()
	return ok && v.BaseHash != HashMD(embedded)
}

// Overlay is the built-in skills with every active tuned SKILL.md in place of the embedded one.
// Everything else (other files, folders) is the embedded skill's. in reports which skills are
// tuned and at which version.
func (t *Tuned) Overlay(base fs.FS) (fs.FS, map[string]int) {
	over := map[string][]byte{}
	in := map[string]int{}
	ents, err := fs.ReadDir(base, ".")
	if err != nil {
		return base, in
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		emb, err := fs.ReadFile(base, path.Join(e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		if md, n, ok := t.Active(e.Name(), emb); ok {
			over[path.Join(e.Name(), "SKILL.md")] = md
			in[e.Name()] = n
		}
	}
	if len(over) == 0 {
		return base, in
	}
	return overlayFS{base: base, files: over}, in
}

// overlayFS serves some files from memory and the rest from base.
type overlayFS struct {
	base  fs.FS
	files map[string][]byte
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if b, ok := o.files[name]; ok {
		f, err := o.base.Open(name)
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		f.Close()
		if err != nil {
			return nil, err
		}
		return &memFile{name: path.Base(name), r: strings.NewReader(string(b)), size: int64(len(b)), mod: fi.ModTime()}, nil
	}
	return o.base.Open(name)
}

type memFile struct {
	name string
	r    *strings.Reader
	size int64
	mod  time.Time
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{f}, nil }
func (f *memFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *memFile) Close() error               { return nil }

type memInfo struct{ f *memFile }

func (i memInfo) Name() string       { return i.f.name }
func (i memInfo) Size() int64        { return i.f.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return i.f.mod }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }

// WithSkillMD is a skill folder with its SKILL.md replaced: how a candidate's instructions are
// loaded next to the skill's other files.
func WithSkillMD(base fs.FS, md []byte) fs.FS {
	return overlayFS{base: base, files: map[string][]byte{"SKILL.md": md}}
}
