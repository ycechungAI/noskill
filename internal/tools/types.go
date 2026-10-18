// Package tools runs the tools skills declare: builtin Go tools and sandboxed script tools
// (FR-S4). The Executor is what the agent loop calls.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"noskill/internal/perms"
	"noskill/internal/sandbox"
	"noskill/internal/skills"
)

// Secrets looks up secret values by name (the OS keychain in production). A tool only ever
// receives the secrets its skill declared.
type Secrets interface {
	Get(name string) (string, bool)
}

// MapSecrets is a Secrets for tests and for values given on the command line.
type MapSecrets map[string]string

func (m MapSecrets) Get(n string) (string, bool) { v, ok := m[n]; return v, ok }

// Env is what one tool call may use. The executor builds it per call from the owning skill's
// permissions, so a builtin can't reach beyond them.
type Env struct {
	RunID  string
	CallID string
	Skill  *skills.Skill
	Perms  perms.Set
	Broker *perms.Broker
	// Secret returns a declared secret (permissions.secrets), or "" when unset or undeclared.
	Secret func(name string) string
	Svc    Services
	// Setting reads a per-skill setting from [skills.<name>] in the config.
	Setting func(key string) (string, bool)
	// Expand resolves path tokens in arguments (~, $DOWNLOADS, volume:<label>/…) to real paths.
	// It does not grant access: the Perms checks still apply to the result.
	Expand func(path string) (string, error)
	// Python runs a program (given as source) in the script sandbox, with no network and the
	// inputs copied into in/ (name → real path, checked against Perms). prepared says whether the
	// code-runner's analysis packages are installed. nil when scripts aren't set up.
	Python func(ctx context.Context, code string, inputs map[string]string, timeout time.Duration) (out sandbox.Output, prepared bool, err error)
}

// Builtin is a tool implemented in Go. The skill's `- builtin: <id>` entry selects it.
type Builtin struct {
	ID          string
	Description string
	Schema      json.RawMessage // JSON Schema for the arguments; nil = no arguments
	// Run returns text for the model: JSON for structured results. Errors go back to the model
	// as the observation, so they must say what to change.
	Run func(ctx context.Context, env *Env, args json.RawMessage) (string, error)
}

// Registry maps builtin ids to implementations.
type Registry map[string]Builtin

func (r Registry) Add(bs ...Builtin) {
	for _, b := range bs {
		if _, dup := r[b.ID]; dup {
			panic("tools: duplicate builtin " + b.ID)
		}
		r[b.ID] = b
	}
}

