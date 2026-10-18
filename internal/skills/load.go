package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Limits keep a hostile or broken skill folder from exhausting memory while it's only being indexed.
const (
	maxSkillFile = 256 << 10
	maxToolFile  = 64 << 10
	maxTreeBytes = 20 << 20
	maxTools     = 64
)

var (
	nameRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	specNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	toolNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)

	// Implementation notes for skill authors live in HTML comments and never reach the model.
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

	optionalSecretRe = regexp.MustCompile(`[\[,]\s*[A-Za-z0-9_]+\?\s*[,\]]`)
)

var scriptRuntimes = []string{"python", "node", "bun", "sh", "pwsh"}

// Load parses the skill whose files are at the root of fsys. dirName is the folder's name (the
// agentskills.io spec wants it to equal the skill name); diskDir is "" for embedded skills.
func Load(fsys fs.FS, dirName, diskDir string, src Source) (*Skill, []Issue) {
	var issues []Issue
	fail := func(f string, a ...any) (*Skill, []Issue) {
		return nil, append(issues, Issue{Level: "error", Skill: dirName, Path: path.Join(dirName, "SKILL.md"), Msg: fmt.Sprintf(f, a...)})
	}
	raw, err := readLimited(fsys, "SKILL.md", maxSkillFile)
	if err != nil {
		return fail("%v", err)
	}
	fm, body, err := splitFrontMatter(raw)
	if err != nil {
		return fail("%v", err)
	}
	var front struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		License     string `yaml:"license"`
		Noskill     *Meta  `yaml:"noskill"`
	}
	if err := yaml.Unmarshal(fm, &front); err != nil {
		hint := ""
		if optionalSecretRe.Match(fm) {
			hint = ` (an optional secret like NAME? must be quoted inside a [list]: "NAME?")`
		} else if strings.Contains(err.Error(), "mapping values are not allowed") {
			hint = ` (a value containing ": " must be quoted, for example description: "Work with PDFs: split and merge")`
		}
		return fail("front-matter: %v%s", err, hint)
	}
	if front.Name == "" {
		return fail("front-matter has no name")
	}
	if !nameRe.MatchString(front.Name) {
		return fail("name %q: use letters, digits, '.', '_' and '-' (at most 64 characters, starting with a letter or digit)", front.Name)
	}
	if strings.TrimSpace(front.Description) == "" {
		return fail("front-matter has no description")
	}
	s := &Skill{Name: front.Name, Description: strings.TrimSpace(front.Description), License: front.License,
		Body: strings.TrimSpace(commentRe.ReplaceAllString(body, "")), Source: src, Dir: diskDir, FS: fsys}
	warn := func(f string, a ...any) {
		m := fmt.Sprintf(f, a...)
		s.Warnings = append(s.Warnings, m)
		issues = append(issues, Issue{Level: "warn", Skill: s.Name, Path: path.Join(dirName, "SKILL.md"), Msg: m})
	}
	if !specNameRe.MatchString(s.Name) {
		warn("name %q isn't lowercase-with-hyphens as agentskills.io expects", s.Name)
	}
	if dirName != "" && dirName != "." && dirName != s.Name {
		warn("folder %q doesn't match the skill name %q", dirName, s.Name)
	}
	if len(s.Description) > 1024 {
		warn("description is %d characters; agentskills.io allows 1024. Only the start is used for selection", len(s.Description))
	}
	if front.Noskill != nil {
		s.HasNoskill = true
		s.Meta = *front.Noskill
		for k := range s.Meta.Extra {
			warn("unknown key noskill.%s", k)
		}
		s.Meta.Extra = nil
		s.checkMeta(warn)
	}
	for _, ref := range s.Meta.ToolRefs {
		t, tissues := loadTool(fsys, s.Name, dirName, ref)
		issues = append(issues, tissues...)
		if t == nil {
			continue
		}
		if len(s.Tools) >= maxTools {
			issues = append(issues, Issue{Level: "error", Skill: s.Name, Msg: fmt.Sprintf("more than %d tools", maxTools)})
			break
		}
		if slices.ContainsFunc(s.Tools, func(x Tool) bool { return x.Name == t.Name }) {
			issues = append(issues, Issue{Level: "error", Skill: s.Name, Path: path.Join(dirName, t.File), Msg: fmt.Sprintf("duplicate tool name %q", t.Name)})
			continue
		}
		s.Tools = append(s.Tools, *t)
	}
	for _, c := range s.Meta.Confirm {
		if !slices.ContainsFunc(s.Tools, func(t Tool) bool { return t.Name == c }) {
			warn("confirm lists %q, which isn't one of this skill's tools", c)
		}
	}
	s.Hash, err = hashTree(fsys)
	if err != nil {
		return fail("%v", err)
	}
	return s, issues
}

