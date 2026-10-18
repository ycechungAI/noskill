package app

import (
	"context"
	"fmt"
	"time"

	"noskill/internal/backends"
	"noskill/internal/decide"
	"noskill/internal/probe"
	"noskill/internal/store"
)

// Probe runs the tool-strategy probe on a model and stores the verdict (ADR-0005).
func (a *App) Probe(ctx context.Context, name string) (probe.Result, error) {
	m, err := a.FindModel(ctx, name)
	if err != nil {
		return probe.Result{}, err
	}
	if !m.chat() {
		return probe.Result{}, fmt.Errorf("%s is not a chat model", m.Name)
	}
	res, err := probe.Run(ctx, m.NB.B, m.ModelInfo, a.numCtx(m))
	if err != nil {
		return res, err
	}
	if err := a.DB.SetToolStrategy(ctx, m.ID, res.Strategy); err != nil {
		return res, err
	}
	a.invalidateModels()
	return res, nil
}

// Bench measures a model's load time, first-token latency, speed and memory at the context size
// noskill uses, and stores it for the router (FR-B4). The model is unloaded first (to time a cold
// load) and again afterwards, so this changes what's loaded: it's an explicit user action.
func (a *App) Bench(ctx context.Context, name string, progress func(string)) (store.Perf, error) {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	m, err := a.FindModel(ctx, name)
	if err != nil {
		return store.Perf{}, err
	}
	if !m.chat() {
		return store.Perf{}, fmt.Errorf("%s is not a chat model", m.Name)
	}
	numCtx := a.numCtx(m)
	say("unloading " + m.Name + " to time a cold load")
	if err := m.NB.B.Load(ctx, m.Name, 0); err != nil {
		return store.Perf{}, err
	}
	say("loading")
	t0 := time.Now()
	var lerr error
	if cl, ok := m.NB.B.(backends.CtxLoader); ok {
		lerr = cl.LoadCtx(ctx, m.Name, numCtx, 5*time.Minute)
	} else {
		lerr = m.NB.B.Load(ctx, m.Name, 5*time.Minute)
	}
	if lerr != nil {
		return store.Perf{}, lerr
	}
	loadMs := float64(time.Since(t0).Milliseconds())
	say("generating")
	no, temp := false, 0.0
	zero := time.Duration(0)
	t1 := time.Now()
	st, err := m.NB.B.Chat(ctx, backends.ChatRequest{Model: m.Name, Think: &no, KeepAlive: &zero,
		Messages: []backends.Message{{Role: backends.RoleUser, Content: "Write the numbers from 1 to 60 separated by spaces. Output only the numbers."}},
		Options:  backends.Options{NumCtx: numCtx, NumPredict: 200, Temperature: &temp}})
	if err != nil {
		return store.Perf{}, err
	}
	var first time.Time
	var mem int64
	r, err := backends.Collect(st, func(c backends.ChatChunk) {
		if first.IsZero() && (c.Content != "" || c.Thinking != "") {
			first = time.Now()
			// Ollama reports the loaded size while the model is generating.
			if l, err := m.NB.B.Loaded(ctx); err == nil {
				for _, x := range l {
					if x.Name == m.Name {
						mem = x.SizeBytes
					}
				}
			}
		}
	})
	if err != nil {
		return store.Perf{}, err
	}
	end := time.Now()
	if first.IsZero() || r.Usage.CompletionTokens < 5 {
		return store.Perf{}, fmt.Errorf("%s produced too little output to measure", m.Name)
	}
	gen := end.Sub(first).Seconds()
	p := store.Perf{TTFTMs: float64(first.Sub(t1).Milliseconds()), LoadMsCold: loadMs, MemBytes: mem, Ctx: numCtx, MeasuredAt: time.Now()}
	if gen > 0 {
		p.TokPerSec = float64(r.Usage.CompletionTokens-1) / gen
	}
	if err := a.DB.PutPerf(ctx, m.ID, p); err != nil {
		return p, err
	}
	a.invalidateModels()
	return p, nil
}

// CheapestChat returns the smallest installed chat model: background requests (titles, tags) run on it.
func (a *App) CheapestChat(ctx context.Context) (string, error) {
	ms, errs := a.Models(ctx)
	best := -1
	for i, m := range ms {
		if m.chat() && (best < 0 || m.SizeBytes < ms[best].SizeBytes || m.Loaded && !ms[best].Loaded) {
			best = i
		}
	}
	if best < 0 {
		if len(errs) > 0 {
			return "", &NoBackendError{Errs: errs}
		}
		return "", fmt.Errorf("no chat model is installed")
	}
	return ms[best].Name, nil
}

// Manager returns the first backend that can download and remove models.
func (a *App) Manager() (backends.Manager, error) {
	for _, nb := range a.Backends {
		if m, ok := nb.B.(backends.Manager); ok {
			return m, nil
		}
	}
	return nil, fmt.Errorf("none of the configured backends can download or remove models")
}

// Pull downloads a model. Callers ask the user first: this is the only place noskill fetches model weights.
func (a *App) Pull(ctx context.Context, name string, progress func(string)) error {
	m, err := a.Manager()
	if err != nil {
		return err
	}
	if err := m.Pull(ctx, name, progress); err != nil {
		return err
	}
	a.invalidateModels()
	return nil
}

// RemoveModel deletes an installed model from its backend. The decision and embedding models and
// any model a routing rule names are protected: the router and skill selector depend on them.
func (a *App) RemoveModel(ctx context.Context, name string) error {
	o := a.Cfg.Orchestrator
	if name == o.DecisionModel || name == o.EmbedModel {
		return fmt.Errorf("%s is the decision or embedding model noskill relies on; change orchestrator.decision_model or embed_model first", name)
	}
	for _, r := range o.Rules {
		if r.Use == name {
			return fmt.Errorf("%s is used by a routing rule (use = %q)", name, name)
		}
	}
	m, err := a.Manager()
	if err != nil {
		return err
	}
	if err := m.Delete(ctx, name); err != nil {
		return err
	}
	a.invalidateModels()
	return nil
}

// Decide makes a raw decision call: the named decision model if installed, else the emulated path.
func (a *App) Decide(ctx context.Context, model string, state map[string]any, qs map[string]decide.Question) (decide.Answers, error) {
	ms, errs := a.Models(ctx)
	if len(ms) == 0 && len(errs) > 0 {
		return decide.Answers{}, &NoBackendError{Errs: errs}
	}
	if model != "" {
		m, err := a.FindModel(ctx, model)
		if err != nil {
			return decide.Answers{}, err
		}
		if m.Has(backends.CapDecide) && m.NB.Kind == "ollama" {
			return decide.JEV{BaseURL: m.NB.URL, Model: m.Name}.Decide(ctx, state, qs)
		}
		if m.Has(backends.CapChat) {
			return decide.Emulated{Backend: m.NB.B, Model: m.Name, NumCtx: a.numCtx(m)}.Decide(ctx, state, qs)
		}
		return decide.Answers{}, &UserError{fmt.Sprintf("%s can't make decisions", m.Name)}
	}
	d := a.decider(ctx, ms)
	if d == nil {
		return decide.Answers{}, &UserError{"no model is available to decide with"}
	}
	return d.Decide(ctx, state, qs)
}
