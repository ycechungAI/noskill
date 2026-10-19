package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"

	"noskill/internal/backends"
	"noskill/internal/docs"
	"noskill/internal/orchestrator"
	"noskill/internal/tools"
	"noskill/internal/volumes"
)

//go:embed catalog.json
var catalogJSON []byte

type catalogEntry struct {
	Name  string   `json:"name"`
	Tasks []string `json:"tasks"`
	Needs []string `json:"needs"`
	MemGB float64  `json:"mem_gb"`
	Note  string   `json:"note"`
}

func catalog() []catalogEntry {
	var c []catalogEntry
	json.Unmarshal(catalogJSON, &c)
	return c
}

// maxDelegations per run (orchestrator skill: "don't delegate more than 3 times").
const maxDelegations = 3

// runServices implements tools.Services for one run.
type runServices struct {
	a           *App
	runID       string
	model       Model
	docs        *docs.Index // an eval task's own documents; nil = the user's index
	delegations atomic.Int32
	delegated   atomic.Pointer[string] // the model the last delegation ran on (swap rule 8)
}

// takeDelegated returns, once, the model a delegation last ran on when it isn't the driver.
func (s *runServices) takeDelegated() string {
	if p := s.delegated.Swap(nil); p != nil && *p != s.model.Name {
		return *p
	}
	return ""
}

