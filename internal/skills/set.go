package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Root is one place skills are loaded from: each sub-folder holding a SKILL.md is a skill.
type Root struct {
	Source Source
	FS     fs.FS  // the folder's contents
	Dir    string // on-disk path; "" for the embedded built-ins
}

// DiskRoot opens a folder on disk. A missing folder is not an error: it has no skills.
func DiskRoot(src Source, dir string) Root { return Root{Source: src, FS: os.DirFS(dir), Dir: dir} }

// Set is the loaded skills. Name clashes resolve by trust, not by who's loaded first:
// user skills replace built-ins of the same name (the user chose that), but skills from a project
// folder or an extra path never replace one that's already there; the clash is reported.
type Set struct {
	byName  map[string]*Skill
	Shadows []Shadow
}

type Shadow struct {
	Name     string
	Kept     Source
	Skipped  Source
	SkipPath string
}

// Learned skills rank below everything the user chose, so a learned skill never replaces another
// skill (ADR-0010); the learning loop picks names that are free.
var precedence = map[Source]int{SourceUser: 4, SourceBuiltin: 3, SourceExtra: 2, SourceLearned: 1, SourceProject: 0}

// LoadAll loads every root. Problems go to the returned issues; one bad skill never stops the rest.
func LoadAll(roots []Root) (*Set, []Issue) {
	set := &Set{byName: map[string]*Skill{}}
	var issues []Issue
	for _, r := range roots {
		entries, err := fs.ReadDir(r.FS, ".")
		if err != nil {
			if !os.IsNotExist(err) {
				issues = append(issues, Issue{Level: "warn", Path: r.Dir, Msg: err.Error()})
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				// A symlinked skill folder shows as a non-dir entry: skip it. Skills are real
				// folders, so a link can't point a skill at somewhere else on disk.
				continue
			}
			sub, err := fs.Sub(r.FS, e.Name())
			if err != nil {
				continue
			}
			if _, err := fs.Stat(sub, "SKILL.md"); err != nil {
				continue // not a skill folder (shared assets, a .git checkout…)
			}
			disk := ""
			if r.Dir != "" {
				disk = filepath.Join(r.Dir, e.Name())
				if fi, err := os.Lstat(disk); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					issues = append(issues, Issue{Level: "warn", Skill: e.Name(), Path: disk, Msg: "symlinked skill folders aren't loaded"})
					continue
				}
			}
			s, is := Load(sub, e.Name(), disk, r.Source)
			issues = append(issues, is...)
			if s == nil {
				continue
			}
			if old, dup := set.byName[s.Name]; dup {
				if precedence[s.Source] > precedence[old.Source] {
					set.Shadows = append(set.Shadows, Shadow{Name: s.Name, Kept: s.Source, Skipped: old.Source, SkipPath: old.Dir})
					set.byName[s.Name] = s
				} else {
					set.Shadows = append(set.Shadows, Shadow{Name: s.Name, Kept: old.Source, Skipped: s.Source, SkipPath: disk})
					issues = append(issues, Issue{Level: "warn", Skill: s.Name, Path: disk,
						Msg: fmt.Sprintf("ignored: a %s skill with this name is already loaded", old.Source)})
				}
				continue
			}
			set.byName[s.Name] = s
		}
	}
	return set, issues
}

func (s *Set) Get(name string) (*Skill, bool) { sk, ok := s.byName[name]; return sk, ok }

// All returns the skills sorted by name.
func (s *Set) All() []*Skill {
	out := make([]*Skill, 0, len(s.byName))
	for _, sk := range s.byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolved is a tool as the model sees it.
type Resolved struct {
	Name  string // what the model calls
	Skill *Skill
	Tool  Tool
}

// Tools lists the tools of the given skills with model-visible names. A name used by more than one
// skill is prefixed with its skill ("web_search__search") so calls stay unambiguous.
func Tools(selected []*Skill) []Resolved {
	count := map[string]int{}
	for _, s := range selected {
		for _, t := range s.Tools {
			count[t.Name]++
		}
	}
	var out []Resolved
	for _, s := range selected {
		for _, t := range s.Tools {
			name := t.Name
			if count[name] > 1 {
				name = strings.NewReplacer("-", "_", ".", "_").Replace(s.Name) + "__" + t.Name
			}
			out = append(out, Resolved{Name: name, Skill: s, Tool: t})
		}
	}
	return out
}

// Closure returns the named skills and everything they depend on, dependencies first. A cycle or
// a missing dependency is an error: running with half a skill would fail later in a confusing way.
func (s *Set) Closure(names ...string) ([]*Skill, error) {
	var out []*Skill
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(n string, path []string) error
	visit = func(n string, path []string) error {
		sk, ok := s.byName[n]
		if !ok {
			return fmt.Errorf("skill %q isn't installed (needed by %s)", n, strings.Join(path, " → "))
		}
		switch state[n] {
		case 1:
			return fmt.Errorf("skills depend on each other in a loop: %s → %s", strings.Join(path, " → "), n)
		case 2:
			return nil
		}
		state[n] = 1
		for _, d := range sk.Meta.DependsOn {
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		if !slices.Contains(out, sk) {
			out = append(out, sk)
		}
		return nil
	}
	for _, n := range names {
		if err := visit(n, []string{n}); err != nil {
			return nil, err
		}
	}
	return out, nil
}
