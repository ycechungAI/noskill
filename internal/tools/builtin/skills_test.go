package builtin_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/skills"
	"noskill/internal/tools/builtin"
	skillfiles "noskill/skills"
)

func TestEveryBuiltInSkillLoadsAndItsToolsExist(t *testing.T) {
	set, issues := skills.LoadAll([]skills.Root{{Source: skills.SourceBuiltin, FS: skillfiles.Builtin()}})
	for _, i := range issues {
		if i.Level == "error" || !strings.Contains(i.Msg, "MCP") {
			t.Errorf("%s", i)
		}
	}
	reg := builtin.Registry()
	for _, name := range skillfiles.Names {
		s, ok := set.Get(name)
		if !ok {
			t.Errorf("skill %s didn't load", name)
			continue
		}
		if !s.HasNoskill || s.Meta.Version == "" || s.Origin() != "builtin" || len(s.Tools) == 0 {
			t.Errorf("%s: %+v", name, s.Meta)
		}
		if strings.Contains(s.Body, "<!--") || strings.Contains(s.Body, "Implementation spec") {
			t.Errorf("%s: implementation notes leak into the body the model sees", name)
		}
		for _, tool := range s.Tools {
			if tool.Runtime != "builtin" {
				t.Errorf("%s/%s: built-in skills use builtin tools only", name, tool.Name)
				continue
			}
			b, ok := reg[tool.Entry]
			if !ok {
				t.Errorf("%s/%s: builtin %q is not implemented", name, tool.Name, tool.Entry)
				continue
			}
			if tool.Description == "" && b.Description == "" {
				t.Errorf("%s/%s: no description anywhere", name, tool.Name)
			}
		}
		// Every skill must be selectable: a description the selector can match.
		if len(s.Description) < 40 {
			t.Errorf("%s: description too short to select on", name)
		}
	}
	for _, name := range skillfiles.Names {
		if _, err := set.Closure(name); err != nil {
			t.Errorf("closure(%s): %v", name, err)
		}
	}
	// confirm lists only real tools (the loader warns about the rest).
	for _, s := range set.All() {
		for _, w := range s.Warnings {
			t.Errorf("%s: %s", s.Name, w)
		}
	}
}

// The other drafts in skills/ are specs for later phases, but they must stay valid skill files.
func TestAllSkillDraftsParse(t *testing.T) {
	root := filepath.Join("..", "..", "..", "skills")
	set, issues := skills.LoadAll([]skills.Root{skills.DiskRoot(skills.SourceBuiltin, root)})
	for _, i := range issues {
		// Phase 1 tools may be missing for the later skills' tool files; the front-matter may not be broken.
		if strings.Contains(i.Msg, "front-matter") || strings.Contains(i.Msg, "name") || strings.Contains(i.Msg, "description") {
			t.Errorf("%s", i)
		}
	}
	entries, _ := os.ReadDir(root)
	n := 0
	for _, e := range entries {
		if _, err := fs.Stat(os.DirFS(filepath.Join(root, e.Name())), "SKILL.md"); err == nil {
			n++
			if _, ok := set.Get(e.Name()); !ok {
				t.Errorf("%s did not load", e.Name())
			}
		}
	}
	if n != 10 {
		t.Errorf("found %d skill folders, want 10", n)
	}
}
