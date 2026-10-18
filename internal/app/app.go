// Package app is noskill's engine: it owns the database, backends, skills, tools and approvals,
// and runs a request end to end (classify → route → select skills → agent loop → record). The CLI,
// the proxy and the daemon are thin layers over it.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/backends"
	_ "noskill/internal/backends/mlx" // register the adapters
	_ "noskill/internal/backends/ollama"
	_ "noskill/internal/backends/openai"
	_ "noskill/internal/backends/vllm"
	"noskill/internal/config"
	"noskill/internal/decide"
	"noskill/internal/docs"
	"noskill/internal/memory"
	"noskill/internal/notify"
	"noskill/internal/orchestrator"
	"noskill/internal/perms"
	"noskill/internal/platform"
	"noskill/internal/sandbox"
	"noskill/internal/secrets"
	"noskill/internal/skills"
	"noskill/internal/store"
	"noskill/internal/tools"
	"noskill/internal/tools/builtin"
	skillfiles "noskill/skills"
)

// NamedBackend is a configured backend.
type NamedBackend struct {
	ID, Kind, URL string
	Key           string // API key for the backend, used when the proxy passes requests through
	B             backends.Backend
}

type Options struct {
	Dirs    platform.Dirs
	Loaded  *config.Loaded
	Secrets *secrets.Manager
	Project string // folder whose .noskill/skills holds project skills; "" = none

	// Overrides for tests; zero values use the real thing.
	Backends []NamedBackend
	Trash    platform.Trasher
	Sandbox  platform.Sandbox
	Notifier platform.Notifier
	Stderr   io.Writer
}

type App struct {
	Opts    Options
	Cfg     config.Config
	Dirs    platform.Dirs
	DB      *store.DB
	Secrets *secrets.Manager

	Backends    []NamedBackend
	Skills      *skills.Set
	SkillIssues []skills.Issue
	Registry    tools.Registry
	Approvals   *approvals.Queue
	Hub         *notify.Hub
	Memory      *memory.Store
	Interp      *tools.Interpreters
	Runner      *sandbox.Runner
	Trash       platform.Trasher

	slots       chan struct{}
	interactive atomic.Int32 // interactive requests waiting for a slot

	bg            sync.WaitGroup                                                              // work after the reply (the decision judge)
	judgeOverride decide.Decider                                                              // tests
	judgeTried    map[string]bool                                                             // runs the LLM judge couldn't judge (this process)
	tuneClock     tuneClock                                                                   // when each decision threshold was last considered (calibrate.go)
	docs          *docs.Index                                                                 // docs.db, opened on first use (docs.go)
	learnCancel   context.CancelFunc                                                          // cancels background learning work (pre-emption); guarded by mu
	powerOverride func() (on, known bool)                                                     // tests
	trainCmd      func(ctx context.Context, out io.Writer, name string, args ...string) error // tests: in place of running uv and ollama (train.go)
	trainMem      func() (platform.Memory, error)                                             // tests
	tunedMu       sync.Mutex
	tunedIn       map[string]int // built-in skill → tuned version in use (ADR-0014); guarded by tunedMu

	mu     sync.Mutex
	egress *sandbox.Egress
	models struct {
		at   time.Time
		list []Model
		errs []error
	}
}

