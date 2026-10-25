// Package eval is the checked-task harness behind `noskill eval` and the per-task-type scores in
// `noskill models bench` (FR-B4, LEARNING.md "Evaluation"). Every task has a deterministic check, so
// no model judges another model's answer.
package eval

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Task is one checked question. Plain tasks are a single model call without tools; agent tasks run
// the whole loop with the named skills against a temporary folder ({dir} in the prompt).
type Task struct {
	ID     string `yaml:"id" json:"id"`
	Type   string `yaml:"type" json:"type"`                         // the router's task types: chat, math, code, …; "files" and "code-runner" for agent tasks
	Bench  bool   `yaml:"bench,omitempty" json:"bench,omitempty"`   // part of the quick micro-eval in `models bench`
	Family string `yaml:"family,omitempty" json:"family,omitempty"` // multi-step tasks: the chore it is one instance of
	Prompt string `yaml:"prompt" json:"prompt"`
	Check  Check  `yaml:"check" json:"check"`

	// Agent tasks.
	Skills   []string          `yaml:"skills,omitempty" json:"skills,omitempty"`
	Files    map[string]string `yaml:"files,omitempty" json:"files,omitempty"`                 // created in {dir} before the run
	Aged     map[string]int    `yaml:"aged_days,omitempty" json:"aged_days,omitempty"`         // files made this many days old (their modified time), for tasks where age matters
	Docs     map[string]string `yaml:"docs,omitempty" json:"docs,omitempty"`                   // a folder of documents, added and indexed for this run alone (local-docs tasks)
	Tools    []string          `yaml:"tools,omitempty" json:"tools,omitempty"`                 // at least one must have been called (see ToolsMet)
	Writes   bool              `yaml:"writes,omitempty" json:"writes,omitempty"`               // the run may write in {dir}
	NeedsBox bool              `yaml:"needs_sandbox,omitempty" json:"needs_sandbox,omitempty"` // skipped without an OS script sandbox
	End      End               `yaml:"end,omitempty" json:"end,omitempty"`
	Source   string            `yaml:"-" json:"source,omitempty"` // builtin | pinned
}

// toolEffects is the change each built-in file tool makes, as the undo log names it.
var toolEffects = map[string]string{"move": "move", "move_many": "move", "copy": "copy", "copy_verify": "copy",
	"write_text": "create", "make_dir": "mkdir", "trash": "trash"}

// ToolsMet says whether a run did the work Tools asks for: it called one of them, or a learned
// script tool made the same kind of change through its ops (ADR-0013), which the broker applies
// with the same checks and undo log. End still checks the folder either way.
// learned are the called tools that learned skills own.
func (t Task) ToolsMet(called, learned, effects []string) bool {
	for _, tool := range t.Tools {
		if slices.Contains(called, tool) {
			return true
		}
		if op, ok := toolEffects[tool]; ok && slices.Contains(effects, op) && len(learned) > 0 {
			return true
		}
	}
	return len(t.Tools) == 0
}

// Agent says whether the task needs the full loop.
func (t Task) Agent() bool { return len(t.Skills) > 0 }

// Check is what the answer must satisfy. Every field that is set must hold.
type Check struct {
	Contains []string       `yaml:"contains,omitempty" json:"contains,omitempty"`   // all, case-insensitive
	Any      []string       `yaml:"any,omitempty" json:"any,omitempty"`             // at least one
	Not      []string       `yaml:"not,omitempty" json:"not,omitempty"`             // none
	Regex    []string       `yaml:"regex,omitempty" json:"regex,omitempty"`         // all match
	NotRegex []string       `yaml:"not_regex,omitempty" json:"not_regex,omitempty"` // none match
	Number   *float64       `yaml:"number,omitempty" json:"number,omitempty"`       // the last number in the answer
	JSON     map[string]any `yaml:"json,omitempty" json:"json,omitempty"`           // an object in the answer with these fields
	MaxWords int            `yaml:"max_words,omitempty" json:"max_words,omitempty"`
	Lines    int            `yaml:"lines,omitempty" json:"lines,omitempty"`       // exactly this many non-empty lines
	Answered bool           `yaml:"answered,omitempty" json:"answered,omitempty"` // any non-empty answer
}

// End is the state a run must leave in {dir}. Paths are relative to it.
type End struct {
	Exists    []string          `yaml:"exists,omitempty" json:"exists,omitempty"`
	Absent    []string          `yaml:"absent,omitempty" json:"absent,omitempty"`
	FileHas   map[string]string `yaml:"file_has,omitempty" json:"file_has,omitempty"`     // the file contains the text
	FileLacks map[string]string `yaml:"file_lacks,omitempty" json:"file_lacks,omitempty"` // the file doesn't contain the text
}

//go:embed tasks/*.yaml
var builtinFS embed.FS

//go:embed workload/*.yaml
var workloadFS embed.FS

// Workload returns the training requests `noskill eval simulate` replays as a user's days. They
// share families with the held-out "multi" tasks but never their names or contents.
func Workload() ([]Task, error) {
	b, err := workloadFS.ReadFile("workload/workload.yaml")
	if err != nil {
		return nil, err
	}
	var raw []Task
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for i := range raw {
		if raw[i].Type == "" {
			raw[i].Type = "multi"
		}
	}
	b, err = yaml.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return parse(b, "workload")
}

// Builtin returns the tasks that ship with noskill, ordered by ID.
func Builtin() ([]Task, error) {
	var out []Task
	err := fs.WalkDir(builtinFS, "tasks", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		b, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		ts, err := parse(b, "builtin")
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, ts...)
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

// Pinned reads the user's own tasks from <dir>/pinned.yaml (none is fine).
func Pinned(dir string) ([]Task, error) {
	b, err := os.ReadFile(filepath.Join(dir, "pinned.yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse(b, "pinned")
}

// SavePinned appends a task to <dir>/pinned.yaml.
func SavePinned(dir string, t Task) error {
	old, err := Pinned(dir)
	if err != nil {
		return err
	}
	for _, o := range old {
		if o.ID == t.ID {
			return fmt.Errorf("a pinned task named %s already exists", t.ID)
		}
	}
	b, err := yaml.Marshal(append(old, t))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "pinned.yaml"), b, 0o644)
}

func parse(b []byte, source string) ([]Task, error) {
	var ts []Task
	if err := yaml.Unmarshal(b, &ts); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		t.Source = source
		switch {
		case t.ID == "" || t.Type == "" || strings.TrimSpace(t.Prompt) == "":
			return nil, fmt.Errorf("task %d needs id, type and prompt", i+1)
		case seen[t.ID]:
			return nil, fmt.Errorf("task id %s is used twice", t.ID)
		}
		if _, err := t.Check.compile(); err != nil {
			return nil, fmt.Errorf("task %s: %w", t.ID, err)
		}
		seen[t.ID] = true
	}
	return ts, nil
}

// Select filters tasks: quick keeps only the micro-eval; ids and types, when given, each add the
// tasks they name (--task a --type math is a's task plus all math tasks). Empty filters keep everything.
func Select(all []Task, ids, types []string, quick bool) []Task {
	var out []Task
	for _, t := range all {
		if quick && !t.Bench {
			continue
		}
		if (len(ids) > 0 || len(types) > 0) && !contains(ids, t.ID) && !contains(types, t.Type) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
