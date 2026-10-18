package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"noskill/internal/eval"
	"noskill/internal/learning"
	"noskill/internal/platform"
	"noskill/internal/skills"
	"noskill/internal/tools"
)

// Weight learning (FR-L8, ADR-0015): `noskill train export` turns good runs from real use into an
// mlx-lm chat dataset; `train lora` trains a LoRA on it with MLX through uv, puts the result in
// Ollama as <base>-ns<n>, and benchmarks it against its base on the checked tasks. A trained model
// is never routed to until `train adopt`; then it stands in for its base on its task type.

const (
	TrainTraining  = "training"
	TrainCandidate = "candidate" // benchmarked better; waiting for adopt
	TrainActive    = "active"    // stands in for its base
	TrainReverted  = "reverted"
	TrainRejected  = "rejected" // not better: removed from Ollama, adapter kept
	TrainFailed    = "failed"

	trainMinFreeBytes = 8 << 30 // a 4-bit 4B QLoRA with gradient checkpointing, and room around it
	trainMinTasks     = 5
)

func (a *App) TrainDir() string { return filepath.Join(a.Dirs.Data, "train") }

func (a *App) datasetDir(id string) string { return filepath.Join(a.TrainDir(), "datasets", id) }

func (a *App) trainRunDir(n int) string {
	return filepath.Join(a.TrainDir(), "runs", strconv.Itoa(n))
}

// Dataset is a dataset's MANIFEST.json.
type Dataset struct {
	ID       string         `json:"id"`
	Made     time.Time      `json:"made"`
	Task     string         `json:"task,omitempty"` // "" = every task type
	Since    string         `json:"since"`
	Base     string         `json:"base,omitempty"` // the model the system prompts were written for
	Runs     int            `json:"runs"`
	Examples int            `json:"examples"`
	Train    int            `json:"train"` // examples in train.jsonl
	Valid    int            `json:"valid"`
	RunIDs   []string       `json:"run_ids"`
	Skipped  map[string]int `json:"skipped"` // why → runs
	Dir      string         `json:"-"`
}

type TrainExportOptions struct {
	Task  string
	Since time.Duration // 0 = 30 days
	Base  string
}