// Open builds the engine: folders, database (migrated), backends, skills, approvals.
func Open(ctx context.Context, o Options) (*App, error) {
	if o.Loaded == nil {
		return nil, errors.New("app: no config")
	}
	if o.Trash == nil {
		o.Trash = platform.OSTrash{}
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	a := &App{Opts: o, Cfg: o.Loaded.Config, Dirs: o.Dirs, Secrets: o.Secrets, Trash: o.Trash, Registry: builtin.Registry(), judgeTried: map[string]bool{}}
	if err := a.Dirs.Ensure(); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, a.Dirs.StateDB(), o.Trash)
	if err != nil {
		return nil, err
	}
	a.DB = db
	if a.Secrets == nil {
		a.Secrets = secrets.NewManager(secrets.OSKeychain(), filepath.Join(a.Dirs.Data, "secrets.names"))
	}

	a.Backends = o.Backends
	if a.Backends == nil {
		if a.Backends, err = a.buildBackends(); err != nil {
			db.Close()
			return nil, err
		}
	}

	if err := a.loadSkills(ctx); err != nil {
		db.Close()
		return nil, err
	}

	a.Hub = &notify.Hub{}
	not := o.Notifier
	if not == nil {
		not = platform.NewNotifier()
	}
	chans, errs := notify.FromConfig(a.Cfg.Notify, func(n string) (string, error) {
		if v, ok := a.Secrets.Get(n); ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %s isn't set", n)
	}, not)
	for _, e := range errs {
		fmt.Fprintln(o.Stderr, "noskill: notifications:", e)
	}
	a.Hub.Channels = chans

	a.Approvals = &approvals.Queue{DB: db, Default: a.Cfg.Approvals.DefaultTimeout.Duration, Validate: a.validateField}
	a.Approvals.Announce = func(ctx context.Context, it approvals.Item) {
		if !contains(a.Cfg.Approvals.Channels, "notification") {
			return
		}
		go func() {
			bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			a.Hub.Send(bg, notify.Message{ID: it.ID, Title: "noskill needs your OK", Urgent: true,
				Body: it.Summary + "\nnoskill approvals approve " + it.ID})
		}()
	}
	a.Memory = &memory.Store{DB: db, Max: a.Cfg.Memory.MaxItems}

	a.Interp = &tools.Interpreters{RuntimesDir: a.Dirs.Runtimes()}
	sc := a.Cfg.Sandbox
	a.Runner = &sandbox.Runner{
		Sandbox: o.Sandbox, Mode: sc.Mode, RunsDir: a.Dirs.Runs(),
		Timeout: sc.ScriptTimeout.Duration, MemBytes: int64(sc.ScriptMemoryMB) << 20, MaxProcs: sc.ScriptMaxProcs,
		MaxFileBytes: int64(sc.ScriptMaxFileMB) << 20,
	}
	n := 1
	if m, err := platform.ReadMemory(); err == nil && m.TotalBytes > 18<<30 {
		n = 2
	}
	a.slots = make(chan struct{}, n)
	return a, nil
}

