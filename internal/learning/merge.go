package learning

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"

	"noskill/internal/skills"
)

// Merging (LEARNING.md §7, FR-L5): the curator folds a near-duplicate learned skill into another.
// The kept skill takes the duplicate's tests (its replay becomes required) and triggers.

// MergeInto adds the tests and triggers of the skill in dupDir to the draft in dir.
func MergeInto(dir, dupDir string, dup *skills.Skill) error {
	read := func(base string) func(string) ([]byte, error) {
		return func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(base, filepath.FromSlash(n))) }
	}
	cases, _, err := ReadCases(read(dir))
	if err != nil {
		return err
	}
	theirs, fx, err := ReadCases(read(dupDir))
	if err != nil {
		return fmt.Errorf("%s: %w", dup.Name, err)
	}
	// The duplicate's own script tool is the kept skill's now: its cases expect that one.
	rename := map[string]string{}
	if kept, _ := skills.Load(os.DirFS(dir), filepath.Base(dir), dir, skills.SourceLearned); kept != nil && len(kept.Tools) == 1 {
		for _, t := range dup.Tools {
			rename[t.Name] = kept.Tools[0].Name
		}
	}
	for n, f := range fx {
		for i := range f {
			if to, ok := rename[f[i].Tool]; ok {
				f[i].Tool = to
			}
		}
		fx[n] = f
	}
	written := map[string]string{}
	for i, c := range theirs {
		for k, t := range c.Expect.Tools {
			if to, ok := rename[t]; ok {
				c.Expect.Tools[k] = to
			}
		}
		c.Name = dup.Name + "-" + c.Name
		c.Required = c.Required || i == 0
		if c.Fixtures != "" {
			to, ok := written[c.Fixtures]
			if !ok {
				to = path.Join(path.Dir(c.Fixtures), dup.Name+"-"+path.Base(c.Fixtures))
				b, err := json.MarshalIndent(fx[c.Fixtures], "", "  ")
				if err != nil {
					return err
				}
				p := filepath.Join(dir, "tests", filepath.FromSlash(to))
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(p, b, 0o600); err != nil {
					return err
				}
				written[c.Fixtures] = to
			}
			c.Fixtures = to
		}
		cases = append(cases, c)
	}
	out, err := yaml.Marshal(cases)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tests", "cases.yaml"), out, 0o600); err != nil {
		return err
	}
	md := filepath.Join(dir, "SKILL.md")
	raw, err := os.ReadFile(md)
	if err != nil {
		return err
	}
	kept, _ := skills.Load(os.DirFS(dir), filepath.Base(dir), dir, skills.SourceLearned)
	var trig []string
	if kept != nil {
		trig = slices.Clone(kept.Meta.Triggers)
	}
	for _, t := range dup.Meta.Triggers {
		if !slices.Contains(trig, t) {
			trig = append(trig, t)
		}
	}
	if len(trig) == 0 {
		return nil
	}
	if raw, err = skills.SetMetaList(raw, "triggers", trig); err != nil {
		return err
	}
	return os.WriteFile(md, raw, 0o600)
}

// ToolsOf names the tools a learned skill's replay case uses (from its fixtures), for the curator's
// overlap check.
func ToolsOf(dir string) []string {
	cases, fx, err := ReadCases(func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(n))) })
	if err != nil || len(cases) == 0 {
		return nil
	}
	var out []string
	for _, f := range fx[cases[0].Fixtures] {
		if !slices.Contains(out, f.Tool) {
			out = append(out, f.Tool)
		}
	}
	return out
}

// Overlap is the Jaccard similarity of two tool sets.
func Overlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	both := 0
	for _, x := range a {
		if slices.Contains(b, x) {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}
