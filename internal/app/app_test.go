package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"noskill/internal/agent"
	"noskill/internal/approvals"
	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/config"
	"noskill/internal/decide"
	"noskill/internal/eval"
	"noskill/internal/platform"
	"noskill/internal/secrets"
	"noskill/internal/skills"
	"noskill/internal/store"
	"noskill/internal/tools"
)

func TestMain(m *testing.M) {
	platform.MaybeRunSandboxChild()
	os.Exit(m.Run())
}

var chatCaps = []backends.Capability{backends.CapChat, backends.CapTools, backends.CapSchema}

func fakeModels() []backends.ModelInfo {
	mk := func(name string, gb float64, params float64, caps ...backends.Capability) backends.ModelInfo {
		return backends.ModelInfo{ID: "fake/" + name, Backend: "fake", Name: name, ParamsB: params, Context: 32768, SizeBytes: int64(gb * 1e9),
			Capabilities: append(append([]backends.Capability{}, chatCaps...), caps...)}
	}
	return []backends.ModelInfo{mk("qwen3:4b", 2.5, 4), mk("qwen2.5-coder:7b", 4.7, 7)}
}

type env struct {
	app  *App
	fake *backendtest.Fake
	home string
}

func newApp(t *testing.T, fake *backendtest.Fake, mutate func(*config.Config), project string) *env {
	t.Helper()
	home := t.TempDir()
	l, err := config.Load(config.Options{File: filepath.Join(home, "config.toml")})
	if err != nil {
		t.Fatal(err)
	}
	// Routing mustn't depend on how much memory the test machine has free right now.
	l.Config.Orchestrator.MemoryBurstGB = config.AutoFloat{Value: 0}
	if mutate != nil {
		mutate(&l.Config)
	}
	if fake.Models == nil {
		fake.Models = fakeModels()
	}
	a, err := Open(context.Background(), Options{
		Dirs: platform.Dirs{Config: home, Data: home}, Loaded: l, Project: project,
		Secrets:  secrets.NewManager(&secrets.Mem{}, filepath.Join(home, "secrets.names")),
		Backends: []NamedBackend{{ID: "fake", Kind: "fake", B: fake}},
		Notifier: quietNotifier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return &env{app: a, fake: fake, home: home}
}

type quietNotifier struct{}

func (quietNotifier) SupportsActions() bool                               { return false }
func (quietNotifier) Notify(context.Context, platform.Notification) error { return nil }

// noRules removes the routing rules, so the task type doesn't matter and no classification call is made.
func noRules(c *config.Config) { c.Orchestrator.Rules = nil }

func user(s string) []backends.Message {
	return []backends.Message{{Role: backends.RoleUser, Content: s}}
}

func TestPlainQuestion(t *testing.T) {
	// The default code rule makes the task type matter, and no decision model is installed, so the
	// chat model answers the classification first (emulated, constrained JSON).
	fake := &backendtest.Fake{Script: []backends.Response{{Content: `{"task_type":"chat"}`}, {Content: "Paris."}}}
	e := newApp(t, fake, nil, "")
	res, err := e.app.Run(context.Background(), RunRequest{Source: "cli",
		Messages: []backends.Message{{Role: backends.RoleSystem, Content: "Answer in one word."}, {Role: backends.RoleUser, Content: "Capital of France?"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "Paris." || res.Stop != agent.StopAnswered || res.Model == "" || len(res.Skills) != 0 || res.Class.Source != "emulated" {
		t.Fatalf("%+v", res)
	}
	if fake.Requests[0].Format == nil {
		t.Error("the classification call must be constrained JSON")
	}
	req := fake.Requests[1]
	sys := req.Messages[0].Content
	if req.Messages[0].Role != backends.RoleSystem || !strings.Contains(sys, "You are noskill") || !strings.Contains(sys, "Answer in one word.") {
		t.Errorf("system prompt = %q", sys)
	}
	if strings.Index(sys, "You are noskill") > strings.Index(sys, "Answer in one word.") {
		t.Error("the client's system prompt must come after noskill's and be kept whole")
	}
	if req.Think == nil || *req.Think {
		t.Error("thinking must be off unless asked for")
	}
	run, err := e.app.DB.GetRun(context.Background(), res.RunID)
	if err != nil || run.Status != "done" || run.Steps != 1 || run.Tokens == 0 || run.DriverModel != res.Model || run.TracePath == "" {
		t.Fatalf("run = %+v %v", run, err)
	}
	b, err := os.ReadFile(run.TracePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{`"kind":"route"`, `"kind":"select"`, `"kind":"answer"`, `"kind":"end"`} {
		if !strings.Contains(string(b), kind) {
			t.Errorf("trace lacks %s:\n%s", kind, b)
		}
	}
	if !strings.Contains(string(b), "Capital of France?") {
		t.Error("trace should record the request")
	}
}

func TestNoBackendAndUnknownModel(t *testing.T) {
	e := newApp(t, &backendtest.Fake{Models: []backends.ModelInfo{}}, nil, "")
	_, err := e.app.Run(context.Background(), RunRequest{Messages: user("hi")})
	if err == nil || !strings.Contains(err.Error(), "no chat model is installed") {
		t.Errorf("no models: %v", err)
	}
	e = newApp(t, &backendtest.Fake{}, nil, "")
	_, err = e.app.Run(context.Background(), RunRequest{Messages: user("hi"), Model: "gpt-9"})
	if err == nil || !strings.Contains(err.Error(), "qwen3:4b") {
		t.Errorf("unknown model must list the installed ones: %v", err)
	}
	var nb *NoBackendError
	dead := newApp(t, &backendtest.Fake{}, nil, "")
	dead.app.Backends = []NamedBackend{{ID: "ollama", Kind: "ollama", B: &deadBackend{}}}
	_, err = dead.app.Run(context.Background(), RunRequest{Messages: user("hi")})
	if !errors.As(err, &nb) {
		t.Errorf("unreachable backend: %v", err)
	}
}

type deadBackend struct{ backendtest.Fake }

func (*deadBackend) ListModels(context.Context) ([]backends.ModelInfo, error) {
	return nil, &backends.Error{Backend: "ollama", Status: 503, Msg: "can't reach 127.0.0.1:11434: connection refused"}
}

func TestSkillSelectionAndToolCall(t *testing.T) {
	var searched string
	fake := &backendtest.Fake{}
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		last := r.Messages[len(r.Messages)-1]
		if last.Role == backends.RoleTool {
			return backends.Response{Content: "Ollama 0.40 is current [1]."}, nil
		}
		return backends.Response{ToolCalls: []backends.ToolCall{{Name: "search", Args: json.RawMessage(`{"query":"ollama release","max_results":"2"}`)}}}, nil
	}
	e := newApp(t, fake, noRules, "")
	real := e.app.Registry["web.search"]
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: real.Schema, Run: func(_ context.Context, _ *tools.Env, a json.RawMessage) (string, error) {
		searched = string(a)
		return `[{"title":"Release","url":"https://example.org","snippet":"0.40"}]`, nil
	}}
	res, err := e.app.Run(context.Background(), RunRequest{Messages: user("look up the latest ollama release please")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "Ollama 0.40 is current [1]." || res.Steps != 2 {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(strings.Join(res.Skills, ","), "web-search") {
		t.Errorf("skills = %v (a request to look something up must select web-search)", res.Skills)
	}
	if !strings.Contains(searched, `"max_results":2`) {
		t.Errorf("arguments were not repaired (string → integer): %s", searched)
	}
	first := fake.Requests[0]
	var names []string
	for _, d := range first.Tools {
		names = append(names, d.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "search") || !strings.Contains(strings.Join(names, ","), "fetch") {
		t.Errorf("tools offered = %v", names)
	}
	if !strings.Contains(first.Messages[0].Content, "### web-search") || strings.Contains(first.Messages[0].Content, "Implementation spec") {
		t.Errorf("skill instructions missing or leaking notes:\n%s", first.Messages[0].Content)
	}
	run, _ := e.app.DB.GetRun(context.Background(), res.RunID)
	var n int
	e.app.DB.QueryRow(`SELECT count(*) FROM run_skills WHERE run_id = ?`, res.RunID).Scan(&n)
	if n < 1 || run.Status != "done" {
		t.Errorf("run_skills = %d, run = %+v", n, run)
	}
	// A client token that may not use web-search never gets it.
	fake2 := &backendtest.Fake{Script: []backends.Response{{Content: "I can't look that up."}}}
	e2 := newApp(t, fake2, noRules, "")
	res, err = e2.app.Run(context.Background(), RunRequest{Messages: user("look up the latest ollama release please"),
		ClientSkills: func(n string) bool { return n == "files" }})
	if err != nil || len(fake2.Requests[0].Tools) != 0 || len(res.Skills) != 0 {
		t.Errorf("token scope ignored: %+v %v", res, err)
	}
}

func TestRoutingUsesTheCodeRuleAndPinning(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: "done"}, nil }}
	e := newApp(t, fake, nil, "")
	res, err := e.app.Run(context.Background(), RunRequest{NoSkills: true, Messages: user("fix this:\n```go\nfunc main() {}\n```")})
	if err != nil || res.Model != "qwen2.5-coder:7b" || res.Class.TaskType != "code" || !strings.Contains(res.Explanation.Why, "user rule") {
		t.Fatalf("%+v %v", res, err)
	}
	if fake.Requests[0].Model != "qwen2.5-coder:7b" {
		t.Errorf("request went to %s", fake.Requests[0].Model)
	}
	// Pinning bypasses classification and the rules.
	res, err = e.app.Run(context.Background(), RunRequest{NoSkills: true, Model: "qwen3:4b", Messages: user("fix this:\n```go\nfunc main() {}\n```")})
	if err != nil || res.Model != "qwen3:4b" || res.Class.Source != "pinned" {
		t.Fatalf("%+v %v", res, err)
	}
	// Plain names without a tag resolve when unambiguous.
	if res, _ = e.app.Run(context.Background(), RunRequest{NoSkills: true, Model: "qwen2.5-coder", Messages: user("hi")}); res == nil || res.Model != "qwen2.5-coder:7b" {
		t.Errorf("prefix: %+v", res)
	}
}

func TestFilesNeedApprovalAndLeaveAnUndoableTrail(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "downloads")
	os.MkdirAll(work, 0o755)
	src := filepath.Join(work, "a.mkv")
	os.WriteFile(src, []byte("episode"), 0o644)
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	plan := map[string]any{"plan": []map[string]string{{"from": src, "to": dst}}, "dry_run": false}
	args, _ := json.Marshal(plan)

	script := func() *backendtest.Fake {
		return &backendtest.Fake{Script: []backends.Response{
			{ToolCalls: []backends.ToolCall{{Name: "move_many", Args: args}}},
			{Content: "Moved it."},
		}}
	}
	allow := []string{"write:" + work}

	// Headless (a job): the run parks and the approval waits.
	e := newApp(t, script(), noRules, "")
	res, err := e.app.Run(context.Background(), RunRequest{Skills: []string{"files"}, Allow: allow, Headless: true, Messages: user("move the episode")})
	if err != nil || res.Stop != agent.StopWaiting || res.WaitingFor == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("the file moved before anyone approved")
	}
	pending, _ := e.app.Approvals.List(context.Background(), false, 0)
	if len(pending) != 1 || pending[0].ID != res.WaitingFor || !strings.Contains(pending[0].Summary, "move_many") {
		t.Fatalf("pending = %+v", pending)
	}
	run, _ := e.app.DB.GetRun(context.Background(), res.RunID)
	if run.Status != "waiting" {
		t.Errorf("status = %s", run.Status)
	}

	// Interactive: the person at the terminal says yes.
	e = newApp(t, script(), noRules, "")
	var asked approvals.Item
	res, err = e.app.Run(context.Background(), RunRequest{Skills: []string{"files"}, Allow: allow, Messages: user("move the episode"),
		Prompt: func(_ context.Context, it approvals.Item) (bool, error) { asked = it; return true, nil }})
	if err != nil || res.Answer != "Moved it." {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "episode" {
		t.Fatalf("moved file: %q", got)
	}
	if asked.ID == "" {
		t.Error("nobody was asked")
	}
	entries, _ := e.app.DB.UndoEntries(context.Background(), res.RunID)
	var ops []string
	for _, en := range entries {
		ops = append(ops, en.Op)
	}
	if strings.Join(ops, ",") != "mkdir,move" {
		t.Errorf("undo log = %v", ops)
	}
	run, _ = e.app.DB.GetRun(context.Background(), res.RunID)
	if run.Outcome != "success" || run.OutcomeSource != "check" {
		t.Errorf("a run whose file changes are all present is a checked success: %+v", run)
	}

	// Declined: nothing moves and the model is told.
	os.Rename(dst, src)
	e = newApp(t, script(), noRules, "")
	res, _ = e.app.Run(context.Background(), RunRequest{Skills: []string{"files"}, Allow: allow, Messages: user("move the episode"),
		Prompt: func(context.Context, approvals.Item) (bool, error) { return false, nil }})
	if _, err := os.Stat(src); err != nil || res == nil {
		t.Fatalf("declined move happened anyway: %v", err)
	}
	last := e.fake.Requests[1].Messages
	if !strings.Contains(last[len(last)-1].Content, "declined") {
		t.Errorf("the model must learn the user said no: %q", last[len(last)-1].Content)
	}

	// Outside the allowed folder: refused by the broker, whatever the model asks.
	other := filepath.Join(home, "elsewhere", "x.mkv")
	bad, _ := json.Marshal(map[string]any{"plan": []map[string]string{{"from": src, "to": other}}, "dry_run": true})
	e = newApp(t, &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{{Name: "move_many", Args: bad}}}, {Content: "ok"}}}, noRules, "")
	e.app.Run(context.Background(), RunRequest{Skills: []string{"files"}, Allow: allow, Messages: user("move it")})
	tr := e.fake.Requests[1].Messages
	if !strings.Contains(tr[len(tr)-1].Content, "permission denied") {
		t.Errorf("dry run outside scope: %q", tr[len(tr)-1].Content)
	}
}

func TestDelegationReusesTheSlot(t *testing.T) {
	fake := &backendtest.Fake{}
	var subRuns atomic.Int32
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		if len(r.Tools) == 0 { // the delegated sub-task: a plain model call
			subRuns.Add(1)
			return backends.Response{Content: "sub-task result"}, nil
		}
		if r.Messages[len(r.Messages)-1].Role == backends.RoleTool {
			return backends.Response{Content: "The helper said: " + r.Messages[len(r.Messages)-1].Content}, nil
		}
		return backends.Response{ToolCalls: []backends.ToolCall{{Name: "delegate", Args: json.RawMessage(`{"task":"summarise this text: abc","task_type":"summarise"}`)}}}, nil
	}
	e := newApp(t, fake, noRules, "")
	done := make(chan *RunResult, 1)
	go func() {
		res, _ := e.app.Run(context.Background(), RunRequest{Skills: []string{"orchestrator"}, Messages: user("hand the summary off")})
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil || res.Answer != "The helper said: sub-task result" || subRuns.Load() != 1 {
			t.Fatalf("%+v subs=%d", res, subRuns.Load())
		}
		var parent string
		e.app.DB.QueryRow(`SELECT parent_id FROM runs WHERE source = 'delegate'`).Scan(&parent)
		if parent != res.RunID {
			t.Errorf("sub-run parent = %q, want %q", parent, res.RunID)
		}
		if run, _ := e.app.DB.LatestRun(context.Background(), ""); run.ID != res.RunID {
			t.Errorf("sub-runs must not show up as top-level runs: %+v", run)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: a delegated sub-run waited for its parent's generation slot")
	}
}

func TestMemoryRatingAndReask(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) {
		return backends.Response{Content: "Noted."}, nil
	}}
	e := newApp(t, fake, nil, "")
	ctx := context.Background()
	if _, err := e.app.Run(ctx, RunRequest{NoSkills: true, Client: "c1", Messages: user("Remember that I use Neovim for editing")}); err != nil {
		t.Fatal(err)
	}
	mem, _ := e.app.Memory.List(ctx)
	if len(mem) != 1 || mem[0].Source != "explicit" || !strings.Contains(mem[0].Value, "Neovim") {
		t.Fatalf("memory = %+v", mem)
	}
	// Used in the next conversation when relevant.
	e.app.Cfg.Memory.AutoExtract = false
	res, _ := e.app.Run(ctx, RunRequest{Client: "c1", Skills: nil, Messages: user("Which editor should I open this file with?")})
	if sys := fake.Requests[len(fake.Requests)-1].Messages[0].Content; !strings.Contains(sys, "What you know about the user") || !strings.Contains(sys, "Neovim") {
		t.Errorf("memory not injected:\n%s", sys)
	}
	// Plain passthrough gets no memory.
	e.app.Run(ctx, RunRequest{NoSkills: true, Messages: user("Which editor should I open this file with?")})
	if sys := fake.Requests[len(fake.Requests)-1].Messages[0].Content; strings.Contains(sys, "Neovim") {
		t.Error("memory leaked into a no-skills call")
	}
	// Explicit rating wins over everything.
	run, err := e.app.Rate(ctx, "last", "good", "")
	if err != nil || run.Outcome != "success" || run.OutcomeSource != "explicit" || run.OutcomeConf != 1 {
		t.Fatalf("%+v %v", run, err)
	}
	if _, err := e.app.Rate(ctx, res.RunID, "meh", ""); err == nil {
		t.Error("bad rating accepted")
	}
	// Asking the same thing again within ten minutes marks the first answer as probably wrong.
	first, _ := e.app.Run(ctx, RunRequest{NoSkills: true, Client: "c2", Messages: user("how do I rename many files at once")})
	e.app.Run(ctx, RunRequest{NoSkills: true, Client: "c2", Messages: user("how do I rename many files at once")})
	r1, _ := e.app.DB.GetRun(ctx, first.RunID)
	if r1.Outcome != "fail" || r1.OutcomeSource != "implicit" || r1.OutcomeConf >= 0.7 {
		t.Errorf("re-ask: %+v", r1)
	}
	// ...but never over a rating the user gave.
	rated, _ := e.app.Run(ctx, RunRequest{NoSkills: true, Client: "c3", Messages: user("explain what a symlink is")})
	e.app.Rate(ctx, rated.RunID, "good", "")
	e.app.Run(ctx, RunRequest{NoSkills: true, Client: "c3", Messages: user("explain what a symlink is")})
	if r, _ := e.app.DB.GetRun(ctx, rated.RunID); r.OutcomeSource != "explicit" || r.Outcome != "success" {
		t.Errorf("an explicit rating was overwritten: %+v", r)
	}
}