func (r Registry) IDs() []string {
	out := make([]string, 0, len(r))
	for id := range r {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Services is how builtins reach the rest of noskill (models, routing, delegation) without
// importing it. The application wiring implements it; tests use fakes.
type Services interface {
	ListModels(ctx context.Context) ([]ModelView, error)
	MachineInfo(ctx context.Context) (MachineView, error)
	// Route picks a model for a task type (no side effects).
	Route(ctx context.Context, task, taskType string) (RouteView, error)
	// Delegate runs a self-contained sub-task on a model chosen by task type, or on model if given.
	Delegate(ctx context.Context, task, taskType, model string) (string, error)
	// DelegateAll runs independent sub-tasks, ordered so each model is loaded once (swap rule 7).
	// Results come back in the order given.
	DelegateAll(ctx context.Context, tasks []SubTask) ([]SubResult, error)
	SuggestModels(ctx context.Context, need string) ([]ModelSuggestion, error)
	PullModel(ctx context.Context, name string) (string, error)
	RemoveModel(ctx context.Context, name string) error
	BenchModel(ctx context.Context, name string) (string, error)
	// NotesDir is a folder for the run's scratch notes, inside the run's folder.
	NotesDir(runID string) (string, error)
	// PlanQuestions asks the driver model for sub-questions (structured output).
	PlanQuestions(ctx context.Context, goal string, n int) ([]string, error)
	// Storm researches a topic from five viewpoints, one at a time, and writes the report's
	// pieces (the pipeline runs in Go; the model gets one small job at a time).
	Storm(ctx context.Context, topic string, perViewpoint int) (*StormResult, error)
}

// StormResult is a finished STORM research: the files of the report folder (the caller writes them
// where the user's permissions allow) and how it went.
type StormResult struct {
	Files       []StormFile `json:"files"`
	Summary     string      `json:"summary"`
	Answered    int         `json:"answered"`    // questions answered from fetched pages
	Unsupported int         `json:"unsupported"` // questions no fetched page supported
	Sources     int         `json:"sources"`
	Sections    int         `json:"sections"`
}

// StormFile is one file of the report folder, by relative name.
type StormFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SubTask is one item of a batch delegation.
type SubTask struct {
	Task     string `json:"task"`
	TaskType string `json:"task_type,omitempty"`
	Model    string `json:"model,omitempty"`
}

type SubResult struct {
	Task   int    `json:"task"` // 1-based, as given
	Model  string `json:"model,omitempty"`
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
}

type ModelView struct {
	Name         string   `json:"name"`
	SizeGB       float64  `json:"size_gb"`
	Capabilities []string `json:"capabilities"`
	Loaded       bool     `json:"loaded"`
	Strategy     string   `json:"tool_strategy,omitempty"`
	Params       string   `json:"params,omitempty"`
}

type MachineView struct {
	RAMGB         float64 `json:"ram_gb"`
	BudgetGB      float64 `json:"model_budget_gb"`
	BudgetWhy     string  `json:"budget_basis"`
	ResidentGB    float64 `json:"resident_gb"`
	MemoryWarning string  `json:"memory_pressure,omitempty"`
	FreeDiskGB    float64 `json:"free_disk_gb"`
	OS            string  `json:"os"`
}

type RouteView struct {
	Model       string `json:"model"`
	Reason      string `json:"reason"`
	Swap        bool   `json:"needs_swap"`
	Explanation string `json:"-"`
}

type ModelSuggestion struct {
	Name   string  `json:"name"`
	SizeGB float64 `json:"size_gb"`
	Why    string  `json:"why"`
	Fits   bool    `json:"fits_budget"`
}

// ErrWaiting means a tool needs an approval nobody has given yet (a `confirm` tool in a headless
// run). The run stops in state "waiting" instead of failing (FR-A6).
type ErrWaiting struct{ ApprovalID string }

func (e *ErrWaiting) Error() string {
	return "waiting for approval " + e.ApprovalID
}

// WaitingFor makes ErrWaiting an agent.Waiter: the loop parks the run instead of failing the call.
func (e *ErrWaiting) WaitingFor() string { return e.ApprovalID }

// IsWaiting reports whether err is (or wraps) an ErrWaiting.
func IsWaiting(err error) (*ErrWaiting, bool) {
	var w *ErrWaiting
	return w, errors.As(err, &w)
}

// ErrDeclined: the user said no to a confirm tool.
var ErrDeclined = errors.New("the user declined this action")

// ConfirmRequest asks for approval before a `confirm` tool runs.
type ConfirmRequest struct {
	RunID string
	Skill string
	Tool  string
	Args  json.RawMessage
}

func (c ConfirmRequest) Summary() string {
	return fmt.Sprintf("%s: %s %s", c.Skill, c.Tool, string(c.Args))
}

// Confirm returns nil to proceed, ErrDeclined, or an *ErrWaiting.
type Confirm func(ctx context.Context, req ConfirmRequest) error

// SkillPerms compiles a skill's declared permissions into a checkable Set: filesystem tokens are
// resolved with r, network hosts and the exec/subprocess flags are copied.
func SkillPerms(r perms.Resolver, sk *skills.Skill) (perms.Set, error) {
	p := sk.Meta.Permissions
	files, err := r.Compile(p.Filesystem)
	if err != nil {
		return perms.Set{}, err
	}
	return perms.Set{Files: files, Network: append([]string(nil), p.Network...), Exec: p.Exec, Subprocess: p.Subprocess}, nil
}