func (s *runServices) ListModels(ctx context.Context) ([]tools.ModelView, error) {
	ms, errs := s.a.Models(ctx)
	if len(ms) == 0 && len(errs) > 0 {
		return nil, &NoBackendError{Errs: errs}
	}
	out := make([]tools.ModelView, 0, len(ms))
	for _, m := range ms {
		v := tools.ModelView{Name: m.Name, SizeGB: round1(float64(m.SizeBytes) / 1e9), Loaded: m.Loaded, Strategy: string(m.Strategy)}
		if m.ParamsB > 0 {
			v.Params = fmt.Sprintf("%.1fB", m.ParamsB)
		}
		for _, c := range m.Capabilities {
			v.Capabilities = append(v.Capabilities, string(c))
		}
		out = append(out, v)
	}
	return out, nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

func (s *runServices) MachineInfo(ctx context.Context) (tools.MachineView, error) {
	budget, why, mem := s.a.memBudget()
	ms, _ := s.a.Models(ctx)
	var resident int64
	for _, m := range ms {
		if m.Loaded {
			resident += m.LoadedSz
		}
	}
	v := tools.MachineView{RAMGB: round1(float64(mem.TotalBytes) / (1 << 30)), BudgetGB: round1(float64(budget) / 1e9), BudgetWhy: why,
		ResidentGB: round1(float64(resident) / 1e9), FreeDiskGB: round1(float64(s.a.diskFree()) / 1e9), OS: runtime.GOOS}
	if mem.Pressure != "" && mem.Pressure != "normal" {
		v.MemoryWarning = mem.Pressure
	}
	return v, nil
}

func (s *runServices) Route(ctx context.Context, task, taskType string) (tools.RouteView, error) {
	p, err := s.a.Plan(ctx, RunRequest{Messages: []backends.Message{{Role: backends.RoleUser, Content: task}}, NoSkills: true, TaskType: taskType})
	if err != nil {
		return tools.RouteView{}, err
	}
	return tools.RouteView{Model: p.pick.Name, Reason: p.decision.Explanation.Why, Swap: p.decision.Swap, Explanation: p.decision.Explanation.Text()}, nil
}

func (s *runServices) Delegate(ctx context.Context, task, taskType, model string) (string, error) {
	if n := s.delegations.Add(1); n > maxDelegations {
		return "", fmt.Errorf("delegation limit reached (%d per task): finish with what you have", maxDelegations)
	}
	res, err := s.a.Run(ctx, RunRequest{
		Messages: []backends.Message{{Role: backends.RoleUser, Content: task}},
		Source:   "delegate", ParentID: s.runID, Model: model, TaskType: taskType, NoSkills: true,
	})
	if err != nil {
		return "", fmt.Errorf("the delegated task failed: %w", err)
	}
	s.delegated.Store(&res.Model)
	if res.Answer == "" {
		return "", fmt.Errorf("the delegated task ended without an answer (%s)", res.Stop)
	}
	return res.Answer, nil
}

// maxBatch is the most sub-tasks one delegate call may carry.
const maxBatch = 6

// DelegateAll runs a batch of independent sub-tasks (swap rule 7): sub-tasks for the loaded model
// first, then the rest grouped by the model they route to, so each model is loaded once. The batch
// counts as one delegation.
func (s *runServices) DelegateAll(ctx context.Context, tasks []tools.SubTask) ([]tools.SubResult, error) {
	if len(tasks) > maxBatch {
		return nil, fmt.Errorf("at most %d sub-tasks per call", maxBatch)
	}
	if n := s.delegations.Add(1); n > maxDelegations {
		return nil, fmt.Errorf("delegation limit reached (%d per task): finish with what you have", maxDelegations)
	}
	routed := make([]string, len(tasks))
	stays := make([]bool, len(tasks))
	for i, t := range tasks {
		routed[i] = t.Model
		if t.Model != "" {
			continue
		}
		if p, err := s.a.Plan(ctx, RunRequest{Messages: []backends.Message{{Role: backends.RoleUser, Content: t.Task}}, NoSkills: true, TaskType: t.TaskType}); err == nil {
			routed[i], stays[i] = p.pick.Name, !p.decision.Swap
		}
	}
	out := make([]tools.SubResult, len(tasks))
	for _, i := range orchestrator.BatchOrder(routed, stays) {
		t := tasks[i]
		out[i].Task = i + 1
		res, err := s.a.Run(ctx, RunRequest{
			Messages: []backends.Message{{Role: backends.RoleUser, Content: t.Task}},
			Source:   "delegate", ParentID: s.runID, Model: t.Model, TaskType: t.TaskType, NoSkills: true,
		})
		switch {
		case err != nil:
			out[i].Error = err.Error()
		case res.Answer == "":
			out[i].Model, out[i].Error = res.Model, "ended without an answer ("+string(res.Stop)+")"
		default:
			out[i].Model, out[i].Answer = res.Model, res.Answer
		}
		if res != nil {
			s.delegated.Store(&res.Model)
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	return out, nil
}

func (s *runServices) SuggestModels(ctx context.Context, need string) ([]tools.ModelSuggestion, error) {
	ms, _ := s.a.Models(ctx)
	have := map[string]bool{}
	var resident int64
	for _, m := range ms {
		have[m.Name] = true
		if m.Loaded && !m.chat() {
			resident += m.LoadedSz
		}
	}
	budget, _, _ := s.a.memBudget()
	var out []tools.ModelSuggestion
	for _, c := range catalog() {
		if have[c.Name] || !(contains(c.Tasks, need) || contains(c.Needs, need)) {
			continue
		}
		fits := budget == 0 || int64(c.MemGB*1e9)+resident <= budget
		why := c.Note
		if !fits {
			why += " Does not fit the current memory budget."
		}
		out = append(out, tools.ModelSuggestion{Name: c.Name, SizeGB: c.MemGB, Why: why, Fits: fits})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fits != out[j].Fits {
			return out[i].Fits
		}
		return out[i].SizeGB > out[j].SizeGB
	})
	return out, nil
}

func (s *runServices) PullModel(ctx context.Context, name string) (string, error) {
	last := ""
	if err := s.a.Pull(ctx, name, func(st string) { last = st }); err != nil {
		return "", err
	}
	return last, nil
}

func (s *runServices) RemoveModel(ctx context.Context, name string) error {
	return s.a.RemoveModel(ctx, name)
}

func (s *runServices) BenchModel(ctx context.Context, name string) (string, error) {
	p, err := s.a.Bench(ctx, name, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%.1f tokens/s, first token in %.0f ms, %.1f GB in memory at %d context, cold load %.1f s",
		p.TokPerSec, p.TTFTMs, float64(p.MemBytes)/1e9, p.Ctx, p.LoadMsCold/1000), nil
}

func (s *runServices) NotesDir(runID string) (string, error) {
	d := filepath.Join(s.a.Dirs.Runs(), runID)
	return d, os.MkdirAll(d, 0o700)
}

func (s *runServices) PlanQuestions(ctx context.Context, goal string, n int) ([]string, error) {
	schema := json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"questions":{"type":"array","minItems":2,"maxItems":%d,"items":{"type":"string"}}},"required":["questions"]}`, n))
	no := false
	temp := 0.2
	st, err := s.model.NB.B.Chat(ctx, backends.ChatRequest{Model: s.model.Name, Think: &no, Format: schema,
		Messages: []backends.Message{
			{Role: backends.RoleSystem, Content: fmt.Sprintf("Break the research goal into %d short, specific questions that can each be answered with a web search. Reply with JSON.", n)},
			{Role: backends.RoleUser, Content: goal}},
		Options: backends.Options{NumCtx: s.a.numCtx(s.model), NumPredict: 400, Temperature: &temp}})
	if err != nil {
		return nil, err
	}
	r, err := backends.Collect(st, nil)
	if err != nil {
		return nil, err
	}
	var out struct{ Questions []string }
	if err := json.Unmarshal([]byte(r.Content), &out); err != nil || len(out.Questions) == 0 {
		return nil, errors.New("the model didn't return a list of questions; write them yourself and continue")
	}
	if len(out.Questions) > n {
		out.Questions = out.Questions[:n]
	}
	return out.Questions, nil
}

// ---- volumes ----

// Volumes resolves volume:<label> and binds each label to its drive's UUID on first use, so another
// drive with the same label is refused (PLATFORMS.md "External drives").
func (a *App) Volumes() *volumes.Binder { return &volumes.Binder{DB: a.DB} }