func TestNoLearnLeavesNoTraceOrMemory(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: "ok"}, nil }}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	res, err := e.app.Run(ctx, RunRequest{NoLearn: true, Messages: user("Remember that my dog is called Rex")})
	if err != nil || res.TracePath != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if mem, _ := e.app.Memory.List(ctx); len(mem) != 0 {
		t.Errorf("--no-learn stored memory: %+v", mem)
	}
	matches, _ := filepath.Glob(filepath.Join(e.app.Dirs.Traces(), "*", "*", "*", "*.jsonl"))
	if len(matches) != 0 {
		t.Errorf("trace files: %v", matches)
	}
	run, _ := e.app.DB.GetRun(ctx, res.RunID)
	if run.Outcome != "" || !run.NoLearn {
		t.Errorf("--no-learn run recorded signals: %+v", run)
	}
	if _, err := e.app.Rate(ctx, res.RunID, "good", ""); err == nil {
		t.Error("rated a --no-learn run")
	}
}

func TestBusyAndCancel(t *testing.T) {
	release := make(chan struct{})
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) {
		<-release
		return backends.Response{Content: "late"}, nil
	}}
	e := newApp(t, fake, noRules, "")
	e.app.slots = make(chan struct{}, 1)
	first := make(chan error, 1)
	go func() {
		_, err := e.app.Run(context.Background(), RunRequest{NoSkills: true, Messages: user("slow one")})
		first <- err
	}()
	for fake.Calls() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	_, err := e.app.Run(context.Background(), RunRequest{NoSkills: true, QueueWait: 100 * time.Millisecond, Messages: user("second")})
	if !errors.Is(err, ErrBusy) {
		t.Errorf("second request while busy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := e.app.Run(ctx, RunRequest{NoSkills: true, Messages: user("third")}); waiting <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled while queued: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("first: %v", err)
	}
}

