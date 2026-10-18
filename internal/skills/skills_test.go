package skills

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"noskill/internal/backends"
)

const webSearch = `---
name: web-search
description: Search the web and read pages. Use when the user asks about current events, facts you are unsure of, or anything that needs a source.
license: MIT
allowed-tools: Bash
noskill:
  version: 1.2.0
  origin: builtin
  triggers: ["look up", "latest news on"]
  task_types: [research, chat]
  min_model: { context: 4096, tools: any }
  permissions:
    network: ["*"]
    filesystem: ["read:~/Documents"]
    secrets: ["BRAVE_API_KEY?"]
  confirm: [fetch]
  tools:
    - tools/search.tool.yaml
    - tools/fetch.tool.yaml
    - mcp: { server: searxng, tool: search }
  depends_on: []
---

# Web search

## Steps
1. Call search.
`

const searchTool = `
name: search
description: Web search. Returns title, url, snippet for each result.
runtime: python
entry: search.py
deps: ["httpx==0.27.*"]
timeout: 20s
max_output: 16kb
input_schema:
  type: object
  required: [query]
  properties:
    query: { type: string }
    max_results: { type: integer, default: 5, maximum: 10 }
`

const fetchTool = `
name: fetch
description: Fetch a page as text.
runtime: builtin
entry: web.fetch
input_schema: { type: object, properties: { url: { type: string } } }
`

func webFS() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":               {Data: []byte(webSearch)},
		"tools/search.tool.yaml": {Data: []byte(searchTool)},
		"tools/search.py":        {Data: []byte("print('{}')")},
		"tools/fetch.tool.yaml":  {Data: []byte(fetchTool)},
	}
}

func TestLoadFullSkill(t *testing.T) {
	s, issues := Load(webFS(), "web-search", "", SourceBuiltin)
	if s == nil || HasErrors(issues) {
		t.Fatalf("%v %v", s, issues)
	}
	if s.Name != "web-search" || !s.HasNoskill || s.Meta.Version != "1.2.0" || s.Origin() != "builtin" || s.Status() != "active" {
		t.Errorf("skill = %+v", s)
	}
	if !strings.HasPrefix(s.Body, "# Web search") || strings.Contains(s.Body, "---") {
		t.Errorf("body = %q", s.Body)
	}
	if len(s.Tools) != 2 {
		t.Fatalf("tools = %+v (the mcp tool is skipped with a warning)", s.Tools)
	}
	search := s.Tools[0]
	if search.Name != "search" || search.Entry != "tools/search.py" || search.Timeout.Seconds() != 20 || search.MaxOutput != 16<<10 ||
		!strings.Contains(string(search.InputSchema), `"required":["query"]`) {
		t.Errorf("search = %+v / %s", search, search.InputSchema)
	}
	if s.Tools[1].Entry != "web.fetch" || s.Tools[1].Runtime != "builtin" {
		t.Errorf("fetch = %+v", s.Tools[1])
	}
	p := s.Meta.Permissions
	if len(p.Network) != 1 || p.Network[0] != "*" || p.SecretList()[0] != (Secret{"BRAVE_API_KEY", true}) {
		t.Errorf("permissions = %+v", p)
	}
	var mcp bool
	for _, i := range issues {
		mcp = mcp || strings.Contains(i.Msg, "MCP")
	}
	if !mcp {
		t.Errorf("no MCP warning: %v", issues)
	}
	if len(s.Hash) != 64 {
		t.Errorf("hash = %q", s.Hash)
	}
	// The hash follows the content.
	fsys := webFS()
	fsys["tools/search.py"] = &fstest.MapFile{Data: []byte("print('changed')")}
	s2, _ := Load(fsys, "web-search", "", SourceBuiltin)
	if s2.Hash == s.Hash {
		t.Error("hash didn't change when a tool script changed")
	}
}

