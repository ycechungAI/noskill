package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		{ID: "notes", Description: "Keep scratch notes for this task. append adds a line (fact + source URL); read returns everything saved so far.",
			Schema: obj(`{"type":"object","required":["action"],"properties":{"action":{"type":"string","enum":["append","read"]},"text":{"type":"string","maxLength":2000}}}`),
			Run:    notes},
	}
}

const maxNotes = 64 << 10

func notes(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	if err := needSvc(env); err != nil {
		return "", err
	}
	var a struct{ Action, Text string }
	json.Unmarshal(raw, &a)
	dir, err := env.Svc.NotesDir(env.RunID)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "notes.md")
	switch a.Action {
	case "read":
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return jsonOut(map[string]any{"notes": "", "note": "nothing saved yet"})
		}
		if err != nil {
			return "", err
		}
		return jsonOut(map[string]any{"notes": string(b)})
	case "append":
		text := strings.TrimSpace(a.Text)
		if text == "" {
			return "", errors.New(`append needs "text"`)
		}
		if fi, err := os.Stat(p); err == nil && fi.Size()+int64(len(text)) > maxNotes {
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
	return "", errors.New(`action must be "append" or "read"`)
}
