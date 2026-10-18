package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"noskill/internal/platform"
	"noskill/internal/skills"
)

// Library is the learned-skills folder, <data>/learned (ADR-0010, FR-S9):
//
//	.drafts/<name>/            a draft being written and verified; not loaded
//	.rejected/<name>-<time>/   drafts that failed verification, kept for inspection
//	<name>/                    a verified skill, loaded like any other; status is in its front-matter
//	<name>/.next/              a refined version waiting for its trial
//	<name>/.versions/<v>/      every earlier version
//	<name>/CHANGELOG.md        one entry per version, newest first
//
// Nothing is ever deleted: replaced content goes to .versions/, and a copy that is already there
// goes to the OS trash.
type Library struct {
	Dir   string
	Trash platform.Trasher
	Now   func() time.Time
}

const (
	draftsDir   = ".drafts"
	rejectedDir = ".rejected"
	nextDir     = ".next"
	versionsDir = ".versions"
	changelog   = "CHANGELOG.md"
)

// managed are the entries of a skill folder that belong to the library, not to a version.
var managed = []string{nextDir, versionsDir, changelog}

func (l *Library) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Library) SkillDir(name string) string { return filepath.Join(l.Dir, name) }
func (l *Library) DraftDir(name string) string { return filepath.Join(l.Dir, draftsDir, name) }
func (l *Library) NextDir(name string) string  { return filepath.Join(l.Dir, name, nextDir) }

// Exists says whether a verified skill of that name is in the library.
func (l *Library) Exists(name string) bool {
	_, err := os.Stat(filepath.Join(l.SkillDir(name), "SKILL.md"))
	return err == nil
}

// HasNext says whether a refinement or merge is on trial for the skill.
func (l *Library) HasNext(name string) bool {
	_, err := os.Stat(filepath.Join(l.NextDir(name), "SKILL.md"))
	return err == nil
}

// NewDraftDir makes an empty folder for a draft. A leftover draft of the same name is set aside in
// .rejected first.
func (l *Library) NewDraftDir(name string) (string, error) {
	d := l.DraftDir(name)
	if _, err := os.Lstat(d); err == nil {
		if _, err := l.Reject(d, "replaced by a newer draft"); err != nil {
			return "", err
		}
	}
	return d, os.MkdirAll(d, 0o700)
}

// NewRefineDir makes a draft that starts as a copy of the skill's current version.
func (l *Library) NewRefineDir(name string) (string, error) {
	if !l.Exists(name) {
		return "", fmt.Errorf("no learned skill named %q", name)
	}
	d, err := l.NewDraftDir(name)
	if err != nil {
		return "", err
	}
	return d, copyContent(l.SkillDir(name), d)
}

// Reject moves a draft (or a .next version) to .rejected with the reason in REJECTED.txt.
func (l *Library) Reject(dir, reason string) (string, error) {
	name := filepath.Base(dir)
	if name == nextDir {
		name = filepath.Base(filepath.Dir(dir)) + "-next"
	}
	dst := filepath.Join(l.Dir, rejectedDir, name+"-"+l.now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(dir, dst); err != nil {
		return "", err
	}
	return dst, os.WriteFile(filepath.Join(dst, "REJECTED.txt"), []byte(strings.TrimSpace(reason)+"\n"), 0o600)
}

// Publish moves a verified draft into the library. A new skill becomes <name>/ with status
// candidate and version 1.0.0; for an existing skill the draft becomes <name>/.next (a refinement
// on trial), replacing any earlier .next. It returns the version the draft will have.
func (l *Library) Publish(draft, note string) (string, error) {
	name := filepath.Base(draft)
	dst := l.SkillDir(name)
	if !l.Exists(name) {
		if _, err := os.Lstat(dst); err == nil {
			return "", fmt.Errorf("%s exists but isn't a skill", dst)
		}
		v := "1.0.0"
		if err := setMetaFile(draft, map[string]string{"version": v, "origin": "learned", "status": "candidate"}); err != nil {
			return "", err
		}
		if err := os.Rename(draft, dst); err != nil {
			return "", err
		}
		return v, l.addChangelog(name, v, note)
	}
	next := l.NextDir(name)
	if _, err := os.Lstat(next); err == nil {
		if _, err := l.Reject(next, "replaced by a newer refinement"); err != nil {
			return "", err
		}
	}
	v, err := l.nextVersion(name)
	if err != nil {
		return "", err
	}
	if err := setMetaFile(draft, map[string]string{"version": v, "origin": "learned"}); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(draft, ".note"), []byte(note), 0o600); err != nil {
		return "", err
	}
	return v, os.Rename(draft, next)
}