// TrainExport writes a dataset of the good runs (ADR-0015). With no usable run it writes nothing
// and says why in Skipped.
func (a *App) TrainExport(ctx context.Context, o TrainExportOptions) (*Dataset, error) {
	if o.Since <= 0 {
		o.Since = 30 * 24 * time.Hour
	}
	small := true // noskill is built for small models; a named base says for sure
	if o.Base != "" {
		m, err := a.FindModel(ctx, o.Base)
		if err != nil {
			return nil, err
		}
		o.Base, small = m.Name, a.smallModel(m)
	}
	d := &Dataset{Made: time.Now(), Task: o.Task, Since: o.Since.String(), Base: o.Base, Skipped: map[string]int{}}
	scrub := a.trainScrubber()
	cutoff := time.Now().Add(-o.Since)
	seen := map[string]bool{}
	var train, valid []learning.Example
	lastN := 0 // examples of the last run put in train
	var review strings.Builder
	var paths []string
	filepath.WalkDir(a.Dirs.Traces(), func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(p, ".jsonl") {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths) // oldest first: YYYY/MM/DD/<ulid>
	for _, p := range paths {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		t, err := learning.ReadTrace(p)
		if err != nil {
			d.Skipped["unreadable"]++
			continue
		}
		why := ""
		if ok, w := learning.Trainable(t); !ok {
			why = w
		} else if t.Start.Before(cutoff) {
			why = "older than " + o.Since.String()
		} else if o.Task != "" && t.TaskType != o.Task {
			why = "another task type"
		} else if seen[learning.DedupKey(t)] {
			why = "a repeat of an earlier run"
		}
		var sel []*skills.Skill
		for _, n := range t.Skills {
			if why != "" {
				break
			}
			if sk, ok := a.Skills.Get(n); ok {
				sel = append(sel, sk)
			} else {
				why = "a skill it used is gone"
			}
		}
		if why != "" {
			// Every failure reason with a value in it ("outcome partial") counts under its own words.
			d.Skipped[why]++
			continue
		}
		seen[learning.DedupKey(t)] = true
		system := fmt.Sprintf(basePrompt, t.Start.Format("Monday 2 January 2006, 15:04 MST"), runtime.GOOS)
		if len(sel) > 0 {
			system += "\n\n" + skillsPrompt(sel, small)
		}
		ex := learning.Examples(t, system, a.chatTools(sel), scrub)
		if learning.ValidRun(t.RunID) {
			valid, lastN = append(valid, ex...), 0
		} else {
			train, lastN = append(train, ex...), len(ex)
		}
		d.RunIDs = append(d.RunIDs, t.RunID)
		d.Runs++
		review.WriteString(learning.ReviewText(t, scrub))
	}
	if d.Runs == 0 {
		return d, nil
	}
	// mlx-lm wants a validation set: with none by hash, the last run's examples are it.
	if len(valid) == 0 && d.Runs > 1 {
		train, valid = train[:len(train)-lastN], train[len(train)-lastN:]
	}
	d.Train, d.Valid, d.Examples = len(train), len(valid), len(train)+len(valid)
	task := o.Task
	if task == "" {
		task = "all"
	}
	d.ID = d.Made.Format("20060102-150405") + "-" + task
	d.Dir = a.datasetDir(d.ID)
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return nil, err
	}
	for name, ex := range map[string][]learning.Example{"train.jsonl": train, "valid.jsonl": valid} {
		if err := writeJSONL(filepath.Join(d.Dir, name), ex); err != nil {
			return nil, err
		}
	}
	head := fmt.Sprintf("# Dataset %s\n\n%d runs, %d examples (one per assistant turn). Read every run below. To leave one out, delete its lines from train.jsonl or valid.jsonl (each line's user message is the request). Then `noskill train lora --data %s --base <model> --reviewed`.\n\n", d.ID, d.Runs, d.Examples, d.ID)
	if err := os.WriteFile(filepath.Join(d.Dir, "REVIEW.md"), []byte(head+review.String()), 0o600); err != nil {
		return nil, err
	}
	return d, writeJSON(filepath.Join(d.Dir, "MANIFEST.json"), d)
}