func TestProjectSkillsNeedApproval(t *testing.T) {
	project := t.TempDir()
	dir := filepath.Join(project, ".noskill", "skills", "repo-tool")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: repo-tool\ndescription: Does the repository's special build thing when asked to build the project.\nnoskill:\n  permissions: { exec: true }\n---\nRun the build.\n"), 0o644)
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: "ok"}, nil }}
	e := newApp(t, fake, noRules, project)
	ctx := context.Background()
	_, err := e.app.Run(ctx, RunRequest{Skills: []string{"repo-tool"}, Messages: user("build the project")})
	if err == nil || !strings.Contains(err.Error(), "skills enable repo-tool") {
		t.Fatalf("an unapproved project skill ran: %v", err)
	}
	res, _ := e.app.Run(ctx, RunRequest{Messages: user("build the project special thing")})
	for _, s := range res.Skills {
		if s == "repo-tool" {
			t.Error("an unapproved project skill was selected automatically")
		}
	}
	if err := e.app.DB.SetSkillEnabled(ctx, "repo-tool", true); err != nil {
		t.Fatal(err)
	}
	if res, err := e.app.Run(ctx, RunRequest{Skills: []string{"repo-tool"}, Messages: user("build the project")}); err != nil || len(res.Skills) != 1 {
		t.Errorf("after approval: %+v %v", res, err)
	}
	// Changing the skill's files withdraws the approval.
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: repo-tool\ndescription: Does the repository's special build thing when asked to build the project.\nnoskill:\n  permissions: { exec: true, network: ['*'] }\n---\nRun the build.\n"), 0o644)
	e2 := newApp(t, fake, nil, project)
	e2.app.DB.Close()
	e2.app.DB = e.app.DB // same database
	_ = e2
	if err := e.app.loadSkills(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.Run(ctx, RunRequest{Skills: []string{"repo-tool"}, Messages: user("build the project")}); err == nil {
		t.Error("a project skill that changed after approval still ran")
	}
}