func (s *Skill) checkMeta(warn func(string, ...any)) {
	m := &s.Meta
	if !slices.Contains([]string{"", "builtin", "user", "learned", "imported"}, m.Origin) {
		warn("origin %q isn't builtin, user, learned or imported", m.Origin)
		m.Origin = ""
	}
	if !slices.Contains([]string{"", "draft", "candidate", "active", "stale", "archived"}, m.Status) {
		warn("status %q isn't draft, candidate, active, stale or archived; treating it as draft", m.Status)
		m.Status = "draft"
	}
	if !slices.Contains([]string{"", "any", "native", "none"}, m.MinModel.Tools) {
		warn("min_model.tools %q isn't any, native or none", m.MinModel.Tools)
	}
	for _, p := range m.Permissions.Filesystem {
		acc, _, ok := strings.Cut(p, ":")
		if !ok || (acc != "read" && acc != "write") {
			warn("filesystem permission %q: want read:<path> or write:<path>", p)
		}
	}
	if b := m.Budget; b.Steps < 0 || b.Tokens < 0 || b.Steps > MaxSkillSteps || b.Tokens > MaxSkillTokens {
		warn("budget: steps 0-%d and tokens 0-%d (a skill can't ask for more; %d steps / %d tokens asked)", MaxSkillSteps, MaxSkillTokens, b.Steps, b.Tokens)
	}
	for _, sec := range m.Permissions.SecretList() {
		if sec.Name == "" || strings.ContainsAny(sec.Name, " =\x00") {
			warn("secret name %q isn't valid", sec.Name)
		}
	}
	// Task types are free text so skills can add their own, but a typo in a built-in one is
	// worth a word.
	for _, t := range m.TaskTypes {
		if t == "" {
			warn("empty task type")
		}
	}
}

func readLimited(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a folder", name)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", name, fi.Size(), limit)
	}
	var b bytes.Buffer
	if _, err := b.ReadFrom(io.LimitReader(f, limit+1)); err != nil {
		return nil, err
	}
	if int64(b.Len()) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b.Bytes(), nil
}

// splitFrontMatter separates the YAML block between the first two "---" lines from the Markdown.
func splitFrontMatter(b []byte) (fm []byte, body string, err error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return nil, "", errors.New("SKILL.md must start with a --- front-matter block")
	}
	// The closing line is "---" on its own. Searching "\n"+rest lets an empty block ("---\n---")
	// match at position 0; the index there equals the block's length in rest.
	if i := strings.Index("\n"+rest, "\n---\n"); i >= 0 {
		return []byte(rest[:i]), rest[i+len("\n---\n")-1:], nil
	}
	if strings.HasSuffix("\n"+rest, "\n---") { // the file ends right after the closing line
		return []byte(rest[:len(rest)-len("---")]), "", nil
	}
	return nil, "", errors.New("front-matter is not closed with ---")
}

