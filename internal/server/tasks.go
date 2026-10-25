package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"noskill/internal/app"
	"noskill/internal/backends"
	"noskill/internal/scheduler"
)

// Model actions from the status page: bench, speed only, tool check and the checked files tasks
// (FR-I6, FR-B4). One runs at a time, since a bench unloads the model it times and every one
// loads a model into the shared memory budget (ADR-0012). Admin only, like the other calls that
// change things.

const (
	taskBench  = "bench"  // speed, memory and the quality micro-eval: `noskill models bench`
	taskSpeed  = "speed"  // `models bench --perf-only`
	taskProbe  = "probe"  // `models probe`
	taskChecks = "checks" // `eval run --type files`: the checked agent tasks
	taskPull   = "pull"   // `models pull`: download a model the user picked
)

type uiTask struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Model   string    `json:"model"`
	State   string    `json:"state"` // running | done | failed | stopped
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
	Lines   []string  `json:"lines"`
	Result  string    `json:"result,omitempty"`
	Error   string    `json:"error,omitempty"`

	cancel context.CancelFunc
}

type taskBook struct {
	mu     sync.Mutex
	n      int
	recent []*uiTask // newest first, at most keepTasks
}

const keepTasks = 8

var validModelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,100}(:[A-Za-z0-9._-]{1,50})?$`)

func (b *taskBook) running() *uiTask {
	for _, t := range b.recent {
		if t.State == "running" {
			return t
		}
	}
	return nil
}

func (b *taskBook) snapshot() []uiTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]uiTask, 0, len(b.recent))
	for _, t := range b.recent {
		c := *t
		c.Lines = slices.Clone(t.Lines)
		out = append(out, c)
	}
	return out
}

// say records a progress line; the page shows the last few.
func (b *taskBook) say(t *uiTask, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := len(t.Lines); n > 0 && t.Lines[n-1] == line {
		return
	}
	t.Lines = append(t.Lines, line)
	if len(t.Lines) > 200 {
		t.Lines = t.Lines[len(t.Lines)-200:]
	}
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.tasks.snapshot()})
}

// startTask: {kind, model}. 202 with the new task, 409 while another is running.
func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Kind  string `json:"kind"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Model) == "" {
		s.fail(w, r, bad(`send {"kind": "bench" | "speed" | "probe" | "checks", "model": a model name}`))
		return
	}
	if !slices.Contains([]string{taskBench, taskSpeed, taskProbe, taskChecks, taskPull}, in.Kind) {
		s.fail(w, r, bad("unknown kind %q (bench, speed, probe, checks, pull)", in.Kind))
		return
	}
	ctx := r.Context()
	if in.Kind == taskPull {
		// The user picked the model and knows the disk it needs; only the name is checked.
		name := strings.TrimSpace(in.Model)
		if !validModelName.MatchString(name) {
			s.fail(w, r, bad("%q isn't a model name (like granite4:3b)", name))
			return
		}
		if slices.Contains(s.App.Cfg.Orchestrator.Exclude, name) {
			apiError(w, r, http.StatusConflict, name+" is excluded in the config (orchestrator.exclude)")
			return
		}
		s.startRun(w, r, in.Kind, name)
		return
	}
	m, err := s.App.FindModel(ctx, in.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	chat := m.Has(backends.CapChat)
	switch {
	case !chat && !m.CanBenchOther():
		s.fail(w, r, bad("%s is not a chat, decision or embedding model; there is no benchmark for it yet", m.Name))
		return
	case !chat && in.Kind != taskBench && in.Kind != taskSpeed:
		s.fail(w, r, bad("%s isn't a chat model: only Bench applies to it", m.Name))
		return
	}
	if slices.Contains(s.App.Cfg.Orchestrator.Exclude, m.Name) {
		apiError(w, r, http.StatusConflict, m.Name+" is excluded in the config (orchestrator.exclude), so noskill doesn't load or time it")
		return
	}
	s.startRun(w, r, in.Kind, m.Name)
}

