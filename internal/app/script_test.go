package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"noskill/internal/approvals"
	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/store"
)

var archiveAsk = regexp.MustCompile(`Make a folder (\S+)/(\w+) and move every file whose name starts with (\S+) into it\.`)

// archiveAgent does the archive chore the long way with the files skill (list, make the folder,
// move), calls archive_by_prefix once when a learned skill offers it, and plays the skill author.
func archiveAgent(author [][2]string) func(r backends.ChatRequest) (backends.Response, error) {
	return func(r backends.ChatRequest) (backends.Response, error) {
		last := r.Messages[len(r.Messages)-1]
		for _, a := range author { // in order: the first marker found wins
			if len(r.Tools) == 0 && last.Role == backends.RoleUser && strings.Contains(last.Content, a[0]) {
				return backends.Response{Content: a[1]}, nil
			}
		}
		if r.Format != nil {
			return backends.Response{Content: "{}"}, nil
		}
		var ask string
		for _, m := range r.Messages {
			if m.Role == backends.RoleUser {
				ask = m.Content
			}
		}
		m := archiveAsk.FindStringSubmatch(ask)
		done := 0
		for _, msg := range r.Messages {
			if msg.Role == backends.RoleTool {
				done++
			}
		}
		if m == nil {
			return backends.Response{Content: "I can't tell which folder."}, nil
		}
		dir, dest, prefix := m[1], m[2], m[3]
		call := func(name string, args any) (backends.Response, error) {
			b, _ := json.Marshal(args)
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: name, Args: b}}}, nil
		}
		if slices.ContainsFunc(r.Tools, func(t backends.ToolDef) bool { return t.Name == "archive_by_prefix" }) {
			if done == 0 {
				return call("archive_by_prefix", map[string]string{"folder": dir, "prefix": prefix, "dest": dest})
			}
			return backends.Response{Content: "Done."}, nil
		}
		switch done {
		case 0:
			return call("list_dir", map[string]string{"path": dir})
		case 1:
			return call("make_dir", map[string]string{"path": dir + "/" + dest})
		case 2:
			ents, _ := os.ReadDir(dir)
			var plan []map[string]string
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), prefix) {
					plan = append(plan, map[string]string{"from": dir + "/" + e.Name(), "to": dir + "/" + dest + "/" + e.Name()})
				}
			}
			return call("move_many", map[string]any{"plan": plan, "dry_run": false})
		}
		return backends.Response{Content: "Moved them."}, nil
	}
}

func archiveDir(t *testing.T, names ...string) string {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range names {
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644)
	}
	return dir
}