// AdoptNext makes the refined version in .next the current one; the current version goes to
// .versions. The skill keeps its status.
func (l *Library) AdoptNext(name string) (string, error) {
	next := l.NextDir(name)
	v, _, err := readVersion(next)
	if err != nil {
		return "", fmt.Errorf("%s has no refined version waiting", name)
	}
	note, _ := os.ReadFile(filepath.Join(next, ".note"))
	if err := l.replaceWith(name, next, "", true); err != nil {
		return "", err
	}
	if err := l.addChangelog(name, v, string(note)); err != nil {
		return "", err
	}
	// Only the note is left, and it's now in the changelog.
	os.Remove(filepath.Join(next, ".note"))
	os.Remove(next)
	return v, nil
}

// Rollback makes an earlier version current again (FR-S9). The current version is kept in
// .versions; the skill keeps its status.
func (l *Library) Rollback(name, version string) error {
	src := filepath.Join(l.SkillDir(name), versionsDir, version)
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return fmt.Errorf("%s has no version %s", name, version)
	}
	cur, _, err := readVersion(l.SkillDir(name))
	if err != nil {
		return err
	}
	if err := l.replaceWith(name, src, version, false); err != nil {
		return err
	}
	return l.addChangelog(name, version, "Rolled back from "+cur+".")
}

// SetStatus changes the status in the skill's front-matter. A status change isn't a new version.
func (l *Library) SetStatus(name, status, note string) error {
	if err := setMetaFile(l.SkillDir(name), map[string]string{"status": status}); err != nil {
		return err
	}
	if note == "" {
		return nil
	}
	version, _, err := readVersion(l.SkillDir(name))
	if err != nil {
		return err
	}
	return l.addChangelog(name, version, note)
}

// Version is one stored version of a learned skill.
type Version struct {
	Version string
	Dir     string
	Current bool
	Next    bool // a refinement waiting for its trial
	Time    time.Time
}

// Versions lists a skill's versions, oldest first: .versions, then the current one, then .next.
func (l *Library) Versions(name string) ([]Version, error) {
	if !l.Exists(name) {
		return nil, fmt.Errorf("no learned skill named %q", name)
	}
	var out []Version
	ents, _ := os.ReadDir(filepath.Join(l.SkillDir(name), versionsDir))
	for _, e := range ents {
		if e.IsDir() {
			d := filepath.Join(l.SkillDir(name), versionsDir, e.Name())
			out = append(out, Version{Version: e.Name(), Dir: d, Time: modTime(d)})
		}
	}
	slices.SortFunc(out, func(a, b Version) int { return compareVersions(a.Version, b.Version) })
	cur, _, err := readVersion(l.SkillDir(name))
	if err != nil {
		return nil, err
	}
	out = append(out, Version{Version: cur, Dir: l.SkillDir(name), Current: true, Time: modTime(filepath.Join(l.SkillDir(name), "SKILL.md"))})
	if v, _, err := readVersion(l.NextDir(name)); err == nil {
		out = append(out, Version{Version: v, Dir: l.NextDir(name), Next: true, Time: modTime(l.NextDir(name))})
	}
	return out, nil
}