func TestUndoRevertsAFilingRunWithoutDeleting(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "a.mkv")
	other := filepath.Join(work, "keep.txt")
	os.WriteFile(src, []byte("episode"), 0o644)
	os.WriteFile(other, []byte("mine"), 0o644)
	dst := filepath.Join(work, "Show", "Show - 01.mkv")
	args, _ := json.Marshal(map[string]any{"plan": []map[string]string{{"from": src, "to": dst}}, "dry_run": false})
	fake := &backendtest.Fake{Script: []backends.Response{{ToolCalls: []backends.ToolCall{{Name: "move_many", Args: args}}}, {Content: "Moved."}}}
	e := newApp(t, fake, noRules, "")
	e.app.Trash = fakeTrash{dir: t.TempDir()}
	ctx := context.Background()
	res, err := e.app.Run(ctx, RunRequest{Skills: []string{"files"}, Allow: []string{"write:" + work}, Messages: user("move it"),
		Prompt: func(context.Context, approvals.Item) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
	// Dry run: lists, changes nothing.
	steps, err := e.app.Undo(ctx, res.RunID, true)
	if err != nil || len(steps) != 2 || steps[0].Op != "move" || steps[1].Op != "mkdir" {
		t.Fatalf("plan = %+v %v", steps, err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("a dry run changed files")
	}
	steps, err = e.app.Undo(ctx, res.RunID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if !s.Done {
			t.Errorf("not undone: %+v", s)
		}
	}
	if got, _ := os.ReadFile(src); string(got) != "episode" {
		t.Errorf("file not back: %q", got)
	}
	if _, err := os.Stat(filepath.Join(work, "Show")); err == nil {
		t.Error("the folder noskill created is still there (it should be in the trash)")
	}
	if got, _ := os.ReadFile(other); string(got) != "mine" {
		t.Error("an unrelated file was touched")
	}
	// Undoing twice finds nothing left to do.
	if steps, _ := e.app.Undo(ctx, res.RunID, false); len(steps) != 0 {
		t.Errorf("second undo = %+v", steps)
	}
	if _, err := e.app.Undo(ctx, "nope", false); err == nil {
		t.Error("unknown run")
	}
}

func TestUndoLeavesWhatChangedOrIsOccupied(t *testing.T) {
	work := t.TempDir()
	db := newApp(t, &backendtest.Fake{}, noRules, "").app
	db.Trash = fakeTrash{dir: t.TempDir()}
	ctx := context.Background()
	run := "01RUN"
	db.DB.InsertRun(ctx, storeRun(run))
	a, b, c := filepath.Join(work, "a.txt"), filepath.Join(work, "b.txt"), filepath.Join(work, "c.txt")
	os.WriteFile(a, []byte("edited by the user afterwards"), 0o644)
	os.WriteFile(b, []byte("squatter"), 0o644)
	os.WriteFile(c, []byte("moved here"), 0o644)
	// create a.txt (hash of the original content differs from what's there now)
	db.DB.AppendUndo(ctx, undoEntry(run, "create", "", a, "0000"))
	// move c.txt ← old.txt, but b.txt sits where the source was
	db.DB.AppendUndo(ctx, undoEntry(run, "move", b, c, ""))
	steps, _ := db.Undo(ctx, run, false)
	if len(steps) != 2 || steps[0].Done || steps[1].Done || !strings.Contains(steps[0].Note, "not replacing") || !strings.Contains(steps[1].Note, "changed since") {
		t.Fatalf("%+v", steps)
	}
	if got, _ := os.ReadFile(b); string(got) != "squatter" {
		t.Error("an occupied place was overwritten")
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("a changed file was removed")
	}
}

type fakeTrash struct{ dir string }

func (f fakeTrash) Trash(p string) (string, error) {
	to := filepath.Join(f.dir, filepath.Base(p))
	return to, os.Rename(p, to)
}

// SPEC §6 criterion 6: a skill reading outside its manifest is blocked and the attempt is traced.
func TestOutOfManifestReadIsBlockedAndTraced(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, "elsewhere", "secret.txt")
	os.MkdirAll(filepath.Dir(secret), 0o755)
	os.WriteFile(secret, []byte("hunter2"), 0o644)
	args, _ := json.Marshal(map[string]string{"path": secret})
	fake := &backendtest.Fake{Script: []backends.Response{
		{ToolCalls: []backends.ToolCall{{Name: "read_text", Args: args}}},
		{Content: "I can't read that file."},
	}}
	e := newApp(t, fake, noRules, "")
	res, err := e.app.Run(context.Background(), RunRequest{Skills: []string{"files"}, Allow: []string{"read:" + filepath.Join(home, "work")},
		Messages: user("show me " + secret)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range fake.Requests {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "hunter2") {
				t.Fatal("the file's content reached the model")
			}
		}
	}
	last := fake.Requests[len(fake.Requests)-1].Messages
	if obs := last[len(last)-1].Content; !strings.Contains(strings.ToLower(obs), "not allowed") && !strings.Contains(strings.ToLower(obs), "denied") {
		t.Errorf("the model should be told the read was blocked: %q", obs)
	}
	run, _ := e.app.DB.GetRun(context.Background(), res.RunID)
	b, err := os.ReadFile(run.TracePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"err":"permission denied: read `) || !strings.Contains(string(b), "outside this skill's read scopes") {
		t.Errorf("trace must record the blocked attempt:\n%s", b)
	}
}

type fakeJudge struct {
	answered, score float64
	calls           int
}

func (f *fakeJudge) Decide(_ context.Context, state map[string]any, qs map[string]decide.Question) (decide.Answers, error) {
	f.calls++
	probs := map[string]float64{"Good": 0.7, "Excellent": 0.2, "Acceptable": 0.05, "Poor": 0.05}
	return decide.Answers{Model: "fake-judge", Answers: map[string]decide.Answer{
		"answered": {Type: decide.Noul, Noul: f.answered},
		"quality":  {Type: decide.Score, Score: f.score, Probabilities: probs},
	}}, nil
}

// LEARNING.md §1 tier 4: the decision judge sets the outcome after the reply, unless a check or
// the user already did.
func TestDecisionJudgeSetsOutcome(t *testing.T) {
	fake := &backendtest.Fake{Script: []backends.Response{{Content: "Paris."}}}
	e := newApp(t, fake, noRules, "")
	j := &fakeJudge{answered: 0.9, score: 3}
	e.app.SetJudge(j)
	res, err := e.app.Run(context.Background(), RunRequest{Messages: user("Capital of France?")})
	if err != nil {
		t.Fatal(err)
	}
	e.app.WaitBackground()
	run, _ := e.app.DB.GetRun(context.Background(), res.RunID)
	if j.calls != 1 || run.Outcome != "success" || run.OutcomeSource != "decision" || run.OutcomeConf < 0.9 {
		t.Fatalf("run = %+v after %d judge calls", run, j.calls)
	}
	b, _ := os.ReadFile(run.TracePath)
	if !strings.Contains(string(b), `"kind":"outcome","tool":"decision"`) {
		t.Errorf("trace lacks the judge's outcome:\n%s", b)
	}
	// An explicit rating isn't replaced by a later judgement.
	e.app.Rate(context.Background(), res.RunID, "bad", "")
	e.app.judge(context.Background(), j, res.RunID, "Capital of France?", "Paris.", "", nil)
	if run, _ = e.app.DB.GetRun(context.Background(), res.RunID); run.Outcome != "fail" || run.OutcomeSource != "explicit" {
		t.Errorf("explicit rating overwritten: %+v", run)
	}
	// A refusal is judged a failure.
	if o, _ := judgeVerdict(decide.Answers{Answers: map[string]decide.Answer{"answered": {Noul: 0.1}, "quality": {Score: 3}}}); o != "fail" {
		t.Errorf("verdict %s", o)
	}
}

func TestMinerQueuesARepeatedPattern(t *testing.T) {
	fake := &backendtest.Fake{}
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		tools := 0
		for _, m := range r.Messages {
			if m.Role == backends.RoleTool {
				tools++
			}
		}
		if tools < 2 {
			q := fmt.Sprintf(`{"query":"ollama release %d"}`, tools)
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: "search", Args: json.RawMessage(q)}}}, nil
		}
		return backends.Response{Content: "Ollama 0.40 [1]."}, nil
	}
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) {
			return `[{"title":"r","url":"https://example.org"}]`, nil
		}}
	ctx := context.Background()
	var last string
	for i := 0; i < 3; i++ {
		res, err := e.app.Run(ctx, RunRequest{Messages: user("look up the latest ollama release please")})
		if err != nil {
			t.Fatal(err)
		}
		e.app.Rate(ctx, res.RunID, "good", "")
		last = res.RunID
	}
	run, _ := e.app.DB.GetRun(ctx, last)
	if run.ToolCalls != 2 || run.ToolPattern != "search>search" {
		t.Fatalf("run = %+v", run)
	}
	// Rating the third run good already queued it: the pattern was seen 3 times.
	items, _ := e.app.DB.ListLearning(ctx, 10)
	if len(items) != 1 || items[0].Reason != "repeated" || items[0].RunIDs[0] != last || items[0].Pattern != "search>search" {
		t.Fatalf("queue = %+v", items)
	}
	// The miner doesn't queue the same pattern again.
	e.app.DB.Exec(`UPDATE runs SET ended_at = ended_at - 600000, started_at = started_at - 600000`)
	if n, err := e.app.MineRuns(ctx); err != nil || n != 0 {
		t.Fatalf("mined %d %v", n, err)
	}
	// Idle, AC and job rules.
	e.app.Cfg.Learning.IdleMinutes = 5
	e.app.SetPower(true)
	if why := e.app.LearningBlocked(ctx); why != "" {
		t.Errorf("blocked after 10 idle minutes: %s", why)
	}
	e.app.SetPower(false)
	if why := e.app.LearningBlocked(ctx); why != "on battery" {
		t.Errorf("battery: %q", why)
	}
	e.app.Cfg.Learning.IdleMinutes = 30
	if why := e.app.LearningBlocked(ctx); !strings.Contains(why, "was used") {
		t.Errorf("recent use: %q", why)
	}
	// `noskill learn` queues even what the rules wouldn't, but not a run without tools.
	if _, err := e.app.Learn(ctx, last); err != nil {
		t.Errorf("learn: %v", err)
	}
	plain := &backendtest.Fake{Script: []backends.Response{{Content: "Paris."}}}
	e2 := newApp(t, plain, noRules, "")
	res, _ := e2.app.Run(ctx, RunRequest{Messages: user("Capital of France?")})
	if _, err := e2.app.Learn(ctx, res.RunID); err == nil || !strings.Contains(err.Error(), "no tools") {
		t.Errorf("learn without tools: %v", err)
	}
}

// searchAgent answers like a small model using web-search twice, and plays the skill author when
// a learning pass asks for JSON.
func searchAgent(author map[string]string) func(r backends.ChatRequest) (backends.Response, error) {
	return func(r backends.ChatRequest) (backends.Response, error) {
		prompt := r.Messages[len(r.Messages)-1].Content
		for marker, reply := range author {
			if strings.Contains(prompt, marker) {
				return backends.Response{Content: reply}, nil
			}
		}
		if r.Format != nil {
			return backends.Response{Content: "{}"}, nil // decisions: no answer, so defaults apply
		}
		tools := 0
		for _, m := range r.Messages {
			if m.Role == backends.RoleTool {
				tools++
			}
		}
		if tools < 2 {
			q := fmt.Sprintf(`{"query":"ollama release %d"}`, tools)
			return backends.Response{ToolCalls: []backends.ToolCall{{Name: "search", Args: json.RawMessage(q)}}}, nil
		}
		return backends.Response{Content: "Ollama 0.40 [1]."}, nil
	}
}

func TestLearnDraftsVerifiesAndPublishes(t *testing.T) {
	fake := &backendtest.Fake{}
	fake.Reply = searchAgent(map[string]string{
		`{"steps"`:    `{"steps": ["Call ` + "`search`" + ` for the release news.", "Call ` + "`search`" + ` again with a narrower query.", "Answer with the version and source."]}`,
		`{"name"`:     `{"name": "release-lookup", "description": "Look up the latest release of a software project with web search. Use when the user asks for the latest ollama release or version.", "triggers": ["latest release"]}`,
		`{"variants"`: `{"variants": [{"request": "look up the latest llama.cpp release please", "values": {}}]}`,
	})
	e := newApp(t, fake, noRules, "")
	searches := 0
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) {
			searches++
			return `[{"title":"r","url":"https://example.org"}]`, nil
		}}
	ctx := context.Background()
	res, err := e.app.Run(ctx, RunRequest{Messages: user("look up the latest ollama release please")})
	if err != nil {
		t.Fatal(err)
	}
	it, err := e.app.Learn(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	it, err = e.app.LearnNow(ctx, it.ID)
	if err != nil || it.Status != "done" || !strings.HasPrefix(it.Result, "learned release-lookup 1.0.0") {
		t.Fatalf("item %+v err %v", it, err)
	}
	if searches != 2 {
		t.Errorf("verification ran the real search tool: %d calls", searches)
	}
	sk, ok := e.app.Skills.Get("release-lookup")
	if !ok || sk.Source != skills.SourceLearned || sk.Status() != "candidate" || sk.Meta.DependsOn[0] != "web-search" {
		t.Fatalf("skill %+v", sk)
	}
	if _, err := os.Stat(filepath.Join(e.home, "learned", "release-lookup", "tests", "fixtures", "replay.json")); err != nil {
		t.Error(err)
	}
	log, _ := os.ReadFile(filepath.Join(e.home, "learned", "release-lookup", "CHANGELOG.md"))
	if !strings.Contains(string(log), "Tests: 2 of 2 passed") {
		t.Errorf("changelog:\n%s", log)
	}
	// Learning's own runs don't count as use.
	var learningRuns int
	e.app.DB.QueryRow(`SELECT count(*) FROM runs WHERE source = 'learning'`).Scan(&learningRuns)
	userRun, _ := e.app.DB.GetRun(ctx, res.RunID)
	if last, _, _ := e.app.DB.LastActivity(ctx); learningRuns != 2 || !last.Equal(userRun.EndedAt) {
		t.Errorf("learning runs %d; last activity %v, user run ended %v", learningRuns, last, userRun.EndedAt)
	}
	// A candidate isn't used automatically.
	usable, _ := e.app.UsableSkills(ctx)
	if slices.ContainsFunc(usable, func(s *skills.Skill) bool { return s.Name == "release-lookup" }) {
		t.Error("a candidate is usable before its trial")
	}
}

