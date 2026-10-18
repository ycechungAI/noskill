# Architecture

## Overview

```
                ┌────────────── Clients ──────────────┐
                │ CLI · tray app · Open WebUI · other  │
                │ chat apps · scripts · agents         │
                └──────┬──────────────────────┬────────┘
            OpenAI/Ollama API                CLI / IPC
                       │                        │
┌──────────────────────▼────────────────────────▼───────────────────┐
│                         noskill daemon (Go)                        │
│                                                                    │
│  ┌────────────┐   ┌────────────┐   ┌──────────────┐  ┌──────────┐  │
│  │ Agent loop │──▶│ Skill      │──▶│ Tool runtime │─▶│ Sandbox/ │  │
│  │ (ReAct,    │   │ registry + │   │ builtin·uv·  │  │ perms    │  │
│  │ ctx mgmt)  │   │ selector   │   │ wasm·sh  │  └──────────┘  │
│  └─────┬──────┘   └────────────┘   └──────────────┘                │
│        │                                                           │
│  ┌─────▼──────┐   ┌────────────┐   ┌──────────────┐  ┌──────────┐  │
│  │Orchestrator│──▶│ Model      │──▶│ Backend      │  │Scheduler │  │
│  │ (router,   │   │ profiles + │   │ adapters     │  │ (cron,   │  │
│  │ bandit)    │   │ benchmarks │   │              │  │ precond.)│  │
│  └────────────┘   └────────────┘   └──────┬───────┘  └──────────┘  │
│                                           │                        │
│  ┌──────────────────── Learning ──────────┼──────────────────────┐ │
│  │ Trace store → Outcome judge → Synthesiser → Verifier →        │ │
│  │ Curator → (opt) Dataset export / LoRA                         │ │
│  └───────────────────────────────────────┼──────────────────────┘ │
│  Storage: SQLite (state, traces index, jobs, memory, vectors)     │
│           Files (skills/, traces/*.jsonl, config.toml)             │
└───────────────────────────────────────────┼────────────────────────┘
                                            │
  Ollama (chat + /v1/systemone decisions) · llama.cpp · LM Studio · MLX · vLLM · OpenAI-compatible
```

## Tech choices (recommended)

| Concern | Choice | Why |
|---|---|---|
| Core language | **Go** | Single static binary on 6 targets with plain `GOOS/GOARCH` cross-compile, no CGO needed; fast startup; good concurrency for daemon + jobs; easy to ship. Rust is a close alternative (see [ADR-0001](adr/0001-core-language.md)). |
| Storage | **SQLite** via `modernc.org/sqlite` (pure Go) | No CGO; one file; durable jobs and traces. |
| Vectors | **sqlite-vec**-compatible schema, or brute-force cosine in Go for < 50k items | Skill/memory counts are small; brute force at this size takes well under 5 ms. |
| Embeddings | Backend embed model (e.g. `nomic-embed-text`, `embeddinggemma`) via Ollama | Reuses what the user already runs. |
| Decisions | JEV decision model via Ollama `/v1/systemone` (`tev1:0.8b` default on 16 GB, `tev1` above; [ADR-0006](adr/0006-decision-model-on-16gb.md)), emulated by chat model if absent | Typed answers with probabilities for routing, judging and safety checks: ~250 ms per question on a 16 GB M4, about a tenth of an emulated decision. |
| WASM tools | **wazero** (pure Go) | Sandboxed, portable tools with no interpreter install. |
| Python tools | **uv** downloaded on first need into `~/.noskill/runtimes` | Fast, per-skill isolated envs, works on all OSes. |
| Node tools | Managed Node or **Bun** download on demand | Same idea. |
| Config | `~/.noskill/config.toml` + env overrides | Human-editable. |
| Secrets | OS keychain (macOS Keychain, Windows Credential Manager, libsecret) | Never in plain config. |
| Service install | launchd / systemd --user / Windows Task Scheduler or service | Native per OS (see [PLATFORMS.md](PLATFORMS.md)). |
| UI | Tray: separate `noskill-tray` binary with `fyne.io/systray` ([ADR-0004](adr/0004-tray-app.md)); status page: static web UI embedded in the daemon at `/ui` | Core stays CGO-free; no Electron, no webview. |

## Components

### Agent loop
- Input: messages + optional pinned model + budget.
- Steps: (1) orchestrator picks driver model; (2) skill selector shortlists skills; (3) model reasons and emits tool calls (native or grammar-constrained, see [MODEL_ADAPTERS.md](MODEL_ADAPTERS.md)); (4) tools run under permission checks; (5) results are compressed into context; (6) loop until answer, budget, or stop condition.
- **Small-model mode** (auto when context ≤ 8k or model < 7B): fewer skills shown (top-3), stricter output grammar, shorter observations, explicit "next action" schema instead of free-form ReAct.

### Skill registry and selector
- Watches skill directories, parses `SKILL.md` front-matter, indexes descriptions + example triggers as embeddings.
- Selection: embed query → cosine top-k → optional rerank by small model → inject chosen skill bodies.
- Tracks per-skill stats: uses, success rate, avg steps, last used.

### Tool runtime
- Uniform interface: `Call(ctx, tool, args) (Result, error)`, all with timeout, output size cap, and permission context.
- Runtimes: `builtin`, `script` (uv/node/bun/sh/pwsh), `wasm`.
- Script tools receive args as JSON on stdin and return JSON on stdout, which keeps them language-agnostic.

