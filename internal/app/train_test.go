package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/agent"
	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/config"
	"noskill/internal/eval"
	"noskill/internal/platform"
)

// writeTrace puts a finished run's trace where the learning loop reads them.
func writeTrace(t *testing.T, a *App, id, source, req, outcome string, args string) {
	t.Helper()
	now := time.Now()
	dir := filepath.Join(a.Dirs.Traces(), now.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	evs := []agent.Event{
		{Kind: agent.EventRunStart, V: agent.TraceVersion, Content: req, Model: "qwen3:4b", Source: source, Skills: []string{"files"}, Tool: "files"},
		{Kind: agent.EventToolCall, Step: 1, Tool: "list_dir", Skill: "files", Args: json.RawMessage(args)},
		{Kind: agent.EventToolResult, Step: 1, Tool: "list_dir", Content: "a.pdf\nb.txt"},
		{Kind: agent.EventAnswer, Content: "There are two files."},
		{Kind: agent.EventEnd, Note: "answered"},
		{Kind: agent.EventOutcome, Content: outcome, Tool: "explicit", Note: "1.00: rated"},
	}
	f, err := os.Create(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range evs {
		e.Time = now
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
}

// trainFake answers release questions right only on the trained model (when good is set).
func trainFake(good *bool) *backendtest.Fake {
	return &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		last := r.Messages[len(r.Messages)-1].Content
		if strings.Contains(last, "latest release") {
			if strings.Contains(r.Model, "-ns") && *good {
				return backends.Response{Content: "It's v2.1."}, nil
			}
			return backends.Response{Content: "No idea."}, nil
		}
		return backends.Response{Content: `{"task_type":"chat"}`}, nil
	}}
}

// Good runs from real use become a dataset; the gate refuses until there are enough and they're
// reviewed; a trained model that beats its base waits for adopt, then stands in for it in routing;
// one that doesn't is removed from Ollama (FR-L8, ADR-0015).
func TestTrainExportGateTrainAdopt(t *testing.T) {
	good := true
	fake := trainFake(&good)
	e := newApp(t, fake, func(c *config.Config) { c.Learning.LoRA.MinRuns = 3 }, "")
	a := e.app
	a.powerOverride = func() (bool, bool) { return true, true }
	ctx := context.Background()
	home, _ := os.UserHomeDir()
	at := func(p string) string {
		b, _ := json.Marshal(map[string]string{"path": filepath.Join(home, p)})
		return string(b)
	}

	writeTrace(t, a, "01A", "cli", "List the files in ~/Downloads", "success", at("Downloads"))
	writeTrace(t, a, "01B", "job", "Which files are in Desktop", "success", at("Desktop"))
	writeTrace(t, a, "01C", "cli", "Show me what's in Pictures please", "success", at("Pictures"))
	writeTrace(t, a, "01D", "proxy", "Count the files in Music", "success", at("Music"))
	writeTrace(t, a, "01E", "cli", "List the files in ~/Videos", "success", at("Videos")) // 01A with another folder
	writeTrace(t, a, "01F", "eval", "List the files in Movies", "success", at("Movies"))
	writeTrace(t, a, "01G", "cli", "Tidy the files in Documents", "partial", at("Documents"))

	d, err := a.TrainExport(ctx, TrainExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Runs != 4 || d.Examples != 8 || d.Valid == 0 || d.Skipped["a repeat of an earlier run"] != 1 || d.Skipped["a run from eval"] != 1 || d.Skipped["outcome partial"] != 1 {
		t.Fatalf("%+v", d)
	}
	b, _ := os.ReadFile(filepath.Join(d.Dir, "train.jsonl"))
	if strings.Contains(string(b), home) || !strings.Contains(string(b), `"path":"~/`) || !strings.Contains(string(b), `"name":"list_dir"`) {
		t.Errorf("train.jsonl:\n%s", b)
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ex struct {
			Messages []map[string]any
			Tools    []any
		}
		if err := json.Unmarshal(sc.Bytes(), &ex); err != nil || ex.Messages[0]["role"] != "system" || len(ex.Tools) == 0 ||
			!strings.Contains(ex.Messages[0]["content"].(string), "### files") {
			t.Fatalf("a line: %v %s", err, sc.Bytes())
		}
	}
	if rv, _ := os.ReadFile(filepath.Join(d.Dir, "REVIEW.md")); !strings.Contains(string(rv), "Count the files in Music") {
		t.Errorf("review:\n%s", rv)
	}

	// The gate.
	tasks := func() []eval.Task { return releaseTasks("lr", 6) }
	a.trainMem = func() (platform.Memory, error) {
		return platform.Memory{AppleSilicon: true, AvailableBytes: 12 << 30}, nil
	}
	if _, err := a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", tasks: tasks}); err == nil || !strings.Contains(err.Error(), "--reviewed") {
		t.Errorf("unreviewed: %v", err)
	}
	a.Cfg.Learning.LoRA.MinRuns = 200
	if _, err := a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", Reviewed: true, tasks: tasks}); err == nil || !strings.Contains(err.Error(), "needs 200") {
		t.Errorf("too few runs: %v", err)
	}
	a.Cfg.Learning.LoRA.MinRuns = 3
	a.trainMem = func() (platform.Memory, error) { return platform.Memory{AvailableBytes: 32 << 30}, nil }
	if _, err := a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", Reviewed: true, tasks: tasks}); err == nil || !strings.Contains(err.Error(), "Apple Silicon") {
		t.Errorf("not a Mac: %v", err)
	}
	a.trainMem = func() (platform.Memory, error) {
		return platform.Memory{AppleSilicon: true, AvailableBytes: 12 << 30}, nil
	}
	p, err := a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", Reviewed: true, DryRun: true, tasks: tasks})
	if err != nil || p.Run.Tag != "qwen3:4b-ns1" || p.Run.Iters != 100 || !strings.Contains(strings.Join(p.Commands[0], " "), "mlx-lm==0.26.0 python -m mlx_lm lora --model mlx-community/Qwen3-4B-4bit --train") {
		t.Fatalf("dry run: %v %+v", err, p)
	}
	if len(a.TrainRuns()) != 0 {
		t.Error("a dry run saved a run")
	}

	// Training, with uv and ollama faked.
	var ran []string
	a.trainCmd = func(_ context.Context, out io.Writer, name string, args ...string) error {
		line := name + " " + strings.Join(args, " ")
		ran = append(ran, line)
		switch {
		case strings.Contains(line, "mlx_lm lora"):
			io.WriteString(out, "Iter 10: Train loss 1.234\nIter 20: Val loss 1.100\n")
		case strings.HasPrefix(line, "ollama show"):
			io.WriteString(out, "# Modelfile\nFROM /blobs/sha256-base\nTEMPLATE \"{{ .Prompt }}\"\nPARAMETER stop \"<|im_end|>\"\n")
		case strings.HasPrefix(line, "ollama create"):
			m := fakeModels()[0]
			m.ID, m.Name = "fake/"+args[1], args[1]
			fake.Models = append(fake.Models, m)
		case strings.HasPrefix(line, "ollama rm"):
			fake.Models = fake.Models[:len(fake.Models)-1]
		}
		return nil
	}
	var progress []string
	p, err = a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", Reviewed: true, tasks: tasks, Progress: func(s string) { progress = append(progress, s) }})
	if err != nil {
		t.Fatal(err)
	}
	r := p.Run
	if r.Status != TrainCandidate || r.Score.Passed != 6 || r.BaseScore.Passed != 0 || len(ran) != 4 {
		t.Fatalf("%+v\n%s", r, strings.Join(ran, "\n"))
	}
	if !strings.Contains(strings.Join(progress, "\n"), "Iter 10: Train loss 1.234") {
		t.Errorf("progress:\n%s", strings.Join(progress, "\n"))
	}
	mf, _ := os.ReadFile(filepath.Join(a.trainRunDir(1), "Modelfile"))
	if !strings.Contains(string(mf), "FROM "+filepath.Join(a.trainRunDir(1), "fused")) || strings.Contains(string(mf), "/blobs/") || !strings.Contains(string(mf), "PARAMETER stop") {
		t.Errorf("Modelfile:\n%s", mf)
	}

	// Not routed to until adopted; then it stands in for its base.
	route := func() string {
		t.Helper()
		pl, err := a.Plan(ctx, RunRequest{Messages: []backends.Message{{Role: backends.RoleUser, Content: "hello there"}}, Source: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		return pl.pick.Name
	}
	a.invalidateModels()
	if got := route(); got == r.Tag {
		t.Errorf("routed to an unadopted candidate")
	}
	if _, err := a.TrainAdopt(1); err != nil {
		t.Fatal(err)
	}
	if got := route(); got != r.Tag {
		t.Errorf("adopted, routed to %s", got)
	}
	if _, err := a.TrainRevert(1); err != nil {
		t.Fatal(err)
	}
	if got := route(); got == r.Tag {
		t.Errorf("reverted, still routed to %s", got)
	}

	// A model that isn't better is removed from Ollama.
	good, ran = false, nil
	p, err = a.TrainLoRA(ctx, TrainOptions{Data: d.ID, Base: "qwen3:4b", Reviewed: true, tasks: tasks})
	if err != nil || p.Run.Status != TrainRejected || p.Run.Tag != "qwen3:4b-ns2" || ran[len(ran)-1] != "ollama rm qwen3:4b-ns2" {
		t.Fatalf("%v %+v\n%s", err, p.Run, strings.Join(ran, "\n"))
	}
	if _, err := a.TrainAdopt(2); err == nil {
		t.Error("adopted a rejected model")
	}
}

func TestTrainExportWithNothingToTrainOn(t *testing.T) {
	e := newApp(t, trainFake(new(bool)), nil, "")
	writeTrace(t, e.app, "01X", "eval", "List the files in Movies", "success", `{"path":"x"}`)
	d, err := e.app.TrainExport(context.Background(), TrainExportOptions{})
	if err != nil || d.Runs != 0 || d.Dir != "" || d.Skipped["a run from eval"] != 1 {
		t.Fatalf("%v %+v", err, d)
	}
}