// trainScrubber replaces secret values and the home folder in dataset text (ADR-0015). Traces
// already have secrets replaced when written; this catches ones added since.
func (a *App) trainScrubber() func(string) string {
	var pairs []string
	for _, s := range a.secretValuesAll() {
		if len(s) >= 4 {
			pairs = append(pairs, s, "[secret]")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		// As JSON writes it too: C:\Users\me is C:\\Users\\me inside an argument.
		if esc := strings.ReplaceAll(home, `\`, `\\`); esc != home {
			pairs = append(pairs, esc, "~")
		}
		pairs = append(pairs, home, "~")
	}
	r := strings.NewReplacer(pairs...)
	return r.Replace
}

// chatTools are the tool definitions the selected skills offer, as mlx-lm wants them.
func (a *App) chatTools(sel []*skills.Skill) []learning.ChatTool {
	if len(sel) == 0 {
		return nil
	}
	e := &tools.Executor{Offered: skills.Tools(sel), Registry: a.Registry}
	var out []learning.ChatTool
	for _, d := range e.Defs() {
		out = append(out, learning.ChatTool{Type: "function", Function: learning.ChatToolSpec{Name: d.Name, Description: d.Description, Parameters: d.Parameters}})
	}
	return out
}

func writeJSONL(p string, ex []learning.Example) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, e := range ex {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return os.WriteFile(p, b.Bytes(), 0o600)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p) // noskill's own state under <data>/train
}

// Dataset reads a dataset's manifest.
func (a *App) Dataset(id string) (*Dataset, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return nil, &UserError{fmt.Sprintf("no dataset %q", id)}
	}
	b, err := os.ReadFile(filepath.Join(a.datasetDir(id), "MANIFEST.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &UserError{fmt.Sprintf("no dataset %q (see `noskill train list`)", id)}
	}
	if err != nil {
		return nil, err
	}
	var d Dataset
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.Dir = a.datasetDir(id)
	return &d, nil
}

// Datasets lists the exported datasets, newest first.
func (a *App) Datasets() []*Dataset {
	ents, _ := os.ReadDir(filepath.Join(a.TrainDir(), "datasets"))
	var out []*Dataset
	for i := len(ents) - 1; i >= 0; i-- {
		if d, err := a.Dataset(ents[i].Name()); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// TrainRun is a training run's RUN.json.
type TrainRun struct {
	N         int            `json:"n"`
	Made      time.Time      `json:"made"`
	Base      string         `json:"base"`
	Repo      string         `json:"mlx_repo"`
	Tag       string         `json:"tag"` // the Ollama model it made
	Data      string         `json:"dataset"`
	Task      string         `json:"task,omitempty"`
	Runs      int            `json:"runs"`
	Examples  int            `json:"examples"`
	Iters     int            `json:"iters"`
	Reviewed  bool           `json:"reviewed"`
	Status    string         `json:"status"`
	BaseScore learning.Score `json:"base_score"`
	Score     learning.Score `json:"score"`
	Tasks     []string       `json:"tasks,omitempty"`
	Why       string         `json:"why,omitempty"`
}

// TrainRuns lists the training runs, newest first.
func (a *App) TrainRuns() []*TrainRun {
	ents, _ := os.ReadDir(filepath.Join(a.TrainDir(), "runs"))
	var out []*TrainRun
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if r, err := a.trainRun(n); err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N > out[j].N })
	return out
}

func (a *App) trainRun(n int) (*TrainRun, error) {
	b, err := os.ReadFile(filepath.Join(a.trainRunDir(n), "RUN.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &UserError{fmt.Sprintf("no training run %d (see `noskill train list`)", n)}
	}
	if err != nil {
		return nil, err
	}
	var r TrainRun
	return &r, json.Unmarshal(b, &r)
}

func (a *App) saveTrainRun(r *TrainRun) error {
	return writeJSON(filepath.Join(a.trainRunDir(r.N), "RUN.json"), r)
}

type TrainOptions struct {
	Data     string // dataset id
	Base     string // Ollama model to train on
	Iters    int    // 0 = two epochs, 100–1000
	Reviewed bool   // the user read REVIEW.md
	Force    bool   // on battery too
	DryRun   bool   // check and print the commands only
	Progress func(string)
	tasks    func() []eval.Task // tests: in place of the checked tasks
}

// TrainPlan is what `train lora` will run (all of it, in --dry-run).
type TrainPlan struct {
	Run      *TrainRun
	Commands [][]string
}

// trainTag is the Ollama name of run n's model: qwen3:4b → qwen3:4b-ns1.
func trainTag(base string, n int) string {
	if name, tag, ok := strings.Cut(base, ":"); ok {
		return fmt.Sprintf("%s:%s-ns%d", name, tag, n)
	}
	return fmt.Sprintf("%s:ns%d", base, n)
}

// TrainLoRA checks the gate, trains, imports and benchmarks (ADR-0015).
func (a *App) TrainLoRA(ctx context.Context, o TrainOptions) (*TrainPlan, error) {
	say := func(f string, args ...any) {
		if o.Progress != nil {
			o.Progress(fmt.Sprintf(f, args...))
		}
	}
	d, err := a.Dataset(o.Data)
	if err != nil {
		return nil, err
	}
	cfg := a.Cfg.Learning.LoRA
	switch {
	case d.Runs < cfg.MinRuns:
		return nil, &UserError{fmt.Sprintf("dataset %s has %d good runs; training needs %d (learning.lora.min_runs). A LoRA on fewer mostly learns those requests' wording. Use noskill for longer and export again", d.ID, d.Runs, cfg.MinRuns)}
	case !o.Reviewed:
		return nil, &UserError{fmt.Sprintf("read %s first, remove any run you don't want trained on, then pass --reviewed", filepath.Join(d.Dir, "REVIEW.md"))}
	}
	if a.Cfg.Learning.RequireAC && !o.Force {
		if on, known := a.onAC(); known && !on {
			return nil, &UserError{"on battery: training takes an hour or more at full power. Plug in, or pass --force"}
		}
	}
	readMem := platform.ReadMemory
	if a.trainMem != nil {
		readMem = a.trainMem
	}
	mem, _ := readMem()
	if !mem.AppleSilicon {
		return nil, &UserError{"LoRA training needs Apple Silicon (MLX) in this version; the CUDA (Unsloth) path isn't built yet (ADR-0015). `train export` works here, and its files suit other trainers"}
	}
	if mem.AvailableBytes > 0 && mem.AvailableBytes < trainMinFreeBytes && !o.DryRun {
		return nil, &UserError{fmt.Sprintf("only %.1f GB of memory is free; training needs about %d GB. Close something (or wait for loaded models to unload) and try again", float64(mem.AvailableBytes)/(1<<30), trainMinFreeBytes>>30)}
	}
	m, err := a.FindModel(ctx, o.Base)
	if err != nil {
		return nil, err
	}
	repo := cfg.MLXModels[m.Name]
	if repo == "" {
		return nil, &UserError{fmt.Sprintf("no MLX build is known for %s. Add one to learning.lora.mlx_models (for example \"%s\" = \"mlx-community/<model>-4bit\")", m.Name, m.Name)}
	}
	tasks, err := a.trainTasks(o, d.Task)
	if err != nil {
		return nil, err
	}

	n := 1
	for _, r := range a.TrainRuns() {
		n = max(n, r.N+1)
	}
	iters := o.Iters
	if iters <= 0 {
		iters = min(1000, max(100, 2*d.Train))
	}
	dir := a.trainRunDir(n)
	r := &TrainRun{N: n, Made: time.Now(), Base: m.Name, Repo: repo, Tag: trainTag(m.Name, n), Data: d.ID, Task: d.Task,
		Runs: d.Runs, Examples: d.Examples, Iters: iters, Reviewed: o.Reviewed, Status: TrainTraining}
	for _, t := range tasks {
		r.Tasks = append(r.Tasks, t.ID)
	}
	mlx := []string{"uv", "run", "--isolated", "--with", "mlx-lm==" + cfg.MLXLM, "python", "-m", "mlx_lm"}
	plan := &TrainPlan{Run: r, Commands: [][]string{
		append(append([]string(nil), mlx...), "lora", "--model", repo, "--train", "--data", d.Dir,
			"--adapter-path", filepath.Join(dir, "adapters"), "--iters", strconv.Itoa(iters), "--batch-size", "1",
			"--num-layers", "8", "--mask-prompt", "--grad-checkpoint", "--max-seq-length", "4096"),
		append(append([]string(nil), mlx...), "fuse", "--model", repo, "--adapter-path", filepath.Join(dir, "adapters"),
			"--save-path", filepath.Join(dir, "fused"), "--dequantize"),
		{"ollama", "show", "--modelfile", m.Name},
		{"ollama", "create", r.Tag, "-q", "q4_K_M", "-f", filepath.Join(dir, "Modelfile")},
	}}
	if o.DryRun {
		return plan, nil
	}
	run := a.trainCmd
	if run == nil {
		if _, err := exec.LookPath("uv"); err != nil {
			return nil, &UserError{"training runs MLX through uv, which isn't installed: see https://docs.astral.sh/uv/ (on a Mac: brew install uv)"}
		}
		run = runLogged
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := a.saveTrainRun(r); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "train.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	fail := func(step string, err error) (*TrainPlan, error) {
		r.Status, r.Why = TrainFailed, fmt.Sprintf("%s: %v (see %s)", step, err, filepath.Join(dir, "train.log"))
		a.saveTrainRun(r)
		return plan, errors.New(r.Why)
	}
	out := io.MultiWriter(logf, progressLines(say))
	say("training %s on %d examples from %d runs, %d iterations (the first run downloads mlx-lm and %s, about 2.5 GB for a 4B model)", m.Name, d.Examples, d.Runs, iters, repo)
	if err := run(ctx, out, plan.Commands[0][0], plan.Commands[0][1:]...); err != nil {
		return fail("training", err)
	}
	say("merging the adapter into the model")
	if err := run(ctx, out, plan.Commands[1][0], plan.Commands[1][1:]...); err != nil {
		return fail("merging", err)
	}
	var mf bytes.Buffer
	if err := run(ctx, &mf, plan.Commands[2][0], plan.Commands[2][1:]...); err != nil {
		return fail("reading the base's Modelfile", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Modelfile"), []byte(trainModelfile(mf.String(), filepath.Join(dir, "fused"))), 0o600); err != nil {
		return fail("writing the Modelfile", err)
	}
	say("importing it into Ollama as %s", r.Tag)
	if err := run(ctx, out, plan.Commands[3][0], plan.Commands[3][1:]...); err != nil {
		return fail("importing into Ollama", err)
	}
	a.invalidateModels()

	say("benchmarking %s against %s on %d checked tasks", r.Tag, m.Name, len(tasks))
	score := func(model string) learning.Score {
		var s learning.Score
		for _, t := range tasks {
			if ctx.Err() != nil {
				break
			}
			res, _ := a.evalTaskWith(ctx, model, t, false, nil)
			s.Total++
			s.Steps += res.Steps
			mark := "✗"
			if res.Passed && !res.Skipped {
				s.Passed++
				mark = "✓"
			}
			say("    %s %s %s (%d steps)", mark, model, t.ID, res.Steps)
		}
		return s
	}
	r.BaseScore = score(m.Name)
	r.Score = score(r.Tag)
	if err := ctx.Err(); err != nil {
		return fail("benchmarking", err)
	}
	if learning.ClearlyBetter(r.Score, r.BaseScore) {
		r.Status = TrainCandidate
		r.Why = fmt.Sprintf("%s against %s for %s: `noskill train adopt %d` routes %s requests to it", r.Score, r.BaseScore, m.Name, n, orAll(d.Task))
	} else {
		r.Status = TrainRejected
		r.Why = fmt.Sprintf("not clearly better: %s against %s for %s", r.Score, r.BaseScore, m.Name)
		var sink bytes.Buffer
		if err := run(ctx, &sink, "ollama", "rm", r.Tag); err != nil {
			r.Why += "; `ollama rm " + r.Tag + "` failed: " + err.Error()
		}
		a.invalidateModels()
	}
	return plan, a.saveTrainRun(r)
}

func orAll(task string) string {
	if task == "" {
		return "all"
	}
	return task
}

// trainTasks are the checked agent tasks a trained model is benchmarked on: the dataset's task
// type, or every agent task.
func (a *App) trainTasks(o TrainOptions, task string) ([]eval.Task, error) {
	var all []eval.Task
	if o.tasks != nil {
		all = o.tasks()
	} else {
		var types []string
		if task != "" {
			types = []string{task}
		}
		ts, err := a.EvalTasks(nil, types, false)
		if err != nil {
			return nil, err
		}
		all = ts
	}
	var out []eval.Task
	for _, t := range all {
		if t.Agent() {
			out = append(out, t)
		}
	}
	if len(out) < trainMinTasks {
		return nil, &UserError{fmt.Sprintf("only %d checked tasks of type %s to benchmark a trained model on; it needs %d. Export with another --task, or add tasks with `noskill eval pin`", len(out), orAll(task), trainMinTasks)}
	}
	return out, nil
}

// trainModelfile is the base's Modelfile pointing at the fused weights: its template and
// parameters carry over.
func trainModelfile(base, fused string) string {
	var out []string
	from := false
	for _, l := range strings.Split(base, "\n") {
		u := strings.ToUpper(strings.TrimSpace(l))
		switch {
		case strings.HasPrefix(u, "FROM "):
			if !from {
				out = append(out, "FROM "+fused)
				from = true
			}
		case strings.HasPrefix(u, "ADAPTER "):
		default:
			out = append(out, l)
		}
	}
	if !from {
		out = append([]string{"FROM " + fused}, out...)
	}
	return strings.Join(out, "\n")
}

// runLogged runs a command with its output (both streams) to out.
func runLogged(ctx context.Context, out io.Writer, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = out, out
	c.WaitDelay = 10 * time.Second
	return c.Run()
}

// progressLines passes mlx-lm's iteration and loss lines to the progress callback.
func progressLines(say func(string, ...any)) io.Writer { return &lineWriter{say: say} }

type lineWriter struct {
	say func(string, ...any)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		if l := strings.TrimSpace(string(w.buf[:i])); strings.HasPrefix(l, "Iter ") || strings.Contains(l, "Saved") {
			w.say("    %s", l)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// TrainAdopt makes a candidate stand in for its base (the only active one for that base and task).
func (a *App) TrainAdopt(n int) (*TrainRun, error) {
	r, err := a.trainRun(n)
	if err != nil {
		return nil, err
	}
	if r.Status != TrainCandidate && r.Status != TrainReverted {
		return nil, &UserError{fmt.Sprintf("training run %d is %s; only a benchmarked candidate (or a reverted one) can be adopted", n, r.Status)}
	}
	for _, o := range a.TrainRuns() {
		if o.N != n && o.Status == TrainActive && o.Base == r.Base && o.Task == r.Task {
			o.Status = TrainReverted
			if err := a.saveTrainRun(o); err != nil {
				return nil, err
			}
		}
	}
	r.Status = TrainActive
	return r, a.saveTrainRun(r)
}

// TrainRevert stops an adopted model standing in for its base.
func (a *App) TrainRevert(n int) (*TrainRun, error) {
	r, err := a.trainRun(n)
	if err != nil {
		return nil, err
	}
	if r.Status != TrainActive {
		return nil, &UserError{fmt.Sprintf("training run %d is %s, not in use", n, r.Status)}
	}
	r.Status = TrainReverted
	return r, a.saveTrainRun(r)
}

// trainedRouting is what routing needs: models it must not pick, and the stand-ins for a base.
func (a *App) trainedRouting() (hidden []string, standIn func(base, task string) string) {
	runs := a.TrainRuns()
	for _, r := range runs {
		if r.Status != TrainActive && r.Tag != "" {
			hidden = append(hidden, r.Tag)
		}
	}
	return hidden, func(base, task string) string {
		for _, r := range runs { // newest first
			if r.Status == TrainActive && r.Base == base && (r.Task == "" || r.Task == task) {
				return r.Tag
			}
		}
		return ""
	}
}

// standIn swaps the pick for its adopted trained model, if there is one and it's installed.
func (a *App) standIn(p *plan, models []Model, standIn func(base, task string) string) {
	tag := standIn(p.pick.Name, p.class.TaskType)
	if tag == "" {
		return
	}
	for _, m := range models {
		if m.Name == tag && m.chat() {
			p.decision.Explanation.Notes = append(p.decision.Explanation.Notes,
				fmt.Sprintf("%s runs as %s, its LoRA trained on your runs (`noskill train revert` stops this)", p.pick.Name, tag))
			p.pick = m
			p.decision.Model = m.Name
			return
		}
	}
}