func TestDraftThatFailsItsTestsIsRejected(t *testing.T) {
	fake := &backendtest.Fake{}
	author := map[string]string{
		`{"steps"`: `{"steps": ["Call ` + "`search`" + ` for it.", "Answer."]}`,
		`{"name"`:  `{"name": "release-lookup", "description": "Look up the latest release of a software project. Use when the user asks for a latest release.", "triggers": []}`,
	}
	fake.Reply = searchAgent(author)
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) { return `[]`, nil }}
	ctx := context.Background()
	res, _ := e.app.Run(ctx, RunRequest{Messages: user("look up the latest ollama release please")})
	// From now on the "model" answers without tools: replays can't pass.
	lazy := searchAgent(author)
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		if len(r.Tools) > 0 {
			return backends.Response{Content: "I think it's 0.40."}, nil
		}
		return lazy(r)
	}
	it, _ := e.app.Learn(ctx, res.RunID)
	it, err := e.app.LearnNow(ctx, it.ID)
	if err != nil || it.Status != "done" || !strings.Contains(it.Result, "rejected release-lookup (its tests failed)") {
		t.Fatalf("%+v %v", it, err)
	}
	ents, _ := os.ReadDir(filepath.Join(e.home, "learned", ".rejected"))
	if len(ents) != 1 {
		t.Fatalf("rejected: %v", ents)
	}
	why, _ := os.ReadFile(filepath.Join(e.home, "learned", ".rejected", ents[0].Name(), "REJECTED.txt"))
	if !strings.Contains(string(why), "after 2 repair(s)") || !strings.Contains(string(why), "expected the tools search") {
		t.Errorf("reason:\n%s", why)
	}
	if _, ok := e.app.Skills.Get("release-lookup"); ok {
		t.Error("a rejected draft was loaded")
	}
}

// writeCandidate puts a learned skill on trial straight into the library.
func writeCandidate(t *testing.T, e *env) {
	t.Helper()
	dir := filepath.Join(e.home, "learned", "release-lookup")
	os.MkdirAll(dir, 0o700)
	md := "---\nname: release-lookup\ndescription: Look up the latest release of a software project with web search.\n" +
		"noskill:\n  version: 1.0.0\n  origin: learned\n  status: candidate\n  triggers: [latest release]\n  depends_on: [web-search]\n---\n" +
		"## Steps\n1. Call `search` twice.\n2. Answer with the version.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sk, ok := e.app.Skills.Get("release-lookup"); !ok || sk.Status() != "candidate" {
		t.Fatalf("candidate not loaded: %+v", sk)
	}
}

func TestCandidateAsksThenIsPromoted(t *testing.T) {
	fake := &backendtest.Fake{Reply: searchAgent(nil)}
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) { return `[]`, nil }}
	writeCandidate(t, e)
	ctx := context.Background()
	ask := "look up the latest release of ollama please"
	used := func(res *RunResult) bool { return slices.Contains(res.Skills, "release-lookup") }

	// At the terminal: asked right away; "no" runs the normal way and counts a skip.
	asked := 0
	res, err := e.app.Run(ctx, RunRequest{Messages: user(ask), Prompt: func(context.Context, approvals.Item) (bool, error) { asked++; return false, nil }})
	if err != nil || asked != 1 || used(res) {
		t.Fatalf("asked %d, used %v, err %v", asked, res.Skills, err)
	}
	// Headless (a job, the proxy): the question is queued and the run goes on without it.
	res, _ = e.app.Run(ctx, RunRequest{Messages: user(ask), Headless: true})
	if used(res) {
		t.Fatal("a candidate was used before anyone said yes")
	}
	items, _ := e.app.Approvals.List(ctx, false, 10)
	if len(items) != 1 || items[0].Kind != "learned_skill" {
		t.Fatalf("approvals %+v", items)
	}
	res, _ = e.app.Run(ctx, RunRequest{Messages: user(ask), Headless: true})
	if items, _ = e.app.Approvals.List(ctx, false, 10); len(items) != 1 || used(res) {
		t.Fatalf("asked twice: %d items", len(items))
	}
	e.app.Approvals.Resolve(ctx, items[0].ID, approvals.Answer{Approve: true, Via: "cli"})

	// Approved: used from now on; promote_after (3) good runs in a row make it active.
	for i := range 3 {
		res, err = e.app.Run(ctx, RunRequest{Messages: user(ask), Headless: true})
		if err != nil || !used(res) {
			t.Fatalf("run %d: skills %v err %v", i, res.Skills, err)
		}
		if sk, _ := e.app.Skills.Get("release-lookup"); sk.Status() != "candidate" {
			t.Fatalf("promoted after %d runs", i)
		}
		if _, err := e.app.Rate(ctx, res.RunID, "good", ""); err != nil {
			t.Fatal(err)
		}
	}
	sk, _ := e.app.Skills.Get("release-lookup")
	if sk.Status() != "active" {
		t.Fatalf("status %s after 3 good runs", sk.Status())
	}
	log, _ := os.ReadFile(filepath.Join(e.home, "learned", "release-lookup", "CHANGELOG.md"))
	if !strings.Contains(string(log), "Promoted to active after 3 good runs.") {
		t.Errorf("changelog:\n%s", log)
	}
	var uses, succ, skips int
	e.app.DB.QueryRow(`SELECT uses, successes, skips FROM skill_stats WHERE name = 'release-lookup' AND version = '1.0.0'`).Scan(&uses, &succ, &skips)
	if uses != 3 || succ != 3 || skips != 1 {
		t.Errorf("stats uses %d successes %d skips %d", uses, succ, skips)
	}
}

func TestCandidateArchivedAfterTwoSkipsOrFailures(t *testing.T) {
	fake := &backendtest.Fake{Reply: searchAgent(nil)}
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) { return `[]`, nil }}
	writeCandidate(t, e)
	ctx := context.Background()
	no := func(context.Context, approvals.Item) (bool, error) { return false, nil }
	for range 2 {
		e.app.Run(ctx, RunRequest{Messages: user("look up the latest release of ollama"), Prompt: no})
	}
	if sk, _ := e.app.Skills.Get("release-lookup"); sk.Status() != "archived" {
		t.Fatalf("status %s after two skips", sk.Status())
	}
	if _, err := os.Stat(filepath.Join(e.home, "learned", "release-lookup", "SKILL.md")); err != nil {
		t.Fatal("archiving removed the skill")
	}

	// A failure asks again; a second failure archives.
	writeCandidate(t, e)
	e.app.DB.Exec(`DELETE FROM skill_stats`)
	yes := func(context.Context, approvals.Item) (bool, error) { return true, nil }
	asked := 0
	counting := func(c context.Context, it approvals.Item) (bool, error) { asked++; return yes(c, it) }
	for i := range 2 {
		res, _ := e.app.Run(ctx, RunRequest{Messages: user("look up the latest release of ollama"), Prompt: counting})
		if !slices.Contains(res.Skills, "release-lookup") {
			t.Fatalf("run %d didn't use it: %v", i, res.Skills)
		}
		e.app.Rate(ctx, res.RunID, "bad", "")
	}
	if sk, _ := e.app.Skills.Get("release-lookup"); asked != 2 || sk.Status() != "archived" {
		t.Fatalf("asked %d; status %s", asked, sk.Status())
	}
}

