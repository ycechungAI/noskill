package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"noskill/internal/backendtest"
	"noskill/internal/perms"
	"noskill/internal/tools"
)

// uniqTrash is a trash that never reuses a name, like the real ones.
type uniqTrash struct {
	dir string
	n   *int
}

func (u uniqTrash) Trash(p string) (string, error) {
	*u.n++
	to := filepath.Join(u.dir, fmt.Sprintf("%d-%s", *u.n, filepath.Base(p)))
	return to, os.Rename(p, to)
}

// docs/REVIEW_BLOCK3.md: a v2 replacement (trash the old file, copy the new one in) is undone by the
// id the anime tracker files under, which has no run row of its own.
func TestUndoOfAReplacementUnderAnAnimeRunID(t *testing.T) {
	a := newApp(t, &backendtest.Fake{}, noRules, "").app
	n := 0
	a.Trash = uniqTrash{t.TempDir(), &n}
	ctx := context.Background()
	lib, _ := perms.Canonical(t.TempDir())
	downloads, _ := perms.Canonical(t.TempDir())
	scopes, err := perms.Resolver{}.Compile([]string{"write:" + lib, "write:" + downloads})
	if err != nil {
		t.Fatal(err)
	}
	const id = "anime-01TEST"
	b := &perms.Broker{RunID: id, Perms: perms.Set{Files: scopes}, Trash: a.Trash, Undo: a.DB}
	ep, v2 := filepath.Join(lib, "Show", "Show S01E01 [480p].mkv"), filepath.Join(downloads, "show-v2.mkv")
	os.MkdirAll(filepath.Dir(ep), 0o755)
	os.WriteFile(ep, []byte("v1"), 0o644)
	os.WriteFile(v2, []byte("v2"), 0o644)

	if err := b.TrashPath(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if err := b.Copy(ctx, v2, ep); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(ep); string(got) != "v2" {
		t.Fatalf("setup: %q", got)
	}

	steps, err := a.Undo(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if !s.Done {
			t.Fatalf("%+v", steps)
		}
	}
	if got, _ := os.ReadFile(ep); string(got) != "v1" {
		t.Errorf("the first release isn't back: %q", got)
	}
	if got, _ := os.ReadFile(v2); string(got) != "v2" {
		t.Errorf("the download copy changed: %q", got)
	}
	if _, err := a.Undo(ctx, "anime-nothing", false); err == nil {
		t.Error("an id with no run and no file changes must still be refused")
	}
}

func TestSanitizeNameIsSafeEverywhere(t *testing.T) {
	for in, want := range map[string]string{
		"Sousou no Frieren: Part 2":       "Sousou no Frieren - Part 2",
		".hack//SIGN":                     "hack--SIGN",
		"Re:Zero?":                        "Re -Zero",
		"  spaced   out  ":                "spaced out",
		"Trailing dot...":                 "Trailing dot",
		"CON":                             "CON_",
		"aux":                             "aux_",
		"Lpt1.special":                    "Lpt1.special_",
		"Console":                         "Console",
		"bell\x07 and\ttab\x00":           "bell andtab",
		"..":                              "_",
		"":                                "_",
		strings.Repeat("あ", 100):          strings.Repeat("あ", 66), // 198 bytes: no half character
		strings.Repeat("a", 300) + " end": strings.Repeat("a", 200),
	} {
		got := sanitizeName(in, "")
		if got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
		if !utf8.ValidString(got) || len(got) > 200 {
			t.Errorf("sanitizeName(%q) = %q is not valid or too long", in, got)
		}
	}
}

// Every built-in skill's declared folders must resolve on a fresh install (no settings, nothing
// approved), or a scheduled model-driven run fails before its first step ("${config.download_dir}
// isn't set"), which is how anime-nightly's first run failed on 2026-10-18.
func TestBuiltinSkillPermissionsResolveWithNoSettings(t *testing.T) {
	a := newApp(t, &backendtest.Fake{}, noRules, "").app
	n := 0
	for _, sk := range a.Skills.All() {
		if sk.Meta.Origin != "builtin" {
			continue
		}
		n++
		if _, err := tools.SkillPerms(a.resolver(), sk); err != nil {
			t.Errorf("%s: %v", sk.Name, err)
		}
	}
	if n < 9 {
		t.Errorf("only %d built-in skills checked", n)
	}
}
