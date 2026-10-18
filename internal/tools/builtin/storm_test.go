package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/tools"
)

// notesSvc is the one service the notes tool needs: a folder for the run's notes.
type notesSvc struct {
	tools.Services
	dir string
}

func (n notesSvc) NotesDir(string) (string, error) { return n.dir, nil }

func callNotes(t *testing.T, e *tools.Env, args string) (map[string]any, error) {
	t.Helper()
	for _, x := range Models() {
		if x.ID == "notes" {
			out, err := x.Run(context.Background(), e, json.RawMessage(args))
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("notes returned %q", out)
			}
			return m, nil
		}
	}
	t.Fatal("no notes tool")
	return nil, nil
}

// STORM keeps one lane of notes per viewpoint, lists them, and builds the numbered reference pool.
func TestNotesLanesAndSources(t *testing.T) {
	e := &tools.Env{Svc: notesSvc{dir: t.TempDir()}}
	for _, a := range []string{
		`{"action":"append","name":"practitioner","text":"Q1 A: it works in production [https://a.example/guide.]"}`,
		`{"action":"append","name":"practitioner","text":"Q2 A: also see https://b.example/paper, and https://a.example/guide"}`,
		`{"action":"append","name":"skeptic","text":"Q1 A: the claim has one source https://b.example/paper"}`,
		`{"action":"append","name":"Skeptic","text":"Q2 A: counter-claim https://c.example/rebuttal)"}`,
		`{"action":"append","text":"a main-notes line"}`,
	} {
		if _, err := callNotes(t, e, a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	m, err := callNotes(t, e, `{"action":"lanes"}`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range m["lanes"].([]any) {
		lane := l.(map[string]any)
		names = append(names, lane["name"].(string))
		if lane["name"] == "skeptic" && lane["lines"].(float64) != 2 {
			t.Errorf("Skeptic and skeptic are one lane: %v", lane)
		}
	}
	if strings.Join(names, ",") != "main,practitioner,skeptic" {
		t.Errorf("lanes %v", names)
	}

	// One lane, whole.
	m, _ = callNotes(t, e, `{"action":"read","name":"skeptic"}`)
	if s := m["notes"].(string); !strings.Contains(s, "one source") || strings.Contains(s, "production") {
		t.Errorf("read skeptic: %q", s)
	}
	// All lanes, each under its name.
	m, _ = callNotes(t, e, `{"action":"read"}`)
	if s := m["notes"].(string); !strings.Contains(s, "## practitioner") || !strings.Contains(s, "## skeptic") || !strings.Contains(s, "main-notes") {
		t.Errorf("read all: %q", s)
	}

	// The reference pool: each URL once, numbered, trailing punctuation dropped, lanes named.
	m, _ = callNotes(t, e, `{"action":"sources"}`)
	list := m["list"].(string)
	want := "[1] https://a.example/guide\n[2] https://b.example/paper\n[3] https://c.example/rebuttal\n"
	if list != want {
		t.Errorf("sources:\n%s\nwant:\n%s", list, want)
	}
	second := m["sources"].([]any)[1].(map[string]any)
	if lanes := second["lanes"].([]any); len(lanes) != 2 || lanes[0] != "practitioner" || lanes[1] != "skeptic" {
		t.Errorf("source 2 lanes: %v", lanes)
	}

	// Names that could leave the folder are refused.
	for _, bad := range []string{"../x", "a/b", "", ".hidden", "x y"} {
		args, _ := json.Marshal(map[string]string{"action": "append", "name": bad, "text": "x"})
		_, err := callNotes(t, e, string(args))
		if bad == "" {
			if err != nil {
				t.Errorf("an empty name is the main notes: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("lane name %q was accepted", bad)
		}
	}
}

// A long lane reads as its newest part, and says so, so a small model's context isn't overrun.
func TestNotesReadIsCappedToTheNewestPart(t *testing.T) {
	e := &tools.Env{Svc: notesSvc{dir: t.TempDir()}}
	for i := 0; i < 40; i++ {
		args, _ := json.Marshal(map[string]string{"action": "append", "name": "history", "text": strings.Repeat("x", 100) + string(rune('A'+i%26))})
		if _, err := callNotes(t, e, string(args)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := callNotes(t, e, `{"action":"read","name":"history","max_chars":500}`)
	s := m["notes"].(string)
	if len([]rune(s)) > 500 || !strings.Contains(m["note"].(string), "newest") || !strings.HasPrefix(s, "- x") {
		t.Errorf("%d chars, note %v, starts %q", len([]rune(s)), m["note"], s[:10])
	}
	m, _ = callNotes(t, e, `{"action":"read","name":"history","max_chars":20000}`)
	if _, cut := m["note"]; cut || len(m["notes"].(string)) < 4000 {
		t.Errorf("a roomy cap shouldn't cut: %v", m["note"])
	}
	// The pile of lanes together is bounded.
	big := strings.Repeat("y", 2000)
	var err error
	for i := 0; i < 40 && err == nil; i++ {
		_, err = callNotes(t, e, `{"action":"append","name":"more","text":"`+big+`"}`)
	}
	if err == nil || !strings.Contains(err.Error(), "full") {
		t.Errorf("the notes never filled up: %v", err)
	}
}

// join_files makes the report from its section files, in order, and never replaces a file.
func TestJoinFiles(t *testing.T) {
	f := newFx(t)
	a := f.write("r/01.md", "## One\nfirst\n")
	b := f.write("r/02.md", "## Two\nsecond")
	to := filepath.Join(f.root, "r", "report.md")
	m, err := call(t, "join_files", f.env, map[string]any{"from": []string{b, a}, "to": to})
	if err != nil || m["files"].(float64) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	if got, _ := os.ReadFile(to); string(got) != "## Two\nsecond\n\n## One\nfirst\n" {
		t.Errorf("joined: %q", got)
	}
	if _, err := call(t, "join_files", f.env, map[string]any{"from": []string{a}, "to": to}); err == nil || !strings.Contains(err.Error(), "never replaced") {
		t.Errorf("replacing: %v", err)
	}
	if _, err := call(t, "join_files", f.env, map[string]any{"from": []string{filepath.Join(f.outside, "x.md")}, "to": filepath.Join(f.root, "r", "o.md")}); err == nil {
		t.Error("a source outside the allowed folder was read")
	}
	if _, err := call(t, "join_files", f.env, map[string]any{"from": []string{filepath.Join(f.root, "missing.md")}, "to": filepath.Join(f.root, "r", "m.md")}); err == nil {
		t.Error("a missing source")
	}
	f.write("r/bin", string([]byte{0xff, 0xfe, 0x00}))
	if _, err := call(t, "join_files", f.env, map[string]any{"from": []string{filepath.Join(f.root, "r", "bin")}, "to": filepath.Join(f.root, "r", "b.md")}); err == nil {
		t.Error("a binary file was joined")
	}
	if _, err := call(t, "join_files", f.env, map[string]any{"from": []string{}, "to": to}); err == nil {
		t.Error("no sources")
	}
}

// stormSvc answers storm_run with a canned result; the tool writes its files in a new folder under
// the skill's folder, never in an existing one.
type stormSvc struct {
	tools.Services
	res *tools.StormResult
}

func (s stormSvc) Storm(_ context.Context, topic string, n int) (*tools.StormResult, error) {
	return s.res, nil
}

func TestStormRunWritesANewFolder(t *testing.T) {
	f := newFx(t)
	f.env.Expand = func(p string) (string, error) { return strings.Replace(p, "$DOCUMENTS", f.root, 1), nil }
	f.env.Svc = stormSvc{res: &tools.StormResult{Summary: "short answer [1]", Sections: 1, Sources: 1, Answered: 2, Files: []tools.StormFile{
		{Name: "report.md", Content: "# Topic\n"}, {Name: "outline.md", Content: "1. One\n"}}}}
	var b tools.Builtin
	for _, x := range Storm() {
		b = x
	}
	run := func() map[string]any {
		out, err := b.Run(context.Background(), f.env, json.RawMessage(`{"topic":"Small Models on Laptops!"}`))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal([]byte(out), &m)
		return m
	}
	first, second := run(), run()
	p1, p2 := first["folder"].(string), second["folder"].(string)
	if p1 == p2 || !strings.Contains(p1, "storm-small-models-on-laptops-") || !strings.Contains(p2, "-2") {
		t.Errorf("folders %q %q", p1, p2)
	}
	if got, _ := os.ReadFile(filepath.Join(p1, "report.md")); string(got) != "# Topic\n" {
		t.Errorf("report.md: %q", got)
	}
	if want := "short answer [1]\n\nFull report: " + filepath.Join(p1, "report.md"); first["summary"] != want || first["report"] != filepath.Join(p1, "report.md") {
		t.Errorf("result %v", first)
	}
	if _, err := b.Run(context.Background(), f.env, json.RawMessage(`{"topic":"x"}`)); err == nil {
		t.Error("a too-short topic")
	}
}