// replaceWith puts src's files in place of the skill's current version. The current files move to
// .versions/<their version>; when that version is already stored unchanged, they go to the trash
// instead, and when the stored copy differs (the folder was edited by hand) they're kept as
// <version>-edited-<time>. src's files are moved when move is set, else copied. version, when set,
// is written to the new SKILL.md.
func (l *Library) replaceWith(name, src, version string, move bool) error {
	dir := l.SkillDir(name)
	cur, _, err := readVersion(dir)
	if err != nil {
		return err
	}
	if cur == "" {
		cur = "0.0.0"
	}
	snap := filepath.Join(dir, versionsDir, cur)
	if _, err := os.Stat(snap); err == nil {
		same, err := sameContent(dir, snap)
		if err != nil {
			return err
		}
		if !same {
			snap = filepath.Join(dir, versionsDir, cur+"-edited-"+l.now().UTC().Format("20060102T150405Z"))
		} else {
			snap = ""
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if snap != "" {
		if err := os.MkdirAll(snap, 0o700); err != nil {
			return err
		}
	}
	for _, e := range ents {
		if slices.Contains(managed, e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if snap != "" {
			if err := os.Rename(p, filepath.Join(snap, e.Name())); err != nil {
				return err
			}
		} else if _, err := l.Trash.Trash(p); err != nil {
			return fmt.Errorf("couldn't move %s to the trash: %w", p, err)
		}
	}
	if move {
		ents, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if !slices.Contains(managed, e.Name()) && e.Name() != ".note" {
				if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
	} else if err := copyContent(src, dir); err != nil {
		return err
	}
	if version != "" {
		return setMetaFile(dir, map[string]string{"version": version})
	}
	return nil
}

func (l *Library) nextVersion(name string) (string, error) {
	vs, err := l.Versions(name)
	if err != nil {
		return "", err
	}
	top := "0.0.0"
	for _, v := range vs {
		if compareVersions(v.Version, top) > 0 {
			top = v.Version
		}
	}
	p := parseVersion(top)
	return fmt.Sprintf("%d.%d.0", p[0], p[1]+1), nil
}

func (l *Library) addChangelog(name, version, note string) error {
	p := filepath.Join(l.SkillDir(name), changelog)
	old, _ := os.ReadFile(p)
	head := "# Changelog\n"
	rest := strings.TrimPrefix(string(old), head)
	note = strings.TrimSpace(note)
	if note == "" {
		note = "No notes."
	}
	entry := fmt.Sprintf("\n## %s (%s)\n%s\n", version, l.now().Format("2006-01-02"), note)
	return os.WriteFile(p, []byte(head+entry+rest), 0o600)
}

// ---- helpers ----

func setMetaFile(dir string, set map[string]string) error {
	p := filepath.Join(dir, "SKILL.md")
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out, err := skills.SetMeta(raw, set)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return os.WriteFile(p, out, 0o600)
}

// readVersion returns noskill.version and status from dir/SKILL.md.
func readVersion(dir string) (version, status string, err error) {
	sk, issues := skills.Load(os.DirFS(dir), "", dir, skills.SourceLearned)
	if sk == nil {
		msg := "not a skill"
		if len(issues) > 0 {
			msg = issues[len(issues)-1].Msg
		}
		return "", "", errors.New(dir + ": " + msg)
	}
	return sk.Meta.Version, sk.Status(), nil
}

func modTime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// contentHash hashes a version's files, leaving out what the library manages.
func contentHash(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if slices.Contains(managed, rel) || rel == ".note" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func sameContent(a, b string) (bool, error) {
	ha, err := contentHash(a)
	if err != nil {
		return false, err
	}
	hb, err := contentHash(b)
	return ha == hb, err
}

// copyContent copies a version's regular files into dst, skipping managed entries and links.
func copyContent(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if slices.Contains(managed, rel) || rel == ".note" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			mode := os.FileMode(0o600)
			if fi, err := d.Info(); err == nil && fi.Mode()&0o111 != 0 {
				mode = 0o700
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}

func parseVersion(v string) [3]int {
	var out [3]int
	v, _, _ = strings.Cut(v, "-") // "1.1.0-edited-…" sorts with 1.1.0
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return strings.Compare(a, b)
}