// LEARNING.md §3 pass d: a repeated file chore becomes a script tool that the learned skill calls
// once; verification runs the script in the sandbox and replays it, and a later run applies its
// ops through the broker.
func TestLearnedScriptToolCutsSteps(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("pass d needs the script sandbox (macOS only, ADR-0008)")
	}
	code := "import json, os, sys\nargs = json.load(sys.stdin)\nfolder = args['folder']\nops = [{'op': 'make_dir', 'path': os.path.join(folder, args['dest'])}]\n" +
		"for n in sorted(os.listdir(folder)):\n    if n.startswith(args['prefix']):\n        ops.append({'op': 'move', 'from': os.path.join(folder, n), 'to': os.path.join(folder, args['dest'], n)})\n" +
		"print(json.dumps({'ops': ops, 'count': len(ops) - 1}))\n"
	fake := &backendtest.Fake{}
	fake.Reply = archiveAgent([][2]string{
		{"Write a Python 3 tool", "tool_name: archive_by_prefix\ndescription: Move the files whose names start with a prefix into a subfolder.\n" +
			"arg prefix: 2021- | the start of the file names\narg dest: archive | the subfolder's name\n```python\n" + code + "```\n"},
		{`{"steps"`, `{"steps": ["Call ` + "`list_dir`" + `.", "Call ` + "`make_dir`" + `.", "Call ` + "`move_many`" + `.", "Answer."]}`},
		{`{"name"`, `{"name": "archive-by-prefix", "description": "Move every file whose name starts with a prefix into a new subfolder. Use when the user asks to archive files by the start of their names.", "triggers": ["whose name starts with"]}`},
		{`{"variants"`, `{"variants": []}`},
	})
	e := newApp(t, fake, noRules, "")
	t.Cleanup(func() { // run workspaces are kept, and their input copies are read-only
		filepath.WalkDir(filepath.Join(e.home, "runs"), func(p string, d os.DirEntry, err error) error {
			if err == nil {
				os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	ctx := context.Background()
	yes := func(context.Context, approvals.Item) (bool, error) { return true, nil }
	run := func(dir, prefix string, learned bool) *RunResult {
		res, err := e.app.Run(ctx, RunRequest{Messages: user("Make a folder " + dir + "/archive and move every file whose name starts with " + prefix + " into it."),
			Skills: []string{"files"}, plusLearned: learned, Allow: []string{"read:" + dir, "write:" + dir}, Prompt: yes})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	learn := func(res *RunResult) store.LearnItem {
		it, err := e.app.Learn(ctx, res.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if it, err = e.app.LearnNow(ctx, it.ID); err != nil {
			t.Fatal(err)
		}
		return it
	}

	// Without a script sandbox: plain steps.
	runner := e.app.Runner
	e.app.Runner = nil
	dir := archiveDir(t, "2021-jan.txt", "2021-feb.txt", "plan.txt")
	res := run(dir, "2021-", false)
	if _, err := os.Stat(filepath.Join(dir, "archive", "2021-jan.txt")); err != nil {
		t.Fatalf("the long way didn't move the files: %v (%s)", err, res.Answer)
	}
	if it := learn(res); it.Status != "done" || !strings.HasPrefix(it.Result, "learned archive-by-prefix ") {
		t.Fatalf("item %+v", it)
	}
	e.app.Runner = runner

	// The same chore again, now with pass d: the plain skill is replaced, not duplicated.
	res = run(archiveDir(t, "2021-mar.pdf", "2021-apr.pdf", "notes.md"), "2021-", false) // the author fake answers for 2021-
	it := learn(res)
	name, _, _ := strings.Cut(strings.TrimPrefix(it.Result, "learned "), " ")
	sk, ok := e.app.Skills.Get(name)
	if it.Status != "done" || !ok || len(sk.Tools) != 1 || sk.Tools[0].Name != "archive_by_prefix" || !slices.Contains(sk.Meta.Confirm, "archive_by_prefix") {
		log, _ := os.ReadFile(filepath.Join(e.home, "learned", name, "CHANGELOG.md"))
		t.Fatalf("item %+v skill %+v\n%s", it, sk, log)
	}
	if old, _ := e.app.Skills.Get("archive-by-prefix"); old.Status() != "archived" {
		t.Fatalf("the plain skill is %s", old.Status())
	}
	body, _ := os.ReadFile(filepath.Join(e.home, "learned", name, "SKILL.md"))
	if !strings.Contains(string(body), "`archive_by_prefix` once") {
		t.Errorf("steps:\n%s", body)
	}

	// On trial: a new folder, a new prefix, one tool call, and the broker does the moves.
	dir2 := archiveDir(t, "draft-a.md", "draft-b.md", "notes.md")
	res = run(dir2, "draft-", true)
	row, _ := e.app.DB.GetRun(ctx, res.RunID)
	if !slices.Contains(res.Skills, name) || row.ToolPattern != "archive_by_prefix" {
		t.Fatalf("skills %v, tools %q (%s)", res.Skills, row.ToolPattern, res.Answer)
	}
	for _, n := range []string{"draft-a.md", "draft-b.md"} {
		if _, err := os.Stat(filepath.Join(dir2, "archive", n)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir2, "notes.md")); err != nil {
		t.Error(err)
	}

	// A third time: covered, nothing drafted.
	res = run(archiveDir(t, "old-a.txt", "keep.txt"), "old-", false)
	if it := learn(res); it.Status != "skipped" || !strings.Contains(it.LastError, "covered by "+name) {
		t.Fatalf("item %+v", it)
	}
}

// A script redraft on trial in <name>/.next brings a tool the current version lacks; the eval's
// tools check must count it as learned (LEARNING.md §3 pass d, "Measuring").
func TestLearnedToolsSeesTrialVersion(t *testing.T) {
	e := newApp(t, &backendtest.Fake{}, noRules, "")
	dir := filepath.Join(e.home, "learned", "backup-csv")
	skill := func(version, extra string) string {
		return "---\nname: backup-csv\ndescription: Copy files into a backup folder.\nnoskill:\n  origin: learned\n  status: active\n" + extra + "  version: " + version + "\n---\n\n# Backup\n\n## Steps\n1. Copy them.\n"
	}
	os.MkdirAll(filepath.Join(dir, ".next", "tools"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill("1.0.0", "")), 0o644)
	os.WriteFile(filepath.Join(dir, ".next", "SKILL.md"), []byte(skill("1.1.0", "  tools:\n    - tools/backup_files.tool.yaml\n")), 0o644)
	os.WriteFile(filepath.Join(dir, ".next", "tools", "backup_files.tool.yaml"), []byte(`{"name": "backup_files", "description": "Back up.", "entry": "backup_files.py", "runtime": "python", "input_schema": {"type": "object"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".next", "tools", "backup_files.py"), []byte("print('{}')\n"), 0o644)
	if err := e.app.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.app.learnedTools([]string{"list_dir", "backup_files"}); !slices.Equal(got, []string{"backup_files"}) {
		t.Fatalf("learnedTools = %v, want [backup_files]", got)
	}
}