func (a *App) Close() error {
	done := make(chan struct{})
	go func() { a.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	a.mu.Lock()
	if a.egress != nil {
		a.egress.Close()
	}
	a.mu.Unlock()
	docsMu.Lock()
	if a.docs != nil {
		a.docs.Close()
	}
	docsMu.Unlock()
	return a.DB.Close()
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Egress starts the shared egress proxy on first use.
func (a *App) Egress() (*sandbox.Egress, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.egress == nil {
		e, err := sandbox.StartEgress()
		if err != nil {
			return nil, err
		}
		a.egress = e
		a.Runner.Egress = e
	}
	return a.egress, nil
}

// ---- backends ----

func (a *App) buildBackends() ([]NamedBackend, error) {
	var out []NamedBackend
	seen := map[string]int{}
	for _, bc := range a.Cfg.Backends {
		key := ""
		if name, ok := config.SecretRef(bc.APIKey); ok {
			v, ok := a.Secrets.Get(name)
			if !ok {
				return nil, fmt.Errorf("backend %s: secret %s isn't set: run `noskill secrets set %s`", bc.Kind, name, name)
			}
			key = v
		} else if bc.APIKey != "" {
			return nil, fmt.Errorf("backend %s: api_key must be a secret reference (secret:NAME)", bc.Kind)
		}
		b, err := backends.New(bc.Kind, bc.URL, key)
		if err != nil {
			return nil, err
		}
		id := bc.Kind
		if seen[bc.Kind]++; seen[bc.Kind] > 1 {
			id = fmt.Sprintf("%s-%d", bc.Kind, seen[bc.Kind])
		}
		out = append(out, NamedBackend{ID: id, Kind: bc.Kind, URL: bc.URL, Key: key, B: b})
	}
	return out, nil
}

// ---- skills ----

func (a *App) loadSkills(ctx context.Context) error {
	// An adopted tuned version of a built-in skill replaces its embedded SKILL.md (FR-L9, ADR-0014).
	builtin, tuned := a.Tuned().Overlay(skillfiles.Builtin())
	a.tunedMu.Lock()
	a.tunedIn = tuned
	a.tunedMu.Unlock()
	roots := []skills.Root{{Source: skills.SourceBuiltin, FS: builtin}}
	roots = append(roots, skills.DiskRoot(skills.SourceUser, a.Dirs.Skills()))
	roots = append(roots, skills.DiskRoot(skills.SourceLearned, a.Dirs.Learned()))
	for _, p := range a.Cfg.Skills.Paths {
		if ep, err := platform.ExpandHome(p); err == nil {
			roots = append(roots, skills.DiskRoot(skills.SourceExtra, ep))
		}
	}
	if a.Opts.Project != "" {
		roots = append(roots, skills.DiskRoot(skills.SourceProject, filepath.Join(a.Opts.Project, ".noskill", "skills")))
	}
	a.Skills, a.SkillIssues = skills.LoadAll(roots)
	now := time.Now()
	for _, s := range a.Skills.All() {
		perm, _ := json.Marshal(s.Meta.Permissions)
		path := s.Dir
		if path == "" {
			path = "builtin:" + s.Name
		}
		if _, err := a.DB.SyncSkill(ctx, store.SkillRecord{
			Name: s.Name, Origin: s.Origin(), Status: s.Status(), Version: s.Meta.Version, Path: path, Description: s.Description,
			Triggers: s.Meta.Triggers, Permissions: perm, Hash: s.Hash, NeedsApproval: s.NeedsApproval(),
		}, now); err != nil {
			return err
		}
	}
	return nil
}

// UsableSkills returns the skills a run may use: active status, enabled, and (for project skills)
// approved by the user.
func (a *App) UsableSkills(ctx context.Context) ([]*skills.Skill, error) {
	enabled, err := a.DB.EnabledSkills(ctx)
	if err != nil {
		return nil, err
	}
	var out []*skills.Skill
	for _, s := range a.Skills.All() {
		if enabled[s.Name] && s.Status() == "active" {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---- models ----

// Model is an installed model with what noskill knows about it.
type Model struct {
	backends.ModelInfo
	NB       NamedBackend
	Strategy backends.ToolStrategy // "" = never probed
	Loaded   bool
	LoadedSz int64
}

// Models lists installed models from every reachable backend (cached for a few seconds) and
// refreshes the stored profiles. Backends that can't be reached are reported, not fatal.
func (a *App) Models(ctx context.Context) ([]Model, []error) {
	a.mu.Lock()
	if time.Since(a.models.at) < 5*time.Second && a.models.list != nil {
		l, e := a.models.list, a.models.errs
		a.mu.Unlock()
		return l, e
	}
	a.mu.Unlock()
	var out []Model
	var errs []error
	for _, nb := range a.Backends {
		ms, err := nb.B.ListModels(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", nb.ID, err))
			continue
		}
		ver := ""
		if v, ok := nb.B.(interface {
			Version(context.Context) (string, error)
		}); ok {
			ver, _ = v.Version(ctx)
		}
		a.DB.UpsertBackend(ctx, nb.ID, nb.Kind, nb.URL, ver, time.Now())
		a.DB.UpsertModels(ctx, nb.ID, ms, time.Now())
		loaded, _ := nb.B.Loaded(ctx)
		for _, m := range ms {
			e := Model{ModelInfo: m, NB: nb}
			if s, ok, _ := a.DB.ToolStrategy(ctx, m.ID); ok {
				e.Strategy = s
			}
			for _, l := range loaded {
				if l.Name == m.Name {
					e.Loaded, e.LoadedSz = true, l.SizeBytes
				}
			}
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	a.mu.Lock()
	a.models.at, a.models.list, a.models.errs = time.Now(), out, errs
	if out == nil {
		a.models.list = []Model{}
	}
	a.mu.Unlock()
	return out, errs
}

// invalidateModels forces the next Models call to ask the backends again.
func (a *App) invalidateModels() {
	a.mu.Lock()
	a.models.at = time.Time{}
	a.mu.Unlock()
}

// FindModel resolves a name the user typed: "qwen3:4b", "ollama/qwen3:4b", or an unambiguous
// prefix without a tag ("qwen3" when only one qwen3 is installed).
func (a *App) FindModel(ctx context.Context, name string) (Model, error) {
	ms, errs := a.Models(ctx)
	for _, m := range ms {
		if m.Name == name || m.ID == name {
			return m, nil
		}
	}
	var cands []Model
	for _, m := range ms {
		if strings.HasPrefix(m.Name, name+":") || strings.HasPrefix(m.Name, name) && len(name) >= 4 {
			cands = append(cands, m)
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	var names []string
	for _, m := range ms {
		names = append(names, m.Name)
	}
	if len(ms) == 0 && len(errs) > 0 {
		return Model{}, &NoBackendError{Errs: errs}
	}
	if len(cands) > 1 {
		return Model{}, fmt.Errorf("%q matches several models (%s): give the full name", name, joinNames(cands))
	}
	return Model{}, &NoModelError{Name: name, Installed: names}
}

// UserError is a mistake in the request itself (an unusable skill, a model that can't chat): the
// proxy answers 400 and the CLI exits with the usage code.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// NoModelError: the named model isn't installed. The proxy answers 404 and lists valid names.
type NoModelError struct {
	Name      string
	Installed []string
}

func (e *NoModelError) Error() string {
	return fmt.Sprintf("no installed model named %q (installed: %s)", e.Name, strings.Join(e.Installed, ", "))
}

func joinNames(ms []Model) string {
	var n []string
	for _, m := range ms {
		n = append(n, m.Name)
	}
	return strings.Join(n, ", ")
}

// NoBackendError: nothing answered. The CLI maps it to exit code 5, the proxy to 503.
type NoBackendError struct{ Errs []error }

func (e *NoBackendError) Error() string {
	if len(e.Errs) == 0 {
		return "no model backend is configured"
	}
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return "no model backend is reachable: " + strings.Join(parts, "; ")
}

func (m Model) chat() bool { return m.Has(backends.CapChat) }

func (a *App) numCtx(m Model) int {
	n := a.Cfg.Orchestrator.DefaultCtx
	if n <= 0 {
		n = 8192
	}
	if m.Context > 0 && m.Context < n {
		n = m.Context
	}
	return n
}

// profiles turns installed chat models into router profiles, using measured memory when there is
// one and a conservative estimate otherwise.
func (a *App) profiles(ctx context.Context, ms []Model) []orchestrator.Profile {
	var out []orchestrator.Profile
	for _, m := range ms {
		if !m.chat() {
			continue
		}
		p := orchestrator.Profile{Info: m.ModelInfo, Ctx: a.numCtx(m)}
		// Quality per task type from `models bench` (FR-B4); "*" is their mean, for the types the
		// micro-eval doesn't cover. Unbenched models stay flat at 0.5.
		if sc, _, err := a.DB.Scores(ctx, m.ID); err == nil && len(sc) > 0 {
			p.Quality = sc
			var sum float64
			for _, v := range sc {
				sum += v
			}
			p.Quality["*"] = sum / float64(len(sc))
		}
		if perf, ok, _ := a.DB.GetPerf(ctx, m.ID); ok {
			p.SizeBytes, p.TokPerSec = perf.MemBytes, perf.TokPerSec
			if perf.LoadMsCold > 0 {
				p.LoadSeconds = perf.LoadMsCold / 1000
			}
		} else if m.LoadedSz > 0 {
			p.SizeBytes = m.LoadedSz
		} else {
			// Not measured: disk size plus room for the context. Over-estimating is the safe
			// direction (a model is left out rather than swapping the machine); `models bench`
			// replaces it with the real figure.
			p.SizeBytes = int64(float64(m.SizeBytes)*1.1) + 600_000_000
		}
		out = append(out, p)
	}
	return out
}

// decider builds the Decider: the decision model when installed, the chat model otherwise.
func (a *App) decider(ctx context.Context, ms []Model) decide.Decider {
	var chat *Model
	for i := range ms {
		if ms[i].chat() {
			if chat == nil || ms[i].Loaded && !chat.Loaded || (ms[i].Loaded == chat.Loaded && ms[i].SizeBytes < chat.SizeBytes) {
				chat = &ms[i]
			}
		}
	}
	var emu decide.Decider
	if chat != nil {
		emu = decide.Emulated{Backend: chat.NB.B, Model: chat.Name, NumCtx: a.numCtx(*chat)}
	}
	want := a.Cfg.Orchestrator.DecisionModel
	if want != "" {
		for _, m := range ms {
			if m.Name == want && m.Has(backends.CapDecide) && m.NB.Kind == "ollama" {
				jev := decide.JEV{BaseURL: m.NB.URL, Model: m.Name}
				if emu == nil {
					return jev
				}
				return decide.Fallback{Primary: jev, Secondary: emu}
			}
		}
	}
	return emu // may be nil: heuristics only
}

// memBudget is the model memory budget in bytes, and why.
func (a *App) memBudget() (int64, string, platform.Memory) {
	mem, _ := platform.ReadMemory()
	b, why := orchestrator.MemoryBudget(a.Cfg.Orchestrator.MemoryBudgetGB, mem)
	return b, why, mem
}

// liveBudget is the budget for a decision made now: the base, grown toward memory_burst_gb by what
// is free at this moment (ADR-0012). note is "" when it didn't grow.
func (a *App) liveBudget(models []Model) (budget, base int64, note string) {
	base, _, mem := a.memBudget()
	var ours int64
	for _, m := range models {
		if m.Loaded {
			ours += m.LoadedSz
		}
	}
	ceiling := orchestrator.BurstCeiling(a.Cfg.Orchestrator.MemoryBurstGB, base, mem)
	budget, note = orchestrator.BurstBudget(base, ceiling, mem, ours)
	return budget, base, note
}

// validateField checks a value the user typed into an approval field. Folder values must be
// acceptable as a write scope (SECURITY.md "Folders picked in approvals").
func (a *App) validateField(f approvals.Field, v string) (string, error) {
	if f.Type != "folder" {
		return v, nil
	}
	bound, _ := a.Volumes().Bindings(context.Background())
	exp, err := perms.Resolver{Volumes: a.Volumes(), VolumeUUIDs: bound}.Expand(v) // volume:RED/ANIME2026 works too
	if err != nil {
		return "", err
	}
	return perms.ValidateApproved(exp, a.Dirs.Own())
}

// diskFree reports free bytes under the data folder.
func (a *App) diskFree() uint64 {
	n, _ := platform.DiskFree(a.Dirs.Data)
	return n
}

var _ = fs.ErrNotExist
var _ = os.Getpid

// InvalidateModels makes the next Models call ask the backends again (after a pull or delete).
func (a *App) InvalidateModels() { a.invalidateModels() }

// DiskFree reports free bytes under the data folder.
func (a *App) DiskFree() uint64 { return a.diskFree() }