func TestFailingLearnedSkillIsRefinedAndAdopted(t *testing.T) {
	fake := &backendtest.Fake{Reply: searchAgent(map[string]string{
		"and the runs below failed": `{"steps": ["Call ` + "`search`" + ` for the project's releases.", "Call ` + "`search`" + ` again for the newest tag.", "Answer with the version and its date."]}`,
	})}
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) { return `[]`, nil }}
	writeCandidate(t, e)
	dir := filepath.Join(e.home, "learned", "release-lookup")
	md, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(strings.Replace(string(md), "status: candidate", "status: active", 1)), 0o600)
	os.MkdirAll(filepath.Join(dir, "tests", "fixtures"), 0o700)
	os.WriteFile(filepath.Join(dir, "tests", "cases.yaml"), []byte("- name: replay\n  input: look up the latest release of ollama\n  network: recorded\n"+
		"  fixtures: fixtures/replay.json\n  expect:\n    tools: [search, search]\n    answered: true\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "tests", "fixtures", "replay.json"), []byte(`[{"tool":"search","result":"[]"}]`), 0o600)
	ctx := context.Background()
	e.app.Reload(ctx)
	ask := "look up the latest release of ollama"
	version := func(res *RunResult) string {
		vs, _ := e.app.DB.RunSkills(ctx, res.RunID)
		return vs["release-lookup"]
	}

	// Two bad runs in a row queue a refinement.
	for range 2 {
		res, err := e.app.Run(ctx, RunRequest{Messages: user(ask), Headless: true})
		if err != nil || version(res) != "1.0.0" {
			t.Fatalf("version %q err %v", version(res), err)
		}
		e.app.Rate(ctx, res.RunID, "bad", "it gave last year's version")
	}
	items, _ := e.app.DB.ListLearning(ctx, 10)
	var it store.LearnItem
	for _, x := range items {
		if x.Kind == "refine" {
			it = x
		}
	}
	if it.Skill != "release-lookup" || len(it.RunIDs) != 2 {
		t.Fatalf("queue %+v", items)
	}
	it, err := e.app.LearnNow(ctx, it.ID)
	if err != nil || !strings.HasPrefix(it.Result, "refined release-lookup: 1.1.0 is on trial") {
		t.Fatalf("%+v %v", it, err)
	}
	next, _ := os.ReadFile(filepath.Join(dir, ".next", "SKILL.md"))
	cases, _ := os.ReadFile(filepath.Join(dir, ".next", "tests", "cases.yaml"))
	if !strings.Contains(string(next), "again for the newest tag") || strings.Count(string(cases), "name: failed-") != 2 {
		t.Fatalf("next:\n%s\ncases:\n%s", next, cases)
	}

	// The refinement runs in place of 1.0.0, without asking (same access), and is adopted after 3 good runs.
	for i := range 3 {
		res, err := e.app.Run(ctx, RunRequest{Messages: user(ask), Headless: true})
		if err != nil || version(res) != "1.1.0" {
			t.Fatalf("run %d: version %q err %v", i, version(res), err)
		}
		e.app.Rate(ctx, res.RunID, "good", "")
	}
	sk, _ := e.app.Skills.Get("release-lookup")
	if sk.Meta.Version != "1.1.0" || sk.Status() != "active" {
		t.Fatalf("after trial: %s %s", sk.Meta.Version, sk.Status())
	}
	if _, err := os.Stat(filepath.Join(dir, ".versions", "1.0.0", "SKILL.md")); err != nil {
		t.Errorf("1.0.0 wasn't kept: %v", err)
	}
	if items, _ := e.app.Approvals.List(ctx, false, 10); len(items) != 0 {
		t.Errorf("asked about a refinement with the same access: %+v", items)
	}
}

// writeLearned puts a learned skill with a replay test into the library (no reload).
func writeLearned(t *testing.T, e *env, name, status, trigger string) {
	t.Helper()
	dir := filepath.Join(e.home, "learned", name)
	os.MkdirAll(filepath.Join(dir, "tests", "fixtures"), 0o700)
	md := "---\nname: " + name + "\ndescription: Look up the latest release of a software project with web search.\n" +
		"noskill:\n  version: 1.0.0\n  origin: learned\n  status: " + status + "\n  triggers: [" + trigger + "]\n  depends_on: [web-search]\n---\n" +
		"## Steps\n1. Call `search` twice.\n2. Answer with the version.\n"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600)
	os.WriteFile(filepath.Join(dir, "tests", "cases.yaml"), []byte("- name: replay\n  input: look up the "+trigger+" of ollama\n  network: recorded\n"+
		"  fixtures: fixtures/replay.json\n  expect:\n    tools: [search, search]\n    answered: true\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "tests", "fixtures", "replay.json"), []byte(`[{"tool":"search","result":"[]"}]`), 0o600)
}

func TestCuratorRetiresAndMerges(t *testing.T) {
	fake := &backendtest.Fake{Reply: searchAgent(map[string]string{`"skill_a"`: `{"duplicate": true}`})}
	e := newApp(t, fake, noRules, "")
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: e.app.Registry["web.search"].Schema,
		Run: func(context.Context, *tools.Env, json.RawMessage) (string, error) { return `[]`, nil }}
	ctx := context.Background()
	writeLearned(t, e, "release-lookup", "active", "latest release")
	writeLearned(t, e, "release-check", "active", "newest version")
	writeLearned(t, e, "old-notes", "active", "old notes")
	os.WriteFile(filepath.Join(e.home, "learned", "old-notes", "tests", "fixtures", "replay.json"), []byte(`[{"tool":"read_text","result":"x"}]`), 0o600)
	if err := e.app.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	e.app.DB.Exec(`UPDATE skills SET created_at = ? WHERE name = 'old-notes'`, time.Now().Add(-40*24*time.Hour).UnixMilli())

	report, err := e.app.Curate(ctx)
	if err != nil || report != "This week: 0 skill(s) learned, 0 refined, 0 merged, 1 retired." {
		t.Fatalf("%q %v", report, err)
	}
	if sk, _ := e.app.Skills.Get("old-notes"); sk.Status() != "stale" {
		t.Fatalf("old-notes is %s", sk.Status())
	}
	if e.app.CurateDueForTest(ctx) {
		t.Error("the curator would run again right away")
	}
	items, _ := e.app.DB.ListLearning(ctx, 10)
	if len(items) != 1 || items[0].Kind != "merge" {
		t.Fatalf("queue %+v", items)
	}
	keep, dup := items[0].Skill, items[0].Pattern
	it, err := e.app.LearnNow(ctx, items[0].ID)
	if err != nil || it.Result != "merged "+dup+" into "+keep+": 1.1.0 is on trial" {
		t.Fatalf("%+v %v", it, err)
	}
	if sk, _ := e.app.Skills.Get(dup); sk.Status() != "archived" {
		t.Fatalf("%s is %s", dup, sk.Status())
	}
	next, _ := os.ReadFile(filepath.Join(e.home, "learned", keep, ".next", "SKILL.md"))
	cases, _ := os.ReadFile(filepath.Join(e.home, "learned", keep, ".next", "tests", "cases.yaml"))
	if !strings.Contains(string(next), "triggers: [") || !strings.Contains(string(next), "newest version") || !strings.Contains(string(next), "latest release") ||
		!strings.Contains(string(cases), "name: "+dup+"-replay") || !strings.Contains(string(cases), "required: true") {
		t.Fatalf("next:\n%s\ncases:\n%s", next, cases)
	}
	// `skills enable` brings a retired skill back.
	if err := e.app.RestoreLearned(ctx, "old-notes"); err != nil {
		t.Fatal(err)
	}
	if sk, _ := e.app.Skills.Get("old-notes"); sk.Status() != "active" {
		t.Fatalf("old-notes is %s", sk.Status())
	}
}

func TestEscalationRetriesOnTheNextModel(t *testing.T) {
	var first string
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		if first == "" {
			first = r.Model
		}
		if r.Model == first {
			return backends.Response{Content: ""}, nil
		}
		return backends.Response{Content: "42."}, nil
	}}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	res, err := e.app.Run(ctx, RunRequest{Source: "cli", NoSkills: true, Messages: user("what is six times seven?")})
	if err != nil || res.Answer != "42." || res.Model == first || len(res.EscalatedFrom) != 1 {
		t.Fatalf("%+v %v (first %s)", res, err, first)
	}
	failed, _ := e.app.DB.GetRun(ctx, res.EscalatedFrom[0])
	if failed.DriverModel != first {
		t.Errorf("first run = %+v", failed)
	}
	if b, _ := os.ReadFile(failed.TracePath); !strings.Contains(string(b), "Escalating: the answer was empty") {
		t.Errorf("the failed run's trace should say why it escalated:\n%s", b)
	}

	// Pinned runs and runs that still fail on the second model escalate no further.
	first = ""
	fake.Reply = func(backends.ChatRequest) (backends.Response, error) { return backends.Response{}, nil }
	n := fake.Calls()
	res, _ = e.app.Run(ctx, RunRequest{Source: "cli", NoSkills: true, Messages: user("what is six times seven?")})
	if res == nil || fake.Calls()-n != 2 || len(res.EscalatedFrom) != 1 {
		t.Errorf("one retry only: %d calls, %+v", fake.Calls()-n, res)
	}
	n = fake.Calls()
	if res, _ = e.app.Run(ctx, RunRequest{Model: "qwen3:4b", NoSkills: true, Messages: user("hi")}); res == nil || fake.Calls()-n != 1 {
		t.Errorf("pinned runs never escalate: %d calls", fake.Calls()-n)
	}
}

func TestRatingsTrainTheBandit(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: "ok"}, nil }}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	res, err := e.app.Run(ctx, RunRequest{Source: "cli", NoSkills: true, Messages: user("say ok")})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := e.app.DB.GetRun(ctx, res.RunID)
	if run.TaskType == "" {
		t.Fatalf("unpinned runs record their task type: %+v", run)
	}
	evidence := func() store.Evidence {
		ev, _ := e.app.DB.BanditEvidence(ctx, run.TaskType, run.Bucket)
		return ev[run.DriverModel]
	}
	if _, err := e.app.Rate(ctx, res.RunID, "good", ""); err != nil {
		t.Fatal(err)
	}
	if ev := evidence(); ev.Alpha < 0.9 || ev.Beta > 0.1 {
		t.Fatalf("after good: %+v", ev)
	}
	// Changing your mind replaces the earlier rating instead of adding to it.
	e.app.Rate(ctx, res.RunID, "bad", "")
	if ev := evidence(); ev.Alpha > 0.01 || ev.Beta < 0.9 || ev.Beta > 1.01 {
		t.Fatalf("after bad: %+v", ev)
	}
	// Pinned runs teach nothing.
	res, _ = e.app.Run(ctx, RunRequest{Model: "qwen3:4b", NoSkills: true, Messages: user("say ok")})
	e.app.Rate(ctx, res.RunID, "good", "")
	if ev := evidence(); ev.Alpha > 0.01 {
		t.Errorf("pinned run counted: %+v", ev)
	}
}