// startRun registers one task and runs it in the background: 202 with the task, or 409 while a
// scheduled job or another task is running.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request, kind, model string) {
	ctx := r.Context()
	if active, err := (&scheduler.Store{DB: s.App.DB}).ActiveRuns(ctx); err == nil && len(active) > 0 {
		apiError(w, r, http.StatusConflict, "a scheduled job is running; try again when it has finished")
		return
	}

	s.tasks.mu.Lock()
	if cur := s.tasks.running(); cur != nil {
		s.tasks.mu.Unlock()
		apiError(w, r, http.StatusConflict, fmt.Sprintf("%s of %s is still running; wait for it or stop it", cur.Kind, cur.Model))
		return
	}
	s.tasks.n++
	tctx, cancel := context.WithCancel(context.Background())
	t := &uiTask{ID: fmt.Sprintf("t%d", s.tasks.n), Kind: kind, Model: model, State: "running", Started: time.Now(), Lines: []string{}, cancel: cancel}
	s.tasks.recent = append([]*uiTask{t}, s.tasks.recent...)
	if len(s.tasks.recent) > keepTasks {
		s.tasks.recent = s.tasks.recent[:keepTasks]
	}
	c := *t
	s.tasks.mu.Unlock()

	go s.runTask(tctx, t)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "task": c})
}

func (s *Server) stopTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.tasks.mu.Lock()
	defer s.tasks.mu.Unlock()
	for _, t := range s.tasks.recent {
		if t.ID == id && t.State == "running" {
			t.cancel()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
	}
	apiError(w, r, http.StatusNotFound, "no running task "+id)
}

func (s *Server) runTask(ctx context.Context, t *uiTask) {
	defer t.cancel()
	say := func(l string) { s.tasks.say(t, l) }
	result, err := s.doTask(ctx, t, say)
	s.tasks.mu.Lock()
	defer s.tasks.mu.Unlock()
	t.Ended = time.Now()
	switch {
	case ctx.Err() != nil:
		t.State, t.Error = "stopped", "stopped"
	case err != nil:
		t.State, t.Error = "failed", err.Error()
	default:
		t.State, t.Result = "done", result
	}
}

func (s *Server) doTask(ctx context.Context, t *uiTask, say func(string)) (string, error) {
	a := s.App
	if t.Kind == taskPull {
		say("downloading " + t.Model)
		if err := a.Pull(ctx, t.Model, say); err != nil {
			return "", err
		}
		return t.Model + " is installed", nil
	}
	if m, err := a.FindModel(ctx, t.Model); err == nil && !m.Has(backends.CapChat) {
		say("benchmark for a " + strings.Join(capNames(m), ", ") + " model")
		out, err := a.BenchOther(ctx, t.Model, say)
		if err != nil {
			return "", err
		}
		return out.Summary, nil
	}
	switch t.Kind {
	case taskBench, taskSpeed:
		p, err := a.Bench(ctx, t.Model, func(l string) { say(l + "…") })
		if err != nil {
			return "", err
		}
		out := fmt.Sprintf("%.1f tokens/s, first token in %.0f ms, %.1f GB in memory at %d context, cold load %.1f s",
			p.TokPerSec, p.TTFTMs, float64(p.MemBytes)/1e9, p.Ctx, p.LoadMsCold/1000)
		if t.Kind == taskBench {
			scores, err := a.BenchScores(ctx, t.Model, func(l string) { say("quality " + l) })
			if err != nil {
				return "", err
			}
			out += "\nquality: " + scoreLine(scores)
		}
		return out, nil
	case taskProbe:
		say("probing tool calls…")
		res, err := a.Probe(ctx, t.Model)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("tool strategy: %s\n%s", res.Strategy, res.Reason), nil
	case taskChecks:
		ts, err := a.EvalTasks(nil, []string{"files"}, false)
		if err != nil {
			return "", err
		}
		out, err := a.Eval(ctx, app.EvalOptions{Model: t.Model, Tasks: ts, Suite: "files", Note: "status page", Progress: say})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d of %d checked files tasks passed", out.Run.Passed, out.Run.Tasks), nil
	}
	return "", fmt.Errorf("unknown kind %q", t.Kind)
}

func capNames(m app.Model) []string {
	var out []string
	for _, c := range m.Capabilities {
		out = append(out, string(c))
	}
	return out
}

func scoreLine(scores map[string]float64) string {
	var ks []string
	for k := range scores {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s %.2f", k, scores[k]))
	}
	return strings.Join(parts, " · ")
}
