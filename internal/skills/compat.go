package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Importing skills from other agent runtimes (FR-S8). A skill written for Claude Code, Codex or
// Hermes is a folder with a SKILL.md, which loads here unchanged (SKILL_FORMAT.md). This file finds
// such folders and says what in them noskill will not do, so the user decides knowing it. Nothing is
// rewritten: the import copies the folder as it is.

// Candidate is a skill found in another runtime's folders.
type Candidate struct {
	Runtime     string `json:"runtime"` // claude | codex | hermes | agents | path
	Dir         string `json:"dir"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Compat      Compat `json:"compat"`
}

// Compat is how well a skill carries over.
type Compat struct {
	Valid   bool     `json:"valid"`             // loads here at all
	Problem string   `json:"problem,omitempty"` // why not
	Notices []string `json:"notices,omitempty"` // what works differently here or not at all
	Scripts []string `json:"scripts,omitempty"` // script files in the folder
}

var scriptExt = map[string]bool{".py": true, ".sh": true, ".bash": true, ".js": true, ".mjs": true, ".ts": true, ".ps1": true, ".rb": true}

var (
	argsRe    = regexp.MustCompile(`\$ARGUMENTS\b|\$\{ARGUMENTS\}`)
	bangRe    = regexp.MustCompile("(?m)^[^`\n]*!`[^`\n]+`")
	rootVarRe = regexp.MustCompile(`\$\{?(CLAUDE_[A-Z_]+|CODEX_[A-Z_]+|SKILL_DIR)\}?`)
)

// Analyze reads a skill folder and reports what carries over. It reads SKILL.md only for its
// front matter and body; it runs nothing.
func Analyze(dir string) Compat {
	var c Compat
	sk, issues := Load(os.DirFS(dir), filepath.Base(dir), dir, SourceUser)
	if sk == nil {
		c.Problem = "not a valid skill"
		if len(issues) > 0 {
			c.Problem = issues[len(issues)-1].Msg
		}
		return c
	}
	c.Valid = true
	raw, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	fm, body, _ := splitFrontMatter(raw)
	var front map[string]any
	yaml.Unmarshal(fm, &front)
	note := func(f string, a ...any) { c.Notices = append(c.Notices, sprintf(f, a...)) }

	if !sk.HasNoskill {
		note("it has no noskill: block, so it declares no permissions: it can follow its instructions, but can't use files, the network or scripts until you add one")
	}
	if v, ok := front["allowed-tools"]; ok {
		note("it lists allowed-tools (%s); noskill doesn't grant these. A skill only gets the tools and permissions its own noskill: block declares and you approve", flat(v))
	}
	if v, ok := front["model"]; ok {
		note("it asks for model %s; noskill picks the model itself (route explain shows how)", flat(v))
	}
	if v, ok := front["context"]; ok && flat(v) == "fork" {
		note("it runs in a separate sub-agent in Claude Code; here it runs in the same conversation")
	}
	if _, ok := front["agent"]; ok {
		note("it names an agent type; noskill has no agent types, so that is ignored")
	}
	if _, ok := front["hooks"]; ok {
		note("it defines hooks; noskill doesn't run hooks from skills")
	}
	if v, ok := front["disable-model-invocation"]; ok && flat(v) == "true" {
		note("it is meant to be started by name only; noskill may pick it by its description (disable it with `noskill skills disable %s` if you want that)", sk.Name)
	}
	if argsRe.MatchString(body) {
		note("its text refers to $ARGUMENTS, which noskill doesn't fill in: the model reads your request instead")
	}
	if bangRe.MatchString(body) {
		note("it has !`command` lines that Claude Code runs before the skill starts; noskill doesn't run them")
	}
	if m := rootVarRe.FindString(body); m != "" {
		note("it refers to %s, a variable that only the original runtime fills in", m)
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && scriptExt[strings.ToLower(filepath.Ext(d.Name()))] {
			rel, _ := filepath.Rel(dir, p)
			c.Scripts = append(c.Scripts, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(c.Scripts)
	if len(c.Scripts) > 0 {
		note("it has %d script file(s) (%s); noskill runs scripts only in its sandbox, only for tools the skill declares, and only with permissions you approve", len(c.Scripts), strings.Join(first(c.Scripts, 3), ", "))
	}
	for _, w := range sk.Warnings {
		note("%s", w)
	}
	return c
}

func first(l []string, n int) []string {
	if len(l) > n {
		return append(append([]string{}, l[:n]...), "…")
	}
	return l
}

func flat(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, flat(e))
		}
		return strings.Join(parts, ", ")
	}
	b, _ := yaml.Marshal(v)
	return strings.TrimSpace(string(b))
}

// Roots are the folders where other runtimes keep skills, for this user and this working folder.
// The user's own folders come first. These are conventions, and a runtime that keeps its skills
// elsewhere is imported by path.
func Roots(home, cwd string, env func(string) string) []struct{ Runtime, Dir string } {
	if env == nil {
		env = os.Getenv
	}
	var out []struct{ Runtime, Dir string }
	add := func(rt, dir string) {
		if dir != "" {
			out = append(out, struct{ Runtime, Dir string }{rt, dir})
		}
	}
	codex := env("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	hermes := env("HERMES_HOME")
	if hermes == "" {
		hermes = filepath.Join(home, ".hermes")
	}
	add("claude", filepath.Join(home, ".claude", "skills"))
	add("codex", filepath.Join(codex, "skills"))
	add("hermes", filepath.Join(hermes, "skills"))
	add("agents", filepath.Join(home, ".agents", "skills"))
	if cwd != "" && cwd != home {
		add("claude", filepath.Join(cwd, ".claude", "skills"))
		add("codex", filepath.Join(cwd, ".codex", "skills"))
		add("agents", filepath.Join(cwd, ".agents", "skills"))
	}
	return out
}

// Discover finds the skills under the roots (a folder holding a SKILL.md, up to three levels down so
// category folders are seen), plus the skills that Claude Code plugins carry. Hidden folders are
// skipped.
func Discover(home, cwd string, env func(string) string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	scan := func(runtime, root string, depth int) {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				if p != root && (strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= depth) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "SKILL.md" {
				return nil
			}
			dir := filepath.Dir(p)
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				if seen[real] {
					return nil
				}
				seen[real] = true
			}
			out = append(out, candidate(runtime, dir))
			return nil
		})
	}
	for _, r := range Roots(home, cwd, env) {
		scan(r.Runtime, r.Dir, 3)
	}
	// Claude Code plugins keep skills in <plugin>/skills/<name>.
	filepath.WalkDir(filepath.Join(home, ".claude", "plugins"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "skills" {
			scan("claude", p, 1)
			return filepath.SkipDir
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Runtime != out[j].Runtime {
			return out[i].Runtime < out[j].Runtime
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CandidateAt describes the skill in one folder, from whichever runtime it came from.
func CandidateAt(runtime, dir string) Candidate { return candidate(runtime, dir) }

func candidate(runtime, dir string) Candidate {
	c := Candidate{Runtime: runtime, Dir: dir, Name: filepath.Base(dir), Compat: Analyze(dir)}
	if raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err == nil {
		if fm, _, err := splitFrontMatter(raw); err == nil {
			var f struct{ Name, Description string }
			if yaml.Unmarshal(fm, &f) == nil {
				if f.Name != "" {
					c.Name = f.Name
				}
				c.Description = strings.TrimSpace(f.Description)
			}
		}
	}
	return c
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