// A skill written for another runtime must load unchanged (AGENTS.md).
func TestPlainSkillLoads(t *testing.T) {
	for name, md := range map[string]string{
		"minimal":  "---\nname: pdf-tools\ndescription: Work with PDFs.\n---\nDo things.\n",
		"crlf+bom": "\xef\xbb\xbf---\r\nname: pdf-tools\r\ndescription: Work with PDFs.\r\nmetadata:\r\n  author: x\r\n---\r\nBody\r\n",
		"no body":  "---\nname: pdf-tools\ndescription: Work with PDFs.\n---",
		"hooks":    "---\nname: pdf-tools\ndescription: Work with PDFs.\nallowed-tools: [Bash, Read]\nhooks: {PreToolUse: []}\n---\nx",
	} {
		s, issues := Load(fstest.MapFS{"SKILL.md": {Data: []byte(md)}}, "pdf-tools", "", SourceUser)
		if s == nil {
			t.Errorf("%s: %v", name, issues)
			continue
		}
		if s.HasNoskill || len(s.Tools) != 0 || len(s.Meta.Permissions.Network) != 0 || s.Origin() != "user" {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

func TestUnquotedOptionalSecretGetsAHint(t *testing.T) {
	md := "---\nname: x\ndescription: d\nnoskill:\n  permissions:\n    secrets: [KEY?]\n---\n"
	s, issues := Load(fstest.MapFS{"SKILL.md": {Data: []byte(md)}}, "x", "", SourceUser)
	if s != nil || len(issues) != 1 || !strings.Contains(issues[0].Msg, `must be quoted`) {
		t.Fatalf("%v", issues)
	}
}

func TestBadSkillsAreRejectedWithAReason(t *testing.T) {
	for want, md := range map[string]string{
		"must start with":  "name: x\n",
		"not closed":       "---\nname: x\ndescription: d\n",
		"no name":          "---\ndescription: d\n---\n",
		"no description":   "---\nname: x\n---\n",
		"name \"../evil\"": "---\nname: ../evil\ndescription: d\n---\n",
		"front-matter:":    "---\nname: [unclosed\n---\n",
		"want a list":      "---\nname: x\ndescription: d\nnoskill:\n  permissions:\n    network: [[a]]\n---\n",
	} {
		s, issues := Load(fstest.MapFS{"SKILL.md": {Data: []byte(md)}}, "x", "", SourceUser)
		if s != nil || len(issues) == 0 || !strings.Contains(issues[len(issues)-1].Msg, want) {
			t.Errorf("%q: s=%v issues=%v", want, s != nil, issues)
		}
	}
	if s, _ := Load(fstest.MapFS{}, "x", "", SourceUser); s != nil {
		t.Error("folder without SKILL.md loaded")
	}
}

func TestToolValidation(t *testing.T) {
	skill := func(tool string, files ...string) []Issue {
		fsys := fstest.MapFS{
			"SKILL.md":      {Data: []byte("---\nname: s\ndescription: d\nnoskill:\n  tools: [t/a.tool.yaml]\n---\n")},
			"t/a.tool.yaml": {Data: []byte(tool)},
		}
		for _, f := range files {
			fsys[f] = &fstest.MapFile{Data: []byte("x")}
		}
		s, issues := Load(fsys, "s", "", SourceUser)
		if s == nil {
			t.Fatal("skill itself must still load")
		}
		if len(s.Tools) != 0 {
			return nil // loaded fine
		}
		return issues
	}
	ok := "name: a\ndescription: d\nruntime: python\nentry: a.py\ninput_schema: {type: object}\n"
	if skill(ok, "t/a.py") != nil {
		t.Fatal("valid tool rejected")
	}
	for want, tool := range map[string]string{
		"not found":               strings.Replace(ok, "a.py", "missing.py", 1),
		"inside the skill folder": strings.Replace(ok, "a.py", "../../etc/passwd", 1),
		"relative path":           strings.Replace(ok, "a.py", "/etc/passwd", 1),
		"no description":          strings.Replace(ok, "description: d\n", "", 1),
		"runtime \"ruby\"":        strings.Replace(ok, "python", "ruby", 1),
		"type: object":            strings.Replace(ok, "{type: object}", "{type: string}", 1),
		"timeout \"soon\"":        ok + "timeout: soon\n",
		"over the 8 MB cap":       ok + "max_output: 99mb\n",
		"tool name \"1bad\"":      strings.Replace(ok, "name: a", "name: 1bad", 1),
	} {
		issues := skill(tool, "t/a.py")
		found := false
		for _, i := range issues {
			found = found || strings.Contains(i.Msg, want)
		}
		if !found {
			t.Errorf("%q: %v", want, issues)
		}
	}
}

func TestPerOSEntryAndOSRestriction(t *testing.T) {
	tool := "name: a\ndescription: d\nruntime: sh\nentry: { windows: a.ps1, default: a.sh }\ninput_schema: {type: object}\n"
	fsys := fstest.MapFS{
		"SKILL.md":    {Data: []byte("---\nname: s\ndescription: d\nnoskill:\n  tools: [a.tool.yaml]\n---\n")},
		"a.tool.yaml": {Data: []byte(tool)},
		"a.sh":        {Data: []byte("x")},
		"a.ps1":       {Data: []byte("x")},
	}
	s, issues := Load(fsys, "s", "", SourceUser)
	want := "a.sh"
	if runtime.GOOS == "windows" {
		want = "a.ps1"
	}
	if s == nil || len(s.Tools) != 1 || s.Tools[0].Entry != want {
		t.Fatalf("%+v %v", s, issues)
	}
	other := "darwin"
	if runtime.GOOS == "darwin" {
		other = "linux"
	}
	fsys["a.tool.yaml"] = &fstest.MapFile{Data: []byte(tool + "os: [" + other + "]\n")}
	s, issues = Load(fsys, "s", "", SourceUser)
	if len(s.Tools) != 0 || HasErrors(issues) {
		t.Errorf("OS-restricted tool: %+v %v", s.Tools, issues)
	}
}

func writeSkill(t *testing.T, root, name, desc string, extra string) {
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + desc + "\n" + extra + "---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSetPrecedenceAndTrust(t *testing.T) {
	builtin, user, project := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, builtin, "files", "builtin files", "")
	writeSkill(t, builtin, "web-search", "builtin search", "")
	writeSkill(t, user, "web-search", "my search", "")
	writeSkill(t, project, "files", "project files claiming the builtin's name", "")
	writeSkill(t, project, "repo-tool", "added by the repo", "")
	os.MkdirAll(filepath.Join(user, "not-a-skill"), 0o755)
	if runtime.GOOS != "windows" {
		os.Symlink(builtin, filepath.Join(user, "linked"))
	}
	set, issues := LoadAll([]Root{DiskRoot(SourceBuiltin, builtin), DiskRoot(SourceUser, user), DiskRoot(SourceProject, project), DiskRoot(SourceUser, "/definitely/missing")})
	if HasErrors(issues) {
		t.Fatal(issues)
	}
	get := func(n string) *Skill { s, _ := set.Get(n); return s }
	if get("web-search").Description != "my search" || get("web-search").Source != SourceUser {
		t.Error("a user skill must replace the built-in of the same name")
	}
	if get("files").Source != SourceBuiltin || get("files").Description != "builtin files" {
		t.Error("a project skill must never replace an existing one")
	}
	if rs := get("repo-tool"); rs == nil || !rs.NeedsApproval() || rs.Origin() != "imported" {
		t.Errorf("project skill = %+v", rs)
	}
	if len(set.Shadows) != 2 {
		t.Errorf("shadows = %+v", set.Shadows)
	}
	var names []string
	for _, s := range set.All() {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "files,repo-tool,web-search" {
		t.Errorf("all = %v (linked and non-skill folders must be ignored)", names)
	}
}

func TestToolNamesAndClosure(t *testing.T) {
	mk := func(name, deps string) *Skill {
		return &Skill{Name: name, Meta: Meta{DependsOn: strings.Fields(deps)}, Tools: []Tool{{Name: "search"}, {Name: name + "-only"}}}
	}
	a, b := mk("web-search", ""), mk("local-docs", "")
	rs := Tools([]*Skill{a, b})
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, ","); got != "web_search__search,web-search-only,local_docs__search,local-docs-only" {
		t.Errorf("names = %s", got)
	}
	set := &Set{byName: map[string]*Skill{"a": mk("a", "b c"), "b": mk("b", "c"), "c": mk("c", ""), "x": mk("x", "y"), "y": mk("y", "x"), "m": mk("m", "ghost")}}
	cl, err := set.Closure("a")
	if err != nil || len(cl) != 3 || cl[0].Name != "c" || cl[2].Name != "a" {
		t.Errorf("closure = %v %v", cl, err)
	}
	if _, err := set.Closure("x"); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Errorf("cycle: %v", err)
	}
	if _, err := set.Closure("m"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("missing: %v", err)
	}
}

// ---- selector ----

// bagEmbedder hashes words into a small vector, so texts sharing words are close.
type bagEmbedder struct{ calls int }

func (b *bagEmbedder) Embed(_ context.Context, req backends.EmbedRequest) ([][]float32, error) {
	b.calls++
	out := make([][]float32, len(req.Input))
	for i, text := range req.Input {
		v := make([]float32, 64)
		for _, w := range tokens(text) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[h.Sum32()%64]++
		}
		out[i] = v
	}
	return out, nil
}