func TestLLMJudgeRatesQuietRunsAndAsksWhenJudgesDisagree(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		if r.Format != nil {
			return backends.Response{Content: `{"answered":true,"quality":"Good"}`}, nil
		}
		return backends.Response{Content: "ok"}, nil
	}}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	age := func(id string) {
		old := time.Now().Add(-20 * time.Minute).UnixMilli()
		e.app.DB.Exec(`UPDATE runs SET started_at = ?, ended_at = ? WHERE id = ?`, old, old, id)
	}
	res, err := e.app.Run(ctx, RunRequest{Source: "cli", NoSkills: true, Messages: user("say ok")})
	if err != nil {
		t.Fatal(err)
	}
	// Too recent: you may still rate it yourself.
	if did, _ := e.app.LearnOnce(ctx); strings.Contains(did, "judge") {
		t.Fatalf("judged a fresh run: %s", did)
	}
	age(res.RunID)
	if did, err := e.app.LearnOnce(ctx); err != nil || did != "LLM judge: judged 1 run(s)" {
		t.Fatalf("%q %v", did, err)
	}
	run, _ := e.app.DB.GetRun(ctx, res.RunID)
	if run.Outcome != "success" || run.OutcomeSource != "llm_judge" || run.OutcomeConf != 0.6 || run.Reward == 0 {
		t.Fatalf("run = %+v", run)
	}

	// The decision judge said fail, unsure; the LLM judge says success: you're asked.
	res, _ = e.app.Run(ctx, RunRequest{Source: "cli", NoSkills: true, Messages: user("say ok again")})
	e.app.DB.SetOutcome(ctx, res.RunID, "fail", 0.55, "decision")
	age(res.RunID)
	e.app.LearnOnce(ctx)
	items, _ := e.app.Approvals.List(ctx, false, 10)
	if len(items) != 1 || items[0].Kind != "outcome_check" || items[0].RunID != res.RunID {
		t.Fatalf("approvals = %+v", items)
	}
	e.app.Approvals.Resolve(ctx, items[0].ID, approvals.Answer{Approve: true, Via: "cli"})
	e.app.LearnOnce(ctx)
	if run, _ = e.app.DB.GetRun(ctx, res.RunID); run.Outcome != "success" || run.OutcomeSource != "explicit" {
		t.Fatalf("the answer should become your rating: %+v", run)
	}
}

func TestJudgeModelPrefersAnotherFamily(t *testing.T) {
	for in, want := range map[string]string{"qwen3:4b": "qwen", "qwen2.5-coder:7b": "qwen", "hf.co/x/Llama-3.2-3B": "llama", "gemma3": "gemma"} {
		if got := family(in); got != want {
			t.Errorf("family(%s) = %s", in, got)
		}
	}
	e := newApp(t, &backendtest.Fake{}, nil, "")
	mk := func(name string, gb, params float64) Model {
		return Model{ModelInfo: backends.ModelInfo{Name: name, ParamsB: params, SizeBytes: int64(gb * 1e9), Capabilities: chatCaps}}
	}
	ms := []Model{mk("qwen3:4b", 2.5, 4), mk("qwen2.5-coder:7b", 4.7, 7), mk("llama3.2:3b", 2, 3)}
	if m := e.app.judgeModel(ms, "qwen3:4b"); m == nil || m.Name != "llama3.2:3b" {
		t.Errorf("picked %v", m)
	}
	if m := e.app.judgeModel(ms[:2], "qwen3:4b"); m == nil || m.Name != "qwen2.5-coder:7b" {
		t.Errorf("picked %v", m)
	}
}

// Swap rules 7 and 8: a batch of sub-tasks runs grouped by model, and when that pushed the driver
// out of memory, the model now loaded writes the answer.
func TestBatchDelegationAndDriverHandoff(t *testing.T) {
	var mu sync.Mutex
	var driver, finisher string
	var subs []string
	other := func(m string) string {
		if m == "qwen3:4b" {
			return "qwen2.5-coder:7b"
		}
		return "qwen3:4b"
	}
	fake := &backendtest.Fake{}
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		last := r.Messages[len(r.Messages)-1]
		switch {
		case len(r.Tools) == 0:
			subs = append(subs, last.Content+"@"+r.Model)
			return backends.Response{Content: "result of " + last.Content}, nil
		case last.Role == backends.RoleTool:
			finisher = r.Model
			return backends.Response{Content: "Combined: " + last.Content}, nil
		}
		driver = r.Model
		args, _ := json.Marshal(map[string]any{"tasks": []map[string]string{
			{"task": "A", "model": other(r.Model)}, {"task": "B", "model": r.Model}, {"task": "C", "model": other(r.Model)}}})
		return backends.Response{ToolCalls: []backends.ToolCall{{Name: "delegate", Args: args}}}, nil
	}
	e := newApp(t, fake, noRules, "")
	res, err := e.app.Run(context.Background(), RunRequest{Source: "cli", Skills: []string{"orchestrator"}, Messages: user("do A, B and C")})
	if err != nil {
		t.Fatal(err)
	}
	// Pinned sub-tasks: none stays on the loaded model (nothing is resident in the fake), so the
	// groups go in order of first appearance: the other model's A and C, then B.
	want := fmt.Sprint([]string{"A@" + other(driver), "C@" + other(driver), "B@" + driver})
	if fmt.Sprint(subs) != want {
		t.Errorf("sub-tasks ran %v, want %v", subs, want)
	}
	if !strings.Contains(res.Answer, `"task":1`) || !strings.Contains(res.Answer, "result of A") {
		t.Errorf("answer %q", res.Answer)
	}
	// The last sub-task ran on the driver's own model, so no handoff: it finishes.
	if finisher != driver || res.Model != driver {
		t.Errorf("finisher %s, result model %s, driver %s", finisher, res.Model, driver)
	}

	// One sub-task on the other model, with the driver evicted: the other model finishes.
	fake.Reply = func(r backends.ChatRequest) (backends.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		last := r.Messages[len(r.Messages)-1]
		switch {
		case len(r.Tools) == 0:
			return backends.Response{Content: "sub result"}, nil
		case last.Role == backends.RoleTool:
			finisher = r.Model
			return backends.Response{Content: "Done."}, nil
		}
		driver = r.Model
		return backends.Response{ToolCalls: []backends.ToolCall{{Name: "delegate", Args: json.RawMessage(`{"task":"A","model":"` + other(r.Model) + `"}`)}}}, nil
	}
	res, err = e.app.Run(context.Background(), RunRequest{Source: "cli", Skills: []string{"orchestrator"}, Messages: user("do A")})
	if err != nil {
		t.Fatal(err)
	}
	if finisher != other(driver) || res.Model != finisher {
		t.Fatalf("finisher %s, result model %s, driver %s", finisher, res.Model, driver)
	}
	run, _ := e.app.DB.GetRun(context.Background(), res.RunID)
	b, _ := os.ReadFile(run.TracePath)
	if run.DriverModel != finisher || !strings.Contains(string(b), "Handing off: "+finisher+" is loaded now") {
		t.Errorf("run %+v; trace:\n%s", run, b)
	}
}

