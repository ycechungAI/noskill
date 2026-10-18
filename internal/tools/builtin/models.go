package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"noskill/internal/tools"
)

var errNoSvc = errors.New("this tool isn't available in this run")

func needSvc(env *tools.Env) error {
	if env.Svc == nil {
		return errNoSvc
	}
	return nil
}

// Models returns the model-manager, orchestrator and research tools. They call into the rest of
// noskill through tools.Services, so they hold no state and import nothing heavy.
func Models() []tools.Builtin {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }
	name := obj(`{"type":"object","required":["name"],"properties":{"name":{"type":"string","description":"Model name such as qwen3:4b"}}}`)
	return []tools.Builtin{
		{ID: "list_models", Description: "List installed models: size, capabilities, whether loaded.", Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
			if err := needSvc(env); err != nil {
				return "", err
			}
			ms, err := env.Svc.ListModels(ctx)
			if err != nil {
				return "", err
			}
			return jsonOut(ms)
		}},
		{ID: "machine_info", Description: "Memory, model memory budget and free disk space of this machine.", Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
			if err := needSvc(env); err != nil {
				return "", err
			}
			m, err := env.Svc.MachineInfo(ctx)
			if err != nil {
				return "", err
			}
			return jsonOut(m)
		}},
		{ID: "suggest_models", Description: "Suggest models for a need that fit this machine's memory budget.",
			Schema: obj(`{"type":"object","required":["need"],"properties":{"need":{"type":"string","description":"A task type (code, vision, chat…) or capability (embed, decide)"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct{ Need string }
				json.Unmarshal(raw, &a)
				s, err := env.Svc.SuggestModels(ctx, a.Need)
				if err != nil {
					return "", err
				}
				return jsonOut(s)
			}},
		{ID: "pull_model", Description: "Download a model. Needs the user's approval.", Schema: name,
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct{ Name string }
				json.Unmarshal(raw, &a)
				r, err := env.Svc.PullModel(ctx, a.Name)
				if err != nil {
					return "", err
				}
				return jsonOut(map[string]string{"pulled": a.Name, "result": r})
			}},
		{ID: "remove_model", Description: "Remove an installed model. Needs the user's approval.", Schema: name,
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct{ Name string }
				json.Unmarshal(raw, &a)
				if err := env.Svc.RemoveModel(ctx, a.Name); err != nil {
					return "", err
				}
				return jsonOut(map[string]string{"removed": a.Name})
			}},
		{ID: "bench_model", Description: "Measure a model's speed and memory use.", Schema: name,
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct{ Name string }
				json.Unmarshal(raw, &a)
				r, err := env.Svc.BenchModel(ctx, a.Name)
				if err != nil {
					return "", err
				}
				return jsonOut(map[string]string{"model": a.Name, "result": r})
			}},
		{ID: "route", Description: "Which model would handle this task, and why. Doesn't run anything.",
			Schema: obj(`{"type":"object","required":["task"],"properties":{"task":{"type":"string"},"task_type":{"type":"string","description":"chat, code, vision, reasoning, summarise…"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct {
					Task     string
					TaskType string `json:"task_type"`
				}
				json.Unmarshal(raw, &a)
				r, err := env.Svc.Route(ctx, a.Task, a.TaskType)
				if err != nil {
					return "", err
				}
				return jsonOut(r)
			}},
		{ID: "delegate", Description: "Run a self-contained sub-task on another model. Include everything it needs in the task text: it can't see this conversation. For several independent sub-tasks, pass them all in tasks: they're grouped by model so each is loaded once.",
			Schema: obj(`{"type":"object","properties":{"task":{"type":"string","minLength":1},"task_type":{"type":"string","description":"code, vision, summarise, reasoning, extract…"},"model":{"type":"string","description":"Only when the user asked for a specific model"},"tasks":{"type":"array","maxItems":6,"description":"Independent sub-tasks (none needs another's result)","items":{"type":"object","required":["task"],"properties":{"task":{"type":"string","minLength":1},"task_type":{"type":"string"},"model":{"type":"string"}}}}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct {
					Task     string
					TaskType string `json:"task_type"`
					Model    string
					Tasks    []tools.SubTask
				}
				json.Unmarshal(raw, &a)
				if len(a.Tasks) > 0 {
					if a.Task != "" {
						a.Tasks = append([]tools.SubTask{{Task: a.Task, TaskType: a.TaskType, Model: a.Model}}, a.Tasks...)
					}
					res, err := env.Svc.DelegateAll(ctx, a.Tasks)
					if err != nil {
						return "", err
					}
					return jsonOut(res)
				}
				if a.Task == "" {
					return "", fmt.Errorf("give a task, or several in tasks")
				}
				return env.Svc.Delegate(ctx, a.Task, a.TaskType, a.Model)
			}},
		{ID: "plan_questions", Description: "Break a research goal into 3–6 sub-questions.",
			Schema: obj(`{"type":"object","required":["goal"],"properties":{"goal":{"type":"string","minLength":1},"count":{"type":"integer","default":4,"minimum":2,"maximum":8}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				if err := needSvc(env); err != nil {
					return "", err
				}
				var a struct {
					Goal  string
					Count int
				}
				json.Unmarshal(raw, &a)
				if a.Count <= 0 {
					a.Count = 4
				}
				qs, err := env.Svc.PlanQuestions(ctx, a.Goal, a.Count)
				if err != nil {
					return "", err
				}
				return jsonOut(map[string]any{"questions": qs})
			}},
		{ID: "notes", Description: "Keep scratch notes for this task, in separate lanes when you give a name (one per viewpoint, say). append adds a line (fact + source URL); read returns a lane (all of them if no name), the newest part when it is long; lanes lists the lanes; sources lists every distinct URL in all the notes, numbered, for a reference list.",
			Schema: obj(`{"type":"object","required":["action"],"properties":{"action":{"type":"string","enum":["append","read","lanes","sources"]},"name":{"type":"string","maxLength":32,"description":"The lane: letters, digits and - only. Leave out for the main notes."},"text":{"type":"string","maxLength":2000},"max_chars":{"type":"integer","default":6000,"minimum":500,"maximum":20000}}}`),
			Run:    notes},
	}
}

const maxNotes = 64 << 10

var laneName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}$`)

// noteFiles maps lane names to files: the main notes are notes.md, a lane is notes-<name>.md.
func noteFile(dir, name string) (string, error) {
	if name == "" || name == "main" {
		return filepath.Join(dir, "notes.md"), nil
	}
	if !laneName.MatchString(name) {
		return "", errors.New("a lane name is letters, digits and - only (up to 32)")
	}
	return filepath.Join(dir, "notes-"+strings.ToLower(name)+".md"), nil
}

// noteLanes lists the lane files of a notes folder, main first, with their names.
func noteLanes(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		n := e.Name()
		switch {
		case n == "notes.md":
			out = append([]string{"main"}, out...)
		case strings.HasPrefix(n, "notes-") && strings.HasSuffix(n, ".md"):
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(n, "notes-"), ".md"))
		}
	}
	return out
}

var urlInNotes = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

// NoteSource is one distinct URL in a run's notes, numbered for citing.
type NoteSource struct {
	N     int      `json:"n"`
	URL   string   `json:"url"`
	Lanes []string `json:"lanes"`
}

// NoteSources lists every distinct URL in the notes of dir, numbered in the order first seen (the
// main notes first, then lanes by name), with the lanes that used each.
func NoteSources(dir string) []NoteSource {
	index := map[string]int{}
	var out []NoteSource
	lanes := noteLanes(dir)
	sort.SliceStable(lanes, func(i, j int) bool { return lanes[i] != "main" && lanes[j] != "main" && lanes[i] < lanes[j] })
	for _, n := range lanes {
		p, _ := noteFile(dir, n)
		b, _ := os.ReadFile(p)
		for _, u := range urlInNotes.FindAllString(string(b), -1) {
			u = strings.TrimRight(u, ".,;:!?")
			i, ok := index[u]
			if !ok {
				out = append(out, NoteSource{N: len(out) + 1, URL: u})
				i = len(out) - 1
				index[u] = i
			}
			if !slices.Contains(out[i].Lanes, n) {
				out[i].Lanes = append(out[i].Lanes, n)
			}
		}
	}
	return out
}

// NoteLanes lists the lanes of dir (main first).
func NoteLanes(dir string) []string { return noteLanes(dir) }

// ReadLane returns a lane's lines (without the leading "- ").
func ReadLane(dir, lane string) []string {
	p, err := noteFile(dir, lane)
	if err != nil {
		return nil
	}
	b, _ := os.ReadFile(p)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(strings.TrimPrefix(l, "- ")); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// AppendNote adds one line to a lane of dir, the way the notes tool does, within the same size cap.
func AppendNote(dir, lane, text string) error {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" {
		return errors.New("nothing to note")
	}
	p, err := noteFile(dir, lane)
	if err != nil {
		return err
	}
	var total int64
	for _, n := range noteLanes(dir) {
		lp, _ := noteFile(dir, n)
		if fi, err := os.Stat(lp); err == nil {
			total += fi.Size()
		}
	}
	if total+int64(len(text)) > maxNotes {
		return fmt.Errorf("notes are full (%d KB)", maxNotes>>10)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("- " + text + "\n")
	return err
}

func notes(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	if err := needSvc(env); err != nil {
		return "", err
	}
	var a struct {
		Action, Text, Name string
		MaxChars           int `json:"max_chars"`
	}
	json.Unmarshal(raw, &a)
	dir, err := env.Svc.NotesDir(env.RunID)
	if err != nil {
		return "", err
	}
	if a.MaxChars <= 0 {
		a.MaxChars = 6000
	}
	switch a.Action {
	case "lanes":
		type lane struct {
			Name  string `json:"name"`
			Lines int    `json:"lines"`
			Chars int    `json:"chars"`
		}
		var out []lane
		for _, n := range noteLanes(dir) {
			p, _ := noteFile(dir, n)
			b, _ := os.ReadFile(p)
			out = append(out, lane{n, strings.Count(string(b), "\n"), len(b)})
		}
		return jsonOut(map[string]any{"lanes": out})
	case "sources":
		// The reference pool: each distinct URL in any lane once, numbered, with the lanes that used
		// it. Done here so no model has to merge reference lists.
		out := NoteSources(dir)
		var list strings.Builder
		for _, s := range out {
			fmt.Fprintf(&list, "[%d] %s\n", s.N, s.URL)
		}
		return jsonOut(map[string]any{"sources": out, "list": list.String()})
	case "read":
		if a.Name == "" && len(noteLanes(dir)) > 1 {
			// Everything, lane by lane: only the newest part of each fits a small model's context.
			var b strings.Builder
			for _, n := range noteLanes(dir) {
				p, _ := noteFile(dir, n)
				c, _ := os.ReadFile(p)
				fmt.Fprintf(&b, "## %s\n%s\n", n, c)
			}
			return capNotes(b.String(), a.MaxChars)
		}
		p, err := noteFile(dir, a.Name)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return jsonOut(map[string]any{"notes": "", "note": "nothing saved yet"})
		}
		if err != nil {
			return "", err
		}
		return capNotes(string(b), a.MaxChars)
	case "append":
		text := strings.TrimSpace(a.Text)
		if text == "" {
			return "", errors.New(`append needs "text"`)
		}
		p, err := noteFile(dir, a.Name)
		if err != nil {
			return "", err
		}
		var total int64
		for _, n := range noteLanes(dir) {
			lp, _ := noteFile(dir, n)
			if fi, err := os.Stat(lp); err == nil {
				total += fi.Size()
			}
		}
		if total+int64(len(text)) > maxNotes {
			return "", fmt.Errorf("notes are full (%d KB); read them and write the report", maxNotes>>10)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString("- " + strings.ReplaceAll(text, "\n", " ") + "\n"); err != nil {
			return "", err
		}
		return jsonOut(map[string]string{"saved": "ok"})
	}
	return "", errors.New(`action must be "append", "read", "lanes" or "sources"`)
}

// capNotes returns the newest max characters of notes, and says so when it cut the start off.
func capNotes(s string, max int) (string, error) {
	r := []rune(s)
	if len(r) <= max {
		return jsonOut(map[string]any{"notes": s})
	}
	tail := string(r[len(r)-max:])
	if i := strings.Index(tail, "\n"); i >= 0 && i < len(tail)-1 {
		tail = tail[i+1:] // start at a whole line
	}
	return jsonOut(map[string]any{"notes": tail, "note": fmt.Sprintf("showing the newest %d of %d characters; read one lane by name, or ask for more with max_chars", len([]rune(tail)), len(r))})
}
