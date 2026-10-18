package app

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"noskill/internal/skills"
)

// ReloadSkills re-reads the skill folders (after an install or removal).
func (a *App) ReloadSkills(ctx context.Context) error { return a.loadSkills(ctx) }

// LoadSkillDir parses a skill folder that isn't installed yet, so its permissions can be shown
// before the user decides.
func LoadSkillDir(dir string) (*skills.Skill, []skills.Issue) {
	return skills.Load(os.DirFS(dir), filepath.Base(dir), dir, skills.SourceUser)
}

// InstallSkill copies a reviewed skill folder into the user's skills folder. The caller has already
// shown the permissions and got a yes; this never replaces an installed skill and never follows links.
func (a *App) InstallSkill(ctx context.Context, srcDir, installedFrom string) (*skills.Skill, error) {
	sk, issues := LoadSkillDir(srcDir)
	if sk == nil {
		msg := "not a valid skill"
		if len(issues) > 0 {
			msg = issues[len(issues)-1].Msg
		}
		return nil, &UserError{msg}
	}
	if old, ok := a.Skills.Get(sk.Name); ok {
		return nil, &UserError{fmt.Sprintf("a skill named %s is already installed (%s); remove it first with `noskill skills remove %s`", sk.Name, old.Source, sk.Name)}
	}
	dst := filepath.Join(a.Dirs.Skills(), sk.Name)
	if _, err := os.Lstat(dst); err == nil {
		return nil, &UserError{fmt.Sprintf("%s already exists", dst)}
	}
	if err := copyTree(srcDir, dst); err != nil {
		a.Trash.Trash(dst) // a half-copied folder of ours; the trash keeps the rule that noskill never deletes
		return nil, err
	}
	if err := a.loadSkills(ctx); err != nil {
		return nil, err
	}
	if installedFrom != "" {
		a.DB.SetInstalledFrom(ctx, sk.Name, installedFrom)
	}
	got, _ := a.Skills.Get(sk.Name)
	return got, nil
}

// RemoveSkill moves an installed skill's folder to the trash. Built-in skills can't be removed
// (disable them instead).
func (a *App) RemoveSkill(ctx context.Context, name string) error {
	sk, ok := a.Skills.Get(name)
	if !ok {
		return &UserError{fmt.Sprintf("no skill named %q", name)}
	}
	if sk.Source == skills.SourceBuiltin {
		return &UserError{name + " is built in; use `noskill skills disable " + name + "` instead"}
	}
	if sk.Dir == "" {
		return &UserError{"can't find " + name + "'s folder"}
	}
	if _, err := a.Trash.Trash(sk.Dir); err != nil {
		return fmt.Errorf("couldn't move %s to the trash: %w", sk.Dir, err)
	}
	if err := a.DB.ForgetSkill(ctx, name); err != nil {
		return err
	}
	return a.loadSkills(ctx)
}

// copyTree copies regular files and folders. Symlinks and special files are skipped, and so is
// .git: an installed skill is its files, not a checkout.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			mode := os.FileMode(0o644)
			if fi, err := d.Info(); err == nil && fi.Mode()&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
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