type memCache struct{ m map[string][]float32 }

func (c *memCache) GetSkillVector(_ context.Context, n, h, m string) ([]float32, bool, error) {
	v, ok := c.m[n+h+m]
	return v, ok, nil
}
func (c *memCache) PutSkillVector(_ context.Context, n, h, m string, v []float32) error {
	c.m[n+h+m] = v
	return nil
}

func candidates() []*Skill {
	return []*Skill{
		{Name: "web-search", Hash: "h1", Description: "Search the web and read pages for current events and facts that need a source.", Meta: Meta{Triggers: []string{"look up"}}},
		{Name: "files", Hash: "h2", Description: "Move, copy, rename and organise files and folders on this computer."},
		{Name: "model-manager", Hash: "h3", Description: "List, pull and remove local models and show how much memory they use."},
	}
}

func TestSelectByEmbeddingCachesVectors(t *testing.T) {
	emb, cache := &bagEmbedder{}, &memCache{m: map[string][]float32{}}
	sel := &Selector{Embedder: emb, EmbedModel: "nomic-embed-text", Cache: cache, MinScore: 0.2}
	ms, err := sel.Select(context.Background(), candidates(), "search the web for current events", 3)
	if err != nil || len(ms) == 0 || ms[0].Skill.Name != "web-search" || ms[0].Method != "embedding" {
		t.Fatalf("%+v %v", ms, err)
	}
	if len(cache.m) != 3 {
		t.Errorf("cached %d vectors, want 3", len(cache.m))
	}
	before := emb.calls
	sel.Select(context.Background(), candidates(), "organise my folders", 3)
	if emb.calls != before+1 {
		t.Errorf("a second query must embed only the query (one call), calls %d→%d", before, emb.calls)
	}
	ms, _ = sel.Select(context.Background(), candidates(), "organise my files and folders", 1)
	if len(ms) != 1 || ms[0].Skill.Name != "files" {
		t.Errorf("%+v", ms)
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, backends.EmbedRequest) ([][]float32, error) {
	return nil, &backends.Error{Backend: "ollama", Status: 404, Msg: "model not found"}
}

func TestSelectFallsBackToKeywords(t *testing.T) {
	for name, sel := range map[string]*Selector{
		"no embedder":   {},
		"embed failing": {Embedder: failingEmbedder{}, EmbedModel: "nomic-embed-text"},
	} {
		ms, err := sel.Select(context.Background(), candidates(), "please rename these files in my folder", 3)
		if err != nil || len(ms) == 0 || ms[0].Skill.Name != "files" || ms[0].Method != "keyword" {
			t.Errorf("%s: %+v %v", name, ms, err)
		}
		ms, _ = sel.Select(context.Background(), candidates(), "tell me a joke about cats", 3)
		if len(ms) != 0 {
			t.Errorf("%s: unrelated request matched %v", name, ms[0].Skill.Name)
		}
	}
}

func TestTriggerPhraseBoostsAndLimits(t *testing.T) {
	sel := &Selector{}
	ms, _ := sel.Select(context.Background(), candidates(), "look up zebra", 3)
	if len(ms) != 1 || ms[0].Skill.Name != "web-search" || ms[0].Method != "trigger" {
		t.Fatalf("%+v", ms)
	}
	ms, _ = sel.Select(context.Background(), candidates(), "files models web memory", 2)
	if len(ms) > 2 {
		t.Errorf("k not applied: %d", len(ms))
	}
	if ms, _ := sel.Select(context.Background(), nil, "x", 3); ms != nil {
		t.Error("no candidates")
	}
}

func TestColonInDescriptionGetsAHint(t *testing.T) {
	md := "---\nname: x\ndescription: Work with PDFs: split and merge them.\n---\n"
	s, issues := Load(fstest.MapFS{"SKILL.md": {Data: []byte(md)}}, "x", "", SourceUser)
	if s != nil || len(issues) != 1 || !strings.Contains(issues[0].Msg, "must be quoted") {
		t.Fatalf("%v", issues)
	}
}

func TestSetMetaKeepsTheRest(t *testing.T) {
	in := "---\nname: x\ndescription: \"Does: things\"\nnoskill:\n  status: candidate\n  triggers: [a, b]\n  custom: 1\n---\n# Body\n\n- step\n"
	out, err := SetMeta([]byte(in), map[string]string{"status": "active", "version": "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	s, issues := Load(fstest.MapFS{"SKILL.md": {Data: out}}, "x", "", SourceLearned)
	if s == nil {
		t.Fatalf("%v\n%s", issues, out)
	}
	if s.Status() != "active" || s.Meta.Version != "1.1.0" || len(s.Meta.Triggers) != 2 || s.Description != "Does: things" || s.Body != "# Body\n\n- step" {
		t.Fatalf("%+v\n%s", s.Meta, out)
	}
	if !strings.Contains(string(out), "custom: 1") {
		t.Errorf("unknown key dropped:\n%s", out)
	}
	plain, err := SetMeta([]byte("---\nname: y\ndescription: d\n---\nbody"), map[string]string{"status": "stale"})
	if err != nil || !strings.HasSuffix(string(plain), "noskill:\n  status: stale\n---\nbody") {
		t.Fatalf("%v\n%s", err, plain)
	}
}

// Learned skills match on their example request both ways, ignore temp-folder paths, and need
// 0.6 (block 2c: "combine every .txt" picked filter-error-lines, a new archive prefix found nothing).
func TestLearnedKeywordMatching(t *testing.T) {
	learned := func(name, desc, example string) *Skill {
		return &Skill{Name: name, Source: SourceLearned, Description: desc + ` Example request: "` + example + `"`}
	}
	cands := []*Skill{
		learned("filter-error-lines", "Reads a log file and writes the lines containing a word to a new file.",
			"Read …, write every line that contains … to …, and tell me how many there were. End with just the number."),
		learned("merge-text-files", "Reads all text files in a folder and joins them into one file.",
			"Read every … file in … and write a new file … that contains all of their lines."),
		learned("archive-by-prefix", "Moves files whose names start with a prefix into a new folder.",
			"Make a folder … and move every file whose name starts with … into it."),
	}
	dir := "/private/var/folders/jv/bksh2c754wz70t0snylh3vz00000gn/T/noskill-eval-1234567"
	sel := &Selector{}
	for q, want := range map[string]string{
		"Read every .txt file in " + dir + " and write a new file " + dir + "/all.txt that contains all of their lines.":                "merge-text-files",
		"Read " + dir + "/worker.log, write every line that contains ERROR to " + dir + "/errors.txt, and tell me how many there were.": "filter-error-lines",
		"Make a folder " + dir + "/done and move every file whose name starts with closed- into it.":                                    "archive-by-prefix",
		"How many files are in " + dir + "? Just the number.":                                                                           "",
		"Read " + dir + "/notes.txt and tell me which day the meeting moved to.":                                                        "",
	} {
		ms, err := sel.Select(context.Background(), cands, q, 1)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if len(ms) > 0 {
			got = ms[0].Skill.Name
		}
		if got != want {
			t.Errorf("%q: got %q %v, want %q", q, got, ms, want)
		}
	}
}