func TestEvalScoresRunsAndRegressions(t *testing.T) {
	answer := map[string]string{"capital of France": "Paris.", "17 * 23": "391", "leap year": "366"}
	broken := false
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		q := r.Messages[len(r.Messages)-1].Content
		for k, v := range answer {
			if strings.Contains(q, k) {
				if broken && k == "17 * 23" {
					return backends.Response{Content: "390"}, nil
				}
				return backends.Response{Content: v}, nil
			}
		}
		return backends.Response{Content: `{"task_type":"chat"}`}, nil
	}}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	tasks, err := e.app.EvalTasks([]string{"chat-01", "chat-03", "math-01"}, nil, false)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("%v %v", tasks, err)
	}
	opts := EvalOptions{Model: "qwen3:4b", Tasks: tasks, Suite: "t"}
	first, err := e.app.Eval(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Run.Passed != 3 || first.Run.Tasks != 3 || first.Prev != nil {
		t.Fatalf("%+v", first.Run)
	}

	// A later run where one task breaks is compared with the first; one flip is noise.
	broken = true
	second, err := e.app.Eval(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	// One flipped task is noise for the types, but 33 points overall is more than [learning]
	// regression_success_drop (5), so the gate flags it.
	if second.Run.Passed != 2 || second.Prev == nil || second.Prev.ID != first.Run.ID || len(second.Regressions) != 1 || !strings.HasPrefix(second.Regressions[0].Type, "overall") {
		t.Errorf("%+v prev=%v %+v", second.Run, second.Prev, second.Regressions)
	}
	if r := second.Results; len(r) != 3 || r[2].Passed || !strings.Contains(r[2].Why, "391") {
		t.Errorf("results %+v", r)
	}

	// Runs leave no bandit evidence behind.
	var n int
	e.app.DB.QueryRowContext(ctx, `SELECT count(*) FROM bandit`).Scan(&n)
	if n != 0 {
		t.Errorf("eval runs trained the bandit (%d rows)", n)
	}

	// Bench scores become the router's quality per task type.
	broken = false
	scores, err := e.app.BenchScores(ctx, "qwen3:4b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if scores["chat"] <= scores["math"] || len(scores) == 0 {
		t.Logf("scores %v", scores)
	}
	ms, _ := e.app.Models(ctx)
	for _, p := range e.app.profiles(ctx, ms) {
		if p.Info.Name == "qwen3:4b" && p.Quality["chat"] == 0 {
			t.Errorf("profile has no benchmark quality: %v", p.Quality)
		}
	}
}

// The regression gate puts a learned skill back when a task it took part in fell after it changed,
// and leaves it alone when only tasks it couldn't touch fell.
func TestEvalGateRollsBackALearnedSkillThatRegressed(t *testing.T) {
	broken := false
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		q := r.Messages[len(r.Messages)-1].Content
		switch {
		case strings.Contains(q, "17 * 23"):
			if broken {
				return backends.Response{Content: "1"}, nil
			}
			return backends.Response{Content: "391"}, nil
		case strings.Contains(q, "144 divided by 12"):
			if broken {
				return backends.Response{Content: "1"}, nil
			}
			return backends.Response{Content: "12"}, nil
		case strings.Contains(q, "15% of 240"):
			return backends.Response{Content: "36"}, nil
		case strings.Contains(q, "latest release"):
			if broken {
				return backends.Response{Content: "no idea"}, nil
			}
			return backends.Response{Content: "v2.1"}, nil
		}
		return backends.Response{Content: `{"task_type":"math"}`}, nil
	}}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	writeLearned(t, e, "release-lookup", "active", "latest release")
	dir := filepath.Join(e.home, "learned", "release-lookup")
	copyTo := func(v string) {
		dst := filepath.Join(dir, ".versions", v)
		os.MkdirAll(dst, 0o700)
		b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		os.WriteFile(filepath.Join(dst, "SKILL.md"), b, 0o600)
	}
	copyTo("1.0.0")
	if err := e.app.loadSkills(ctx); err != nil {
		t.Fatal(err)
	}
	math, _ := e.app.EvalTasks([]string{"math-01", "math-02", "math-03"}, nil, false)
	// Agent tasks name their skills; a learned skill that matches joins them.
	lookups := []eval.Task{}
	for i := 1; i <= 3; i++ {
		lookups = append(lookups, eval.Task{ID: fmt.Sprintf("rel-%d", i), Type: "files", Skills: []string{"files"},
			Prompt: fmt.Sprintf("What is the latest release of tool %d?", i), Check: eval.Check{Contains: []string{"v2.1"}}})
	}
	opts := EvalOptions{Model: "qwen3:4b", Tasks: append(math, lookups...), Suite: "mix"}
	good, err := e.app.Eval(ctx, opts)
	if err != nil || good.Run.Passed != 6 || !strings.Contains(good.Run.SkillState, `"release-lookup":"1.0.0"`) {
		t.Fatalf("%+v %v", good.Run, err)
	}
	for _, r := range good.Results {
		if strings.HasPrefix(r.TaskID, "rel-") != slices.Contains(r.Learned, "release-lookup") {
			t.Errorf("%s used learned skills %v", r.TaskID, r.Learned)
		}
	}

	// A refinement is adopted (1.1.0), and afterwards two plain math tasks fail: the learned skill
	// can't have caused that, so nothing goes back.
	md, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(strings.Replace(string(md), "version: 1.0.0", "version: 1.1.0", 1)), 0o600)
	copyTo("1.1.0")
	e.app.loadSkills(ctx)
	opts.Tasks = math
	opts.Suite = "math"
	e.app.Eval(ctx, opts) // the math-only baseline
	broken = true
	g, err := e.app.EvalGate(ctx, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Outcome.Regressions) == 0 || len(g.RolledBack) != 0 {
		t.Fatalf("plain tasks fell: regressions %+v rolled back %v", g.Outcome.Regressions, g.RolledBack)
	}

	// The tasks that use it fall too: now it goes back to the version of the last run.
	broken = false
	opts.Tasks, opts.Suite = append(math, lookups...), "mix"
	broken = true // the last "mix" run had 1.0.0; 1.1.0 is current
	g, err = e.app.EvalGate(ctx, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Outcome.Regressions) == 0 || g.RolledBack["release-lookup"] != "1.0.0" {
		t.Fatalf("regressions %+v rolled back %v", g.Outcome.Regressions, g.RolledBack)
	}
	if sk, _ := e.app.Skills.Get("release-lookup"); sk == nil || sk.Meta.Version != "1.0.0" {
		t.Errorf("the skill isn't back at 1.0.0: %+v", sk)
	}

	// Without --rollback the gate only reports.
	broken = false
	e.app.RollbackSkill(ctx, "release-lookup", "1.1.0")
	g, err = e.app.EvalGate(ctx, opts, false)
	if err != nil || len(g.RolledBack) != 0 {
		t.Errorf("second gate: %+v %v", g, err)
	}

	// The "before" arm keeps learned skills out.
	opts.NoLearned = true
	plain, err := e.app.Eval(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plain.Results {
		if len(r.Learned) > 0 {
			t.Errorf("%s used %v with NoLearned", r.TaskID, r.Learned)
		}
	}
}

func TestWeeklyEvalIsDueOnSchedule(t *testing.T) {
	e := newApp(t, &backendtest.Fake{Reply: func(backends.ChatRequest) (backends.Response, error) { return backends.Response{Content: "x"}, nil }}, noRules, "")
	ctx := context.Background()
	if e.app.evalDue(ctx) {
		t.Fatal("due on the first look: it should start the clock instead")
	}
	week := time.Now().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	e.app.DB.SetState(ctx, evalAtKey, week, time.Now())
	if !e.app.evalDue(ctx) {
		t.Error("not due 8 days after the last run (eval_schedule is weekly)")
	}
	e.app.DB.SetState(ctx, evalTriedKey, time.Now().Format(time.RFC3339), time.Now())
	if e.app.evalDue(ctx) {
		t.Error("retried right after an attempt")
	}
}

// The exit-check harness runs end to end on a scripted model and refuses a home that already
// learned something.
func TestSimulateRunsAndNeedsAFreshHome(t *testing.T) {
	fake := &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		return backends.Response{Content: "Done."}, nil
	}}
	e := newApp(t, fake, noRules, "")
	ctx := context.Background()
	var last SimProgress
	rep, err := e.app.Simulate(ctx, SimOptions{Model: "qwen3:4b", Days: 2, PerDay: 2, Progress: func(p SimProgress) { last = p }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requests != 4 || rep.Before == nil || rep.After == nil || len(rep.Nights) != 2 || last.Phase != "done" || last.Done != last.Total {
		t.Fatalf("%+v last %+v", rep, last)
	}
	if rep.Pass || !strings.HasPrefix(rep.Summary(), "FAIL") {
		t.Errorf("a model that does nothing passed: %s", rep.Summary())
	}
	// The simulated requests are real runs with explicit ratings.
	runs, _ := e.app.DB.ListRuns(ctx, 100)
	rated := 0
	for _, r := range runs {
		if r.OutcomeSource == "explicit" {
			rated++
		}
	}
	if rated != 4 {
		t.Errorf("%d rated runs, want 4", rated)
	}
	if _, err := e.app.Simulate(ctx, SimOptions{Model: "qwen3:4b", Days: 1}); err == nil {
		t.Error("simulated again in a home with eval runs")
	}
}

// ADR-0006/0012: a model of ours that grew past decision_cache_gb over its fresh size is unloaded;
// models the user loaded are never touched.
func TestCapCachesUnloadsGrownModels(t *testing.T) {
	fake := &backendtest.Fake{}
	e := newApp(t, fake, nil, "")
	a := e.app
	at := func(name string, gb float64) backends.LoadedModel {
		return backends.LoadedModel{Name: name, SizeBytes: int64(gb * 1e9), Context: 8192}
	}
	run := func(driver string, loaded ...backends.LoadedModel) []string {
		fake.Resident, fake.Unloaded = loaded, nil
		a.invalidateModels()
		a.capCaches(driver)
		a.bg.Wait()
		return fake.Unloaded
	}
	if got := run("qwen3:4b", at("qwen3:4b", 3.7)); len(got) != 0 { // looks fresh (≤ 1.5 × 2.5 GB file): recorded
		t.Fatalf("unloaded a fresh model: %v", got)
	}
	if got := run("qwen3:4b", at("qwen3:4b", 4.5)); len(got) != 0 { // 0.8 GB over: under the cap
		t.Fatalf("unloaded under the cap: %v", got)
	}
	if got := run("qwen3:4b", at("qwen3:4b", 4.8), at("llama3:70b", 60)); !slices.Equal(got, []string{"qwen3:4b"}) {
		t.Fatalf("unloaded %v, want just qwen3:4b (llama3:70b isn't ours)", got)
	}
	// Never seen loaded and far over its file size (qwen3.5:4b at 12.7 GB in block 2c).
	if got := run("qwen2.5-coder:7b", at("qwen2.5-coder:7b", 12.7)); !slices.Equal(got, []string{"qwen2.5-coder:7b"}) {
		t.Fatalf("unloaded %v", got)
	}
}

func TestSimilarTools(t *testing.T) {
	for _, c := range []struct {
		p, q string
		want bool
	}{
		{"make_dir>list_dir>move_many", "list_dir>make_dir>move_many", true},
		{"make_dir>list_dir>move_many", "make_dir>list_dir>sanitize_name>sanitize_name>move_many", true},
		{"list_dir>read_text>write_text", "read_text>write_text", true},
		{"list_dir>make_dir>copy_verify", "read_text>write_text", false},
		{"web_search>fetch", "list_dir>move_many", false},
	} {
		if got := similarTools(c.p, c.q); got != c.want {
			t.Errorf("similarTools(%q, %q) = %v", c.p, c.q, got)
		}
	}
}
