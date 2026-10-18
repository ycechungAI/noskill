package app

import (
	"context"
	"fmt"
	"time"

	"noskill/internal/learning"
	"noskill/internal/skills"
	"noskill/internal/store"
)

// Learned is the learned-skills library (<data>/learned, ADR-0010).
func (a *App) Learned() *learning.Library {
	return &learning.Library{Dir: a.Dirs.Learned(), Trash: a.Trash}
}

// Tuned is the tuned built-in skills (<data>/tuned, ADR-0014).
func (a *App) Tuned() *learning.Tuned { return &learning.Tuned{Dir: a.Dirs.Tuned()} }

// TunedVersion is the tuned version in use for a built-in skill, 0 when it uses the embedded one.
func (a *App) TunedVersion(name string) int {
	a.tunedMu.Lock()
	defer a.tunedMu.Unlock()
	return a.tunedIn[name]
}

func (a *App) learnedSkill(name string) error {
	sk, ok := a.Skills.Get(name)
	if ok && sk.Source == skills.SourceLearned {
		return nil
	}
	if ok {
		return &UserError{fmt.Sprintf("%s is a %s skill; only learned skills keep versions", name, sk.Source)}
	}
	if a.Learned().Exists(name) {
		return &UserError{fmt.Sprintf("%s is in the learned folder but another skill with that name is used instead", name)}
	}
	return &UserError{fmt.Sprintf("no learned skill named %q", name)}
}

// SkillVersions lists a learned skill's versions, oldest first (FR-S9).
func (a *App) SkillVersions(name string) ([]learning.Version, error) {
	if err := a.learnedSkill(name); err != nil {
		return nil, err
	}
	return a.Learned().Versions(name)
}

// DiffSkill compares two versions of a learned skill. With no versions given it shows the
// refinement waiting for its trial, or else the last change.
func (a *App) DiffSkill(name, from, to string) (string, error) {
	vs, err := a.SkillVersions(name)
	if err != nil {
		return "", err
	}
	find := func(v string) (learning.Version, error) {
		for _, x := range vs {
			if x.Version == v || (v == "current" && x.Current) || (v == "next" && x.Next) {
				return x, nil
			}
		}
		return learning.Version{}, &UserError{fmt.Sprintf("%s has no version %s", name, v)}
	}
	if from == "" {
		cur := 0
		for i, x := range vs {
			if x.Current {
				cur = i
			}
		}
		switch {
		case cur+1 < len(vs): // a .next refinement
			from, to = vs[cur].Version, vs[cur+1].Version
		case cur > 0:
			from, to = vs[cur-1].Version, vs[cur].Version
		default:
			return "", &UserError{name + " has only one version"}
		}
	}
	if to == "" {
		to = "current"
	}
	fv, err := find(from)
	if err != nil {
		return "", err
	}
	tv, err := find(to)
	if err != nil {
		return "", err
	}
	return learning.DiffDirs(fv.Dir, tv.Dir, fv.Version, tv.Version)
}

// RollbackSkill makes an earlier version of a learned skill current (FR-S9). With no version it
// goes back one.
func (a *App) RollbackSkill(ctx context.Context, name, version string) (string, error) {
	vs, err := a.SkillVersions(name)
	if err != nil {
		return "", err
	}
	if version == "" {
		for i, x := range vs {
			if x.Current && i > 0 {
				version = vs[i-1].Version
			}
		}
		if version == "" {
			return "", &UserError{name + " has no earlier version"}
		}
	}
	if err := a.Learned().Rollback(name, version); err != nil {
		return "", &UserError{err.Error()}
	}
	return version, a.loadSkills(ctx)
}

// recordVersion indexes a new learned version in skill_versions.
func (a *App) recordVersion(ctx context.Context, name, version, parent, note, by string) error {
	sk, ok := a.Skills.Get(name)
	hash := ""
	if ok {
		hash = sk.Hash
	}
	return a.DB.AddSkillVersion(ctx, store.SkillVersion{Name: name, Version: version, Hash: hash, Parent: parent,
		Changelog: note, CreatedBy: by}, time.Now())
}