### Orchestrator
See [ORCHESTRATOR.md](ORCHESTRATOR.md).

### Backend adapters
Go interface with capability flags:

```go
type Backend interface {
    Name() string
    ListModels(ctx) ([]ModelInfo, error)
    Chat(ctx, ChatRequest) (ChatStream, error)      // tools optional
    Embed(ctx, EmbedRequest) ([][]float32, error)
    Load(ctx, model string, keepAlive time.Duration) error
    Loaded(ctx) ([]LoadedModel, error)              // for hardware-aware routing
}
```
Adapters: `ollama`, `openai` (covers LM Studio, llama.cpp server, vLLM, Jan, LocalAI), `mlx`, `jev` (decision models). See [MODEL_ADAPTERS.md](MODEL_ADAPTERS.md).

Decision models sit behind a separate interface, since they don't chat:

```go
type Decider interface {
    Decide(ctx, state map[string]any, questions map[string]Question) (Answers, error)
}
```
Implementations: `jev` (Ollama `/v1/systemone`) and `emulated` (any chat backend with constrained JSON).

### Learning
See [LEARNING.md](LEARNING.md). Runs as low-priority background work in the daemon, and only when the machine is idle (configurable), so it never competes with interactive use for the GPU.

### Approvals
- One durable queue (SQLite) for everything that needs a human: `confirm` tool calls, learned skills on first use (Q6), anime picks (every new title), permission escalations.
- Each item: id, source, summary, details link, created, expires, default on expiry (always **skip**), and a resume token (the run's continuation, [ADR-0009](adr/0009-scheduler-through-the-database.md)) so the waiting job/run continues where it stopped.
- Items can carry **editable fields** with a prefilled value and a type (`folder`, `text`, `choice`). The answer stores the final values; the waiting run gets them on resume. A `folder` field the user approves becomes a write scope for that skill (only that folder; never noskill's own folders or OS system folders).
- Channels: CLI (`noskill approvals`), OS notifications with action buttons where supported (macOS, Windows; Linux via D-Bus actions), tray menu, status page. First answer wins; others update.
- A job waiting on approval doesn't block other jobs.

### Scheduler
- Durable job table in SQLite; cron parser; precondition checks (volume mounted, AC power, network type, time window).
- The daemon (any `serve` process) is the only executor. The CLI only edits the database; a lease row makes sure one process schedules at a time ([ADR-0009](adr/0009-scheduler-through-the-database.md)).
- A run waiting on an approval stores a continuation (conversation, pending tool calls, model, skills) and picks up from there once approved.

## Data layout

Full schema, migrations and retention: [DATA_MODEL.md](DATA_MODEL.md). Settings: [CONFIG.md](CONFIG.md).

```
~/.noskill/                      (Linux/Windows split config and data folders: see PLATFORMS.md#paths)
  config.toml
  registries.json                # registries you added, with the key for each (FR-S10, ADR-0016)
  registry/<name>.json           # the last checked index from each
  state.db                       # jobs, approvals, skill index, profiles, memory, vectors, trace index
  docs.db                        # local-docs index (rebuildable)
  skills/
    builtin/   (read-only, shipped)
    user/      (yours)
    learned/   (created by learning loop; each has .versions/)
  tuned/<name>/                  # optimised built-in instructions: .versions/<n>/SKILL.md, TUNED.json (FR-L9, ADR-0014)
  train/datasets/<id>/           # train.jsonl, valid.jsonl, REVIEW.md, MANIFEST.json (FR-L8, ADR-0015)
  train/runs/<n>/                # RUN.json, adapters/, fused/, Modelfile, train.log
  traces/2026/10/15/<run-id>.jsonl
  runs/<run-id>/work/            # sandbox copy-in workspaces
  runtimes/  uv/  node/  wasm-cache/
  logs/  run/ (IPC socket)
```

## Key flows

**Interactive request via proxy**
1. Open WebUI → `POST /v1/chat/completions model=noskill/auto`.
2. Orchestrator: task type = research → driver = `qwen3:8b` (loaded, tool-capable).
3. Selector: `research` + `web-search`.
4. Loop: 6 tool calls → cited answer streamed back in OpenAI format.
5. Trace stored; thumbs-up from the client (if supported) or a later judge sets the outcome.

**Scheduled job**
1. 03:00, the daemon wakes job `anime-tracker`.
2. Preconditions: qBittorrent reachable ✓, on AC ✓.
3. Runs the skill headless. `confirm` actions never run unattended: new titles become approvals (with the folder to save in) and the run moves on.
4. Files finished episodes of already-approved titles: copy from `~/Downloads` to the chosen folder, verify, rename, then move the download to the trash, one at a time until none are left. If the folder's drive (e.g. `RED`) isn't mounted, the episodes wait.
5. Result → notification + trace.

**Learning (idle time)**
1. Curator scans new traces → finds successful 8-step run with no matching skill.
2. Synthesiser (strongest available local model) drafts `skills/learned/<name>`.
3. Verifier replays/derives tests in sandbox → pass → `candidate`.
4. Next N matching requests use the candidate in shadow or A/B → if better, promote to `active`.

## Extensibility points
- `Backend`, `ToolRuntime`, `Notifier`, `Precondition`, `OutcomeJudge`, `Router` are Go interfaces with registries.
- Script and WASM tools, so most extension never needs Go.