func hashTree(fsys fs.FS) (string, error) {
	h := sha256.New()
	var total int64
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".versions" { // a learned skill's old versions aren't its content
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // symlinks and specials aren't part of the skill
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if total += int64(len(b)); total > maxTreeBytes {
			return fmt.Errorf("skill folder is larger than %d MB", maxTreeBytes>>20)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// ---- tools ----

type toolFile struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	Runtime      string    `yaml:"runtime"`
	Entry        yaml.Node `yaml:"entry"`
	Deps         []string  `yaml:"deps"`
	Timeout      string    `yaml:"timeout"`
	MaxOutput    string    `yaml:"max_output"`
	InputSchema  any       `yaml:"input_schema"`
	OutputSchema any       `yaml:"output_schema"`
	OS           []string  `yaml:"os"`
}

func loadTool(fsys fs.FS, skill, dirName string, ref ToolRef) (*Tool, []Issue) {
	var issues []Issue
	if ref.Builtin != "" {
		if !toolNameRe.MatchString(ref.Builtin) {
			return nil, append(issues, Issue{Level: "error", Skill: skill, Msg: fmt.Sprintf("builtin tool id %q isn't valid", ref.Builtin)})
		}
		return &Tool{Name: ref.Builtin, Runtime: "builtin", Entry: ref.Builtin, Skill: skill, Inline: true,
			Timeout: 30 * time.Second, MaxOutput: 16 << 10}, issues
	}
	bad := func(file, f string, a ...any) (*Tool, []Issue) {
		return nil, append(issues, Issue{Level: "error", Skill: skill, Path: path.Join(dirName, file), Msg: fmt.Sprintf(f, a...)})
	}
	if ref.MCP != nil {
		return nil, append(issues, Issue{Level: "warn", Skill: skill, Msg: fmt.Sprintf("MCP tool %s/%s skipped: MCP tools arrive with FR-I5", ref.MCP.Server, ref.MCP.Tool)})
	}
	file := ref.Path
	if !fs.ValidPath(file) || strings.HasPrefix(file, "/") || file == "." {
		return bad(file, "tool path must be inside the skill folder")
	}
	raw, err := readLimited(fsys, file, maxToolFile)
	if err != nil {
		return bad(file, "%v", err)
	}
	var tf toolFile
	if err := yaml.Unmarshal(raw, &tf); err != nil {
		return bad(file, "%v", err)
	}
	if !toolNameRe.MatchString(tf.Name) {
		return bad(file, "tool name %q: start with a letter, then letters, digits, '_', '.' or '-' (at most 64 characters)", tf.Name)
	}
	if strings.TrimSpace(tf.Description) == "" {
		return bad(file, "tool %q has no description (the model needs it)", tf.Name)
	}
	t := &Tool{Name: tf.Name, Description: strings.TrimSpace(tf.Description), Runtime: tf.Runtime, Deps: tf.Deps, OS: tf.OS, File: file, Skill: skill}
	isScript := slices.Contains(scriptRuntimes, tf.Runtime)
	switch {
	case tf.Runtime == "builtin" || isScript:
	case tf.Runtime == "wasm":
	default:
		return bad(file, "runtime %q isn't one of builtin, python, node, bun, sh, pwsh, wasm", tf.Runtime)
	}
	if len(tf.OS) > 0 && !slices.Contains(tf.OS, runtime.GOOS) {
		return nil, append(issues, Issue{Level: "warn", Skill: skill, Path: path.Join(dirName, file), Msg: fmt.Sprintf("tool %q is not for %s; skipped", tf.Name, runtime.GOOS)})
	}
	entry, err := resolveEntry(tf.Entry)
	if err != nil {
		return bad(file, "entry: %v", err)
	}
	if entry == "" {
		return bad(file, "tool %q has no entry for %s", tf.Name, runtime.GOOS)
	}
	if isScript || tf.Runtime == "wasm" {
		if tf.Runtime == "wasm" && !strings.HasSuffix(entry, ".wasm") {
			return bad(file, "entry %q: a wasm tool's entry is a .wasm module", entry)
		}
		p := path.Join(path.Dir(file), entry)
		if !fs.ValidPath(p) || strings.HasPrefix(entry, "/") || strings.Contains(entry, `\`) || strings.Contains(entry, ":") {
			return bad(file, "entry %q must be a relative path inside the skill folder", entry)
		}
		if fi, err := fs.Stat(fsys, p); err != nil || !fi.Mode().IsRegular() {
			return bad(file, "entry %q not found", entry)
		}
		entry = p
	}
	t.Entry = entry
	t.Timeout = 30 * time.Second
	if tf.Timeout != "" {
		d, err := time.ParseDuration(tf.Timeout)
		if err != nil || d <= 0 {
			return bad(file, "timeout %q: want a duration like 20s", tf.Timeout)
		}
		t.Timeout = min(d, 10*time.Minute)
	}
	t.MaxOutput = 16 << 10
	if tf.MaxOutput != "" {
		n, err := parseSize(tf.MaxOutput)
		if err != nil {
			return bad(file, "max_output: %v", err)
		}
		t.MaxOutput = n
	}
	if t.InputSchema, err = schemaJSON(tf.InputSchema, true); err != nil {
		return bad(file, "input_schema: %v", err)
	}
	if t.OutputSchema, err = schemaJSON(tf.OutputSchema, false); err != nil {
		return bad(file, "output_schema: %v", err)
	}
	return t, issues
}

// resolveEntry accepts "entry: x" or "entry: { windows: x.ps1, default: x.sh }".
func resolveEntry(n yaml.Node) (string, error) {
	switch n.Kind {
	case 0:
		return "", nil
	case yaml.ScalarNode:
		return n.Value, nil
	case yaml.MappingNode:
		var m map[string]string
		if err := n.Decode(&m); err != nil {
			return "", err
		}
		if v := m[runtime.GOOS]; v != "" {
			return v, nil
		}
		return m["default"], nil
	}
	return "", errors.New("want a path or a per-OS map")
}

func schemaJSON(v any, required bool) (json.RawMessage, error) {
	if v == nil {
		if required {
			return nil, nil
		}
		return nil, nil
	}
	b, err := json.Marshal(normalizeYAML(v))
	if err != nil {
		return nil, err
	}
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &probe) != nil {
		return nil, errors.New("not a JSON Schema object")
	}
	if required && probe.Type != "object" {
		return nil, errors.New(`must have "type: object" at the top`)
	}
	return b, nil
}

// normalizeYAML turns yaml.v3's map[string]any / map[any]any into JSON-encodable values.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeYAML(e)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = normalizeYAML(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = normalizeYAML(e)
		}
		return x
	}
	return v
}

func parseSize(s string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := 1
	for suf, m := range map[string]int{"kb": 1 << 10, "mb": 1 << 20, "k": 1 << 10, "m": 1 << 20} {
		if strings.HasSuffix(t, suf) {
			t, mult = strings.TrimSuffix(t, suf), m
			break
		}
	}
	n, err := strconv.Atoi(strings.TrimSpace(t))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: want a size like 16kb", s)
	}
	if n*mult > 8<<20 {
		return 0, fmt.Errorf("%q is over the 8 MB cap", s)
	}
	return n * mult, nil
}
