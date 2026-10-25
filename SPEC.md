# noskill — Product Specification

Version: 0.1 (draft) · Date: 2026-10-15 · Status: planning

This spec defines **what** noskill must do. **How** lives in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and the subsystem docs. Requirement IDs are stable; reference them in issues and commits.

Priority: **P0** = MVP (v0.1), **P1** = v0.2–0.3, **P2** = later.

---

## 1. Scope

noskill is a local runtime that sits between **clients** (CLI, chat UIs, editors, other agents) and **model backends** (Ollama, llama.cpp, LM Studio, MLX, vLLM, any OpenAI-compatible server). It provides:

1. a **skill system** (load, select, run, permission),
2. an **agent loop** that works with any model,
3. a **learning loop** that creates and improves skills,
4. an **orchestrator** that routes work across models,
5. a **scheduler** for unattended jobs,
6. **interfaces**: CLI, OpenAI/Ollama-compatible HTTP proxy, status page, tray app.

## 2. Glossary

| Term | Meaning |
|---|---|
| **Skill** | A folder with a `SKILL.md` (instructions + metadata) and optional tools, scripts, references and tests. See [SKILL_FORMAT.md](docs/SKILL_FORMAT.md). |
| **Tool** | A single callable function with a JSON-schema input (built-in, script or WASM). Skills bundle and teach tools. |
| **Backend** | A model server noskill talks to (Ollama, llama.cpp, …). |
| **Model profile** | Measured facts about one model on this machine: capabilities, speed, memory, quality scores per task type. |
| **Trace** | A full record of one run: prompt, model(s), tool calls, results, outcome, cost. |
| **Learned skill** | A skill created or modified by the learning loop (as opposed to built-in or user-authored). |
| **Job** | A scheduled run of a skill or prompt. |
| **JEV decision model** | A model that answers typed questions (`choice`, `noul` yes/no, `score`) about an input in one fast call, via Ollama's `/v1/systemone` (TypeSafe Jev API), e.g. `nimble`, `tev1`. Doesn't chat. |
| **Decision** | One `decide` call: an input `state` plus named questions → typed answers with probabilities. Served by a JEV model, or emulated by a chat model. |

## 3. Functional requirements

### 3.1 Backends and models

| ID | P | Requirement |
|---|---|---|
| FR-B1 | P0 | Auto-detect running Ollama, LM Studio, llama.cpp server and any OpenAI-compatible endpoint on default ports at `init`. |
| FR-B2 | P0 | List all models per backend and build a **model profile** for each (capabilities: chat, tools, thinking, vision, embed, context length, quantization, size). |
| FR-B3 | P0 | Support **native tool calling** where the model has it (the default), and **grammar/JSON-schema constrained tool calling** where it doesn't or as a retry after a malformed call, so that any instruct model can call tools. The strategy per model comes from an `init` probe ([ADR-0005](docs/adr/0005-native-tool-calling-default.md)). |
| FR-B4 | P1 | *(built in 2b: speed, memory and a 12-task quality micro-eval per task type; decision-model accuracy isn't measured yet)* Run a short local **benchmark** per model (tokens/s, time-to-first-token, memory, plus a micro eval per task type) and store it in the profile. Re-run on model change. |
| FR-B5 | P1 | MLX backend on Apple Silicon and vLLM backend on Linux. |
| FR-B6 | P0 | **JEV decision models**: support Ollama's `/v1/systemone` decision models (`nimble`, `tev1`, …) as the `decide` capability, kept resident and reloaded when its prompt cache passes `decision_cache_gb` ([ADR-0006](docs/adr/0006-decision-model-on-16gb.md)). Default `tev1:0.8b` on ≤ 16 GB, `tev1` above when benchmarked fast enough. Used by the router, skill selector, outcome judge, safety checks and skills. See [MODEL_ADAPTERS.md](docs/MODEL_ADAPTERS.md#jev-decision-models). |
| FR-B6a | P0 | **Decision emulation**: when no decision model is installed, serve `decide` with the chat model via constrained JSON (+ logprobs when available), so every skill works on any model. |
| FR-B6b | P1 | Proxy passes `/v1/systemone` through to the backend, or emulates it, so TypeSafe SDK clients can use noskill. |
| FR-B6c | P1 | **Decision calibration**: store each thresholded decision (router task type, judge "answered") with its probability; once its run has an outcome, report per decision and model how well the probabilities match (`noskill calibration`). With `orchestrator.auto_tune` on (off by default) a threshold moves at most once a day, by ≤ 0.10, within fixed bounds, only when the evidence clearly separates right from wrong; every change is kept and revertable. See [MODEL_ADAPTERS.md](docs/MODEL_ADAPTERS.md#calibration). |
| FR-B7 | P2 | Optional cloud backends (Anthropic, OpenAI, OpenRouter) as **opt-in fallbacks** only, never default. |

### 3.2 Skills

| ID | P | Requirement |
|---|---|---|
| FR-S1 | P0 | Load skills from `~/.noskill/skills`, project-local `./.noskill/skills`, and extra configured paths. Format is compatible with the agentskills.io `SKILL.md` standard. |
| FR-S2 | P0 | **Progressive disclosure**: only name + description go into context at start; the full `SKILL.md` loads when selected; references/scripts load on demand. |
| FR-S3 | P0 | **Skill selection** that works with small context windows: embedding retrieval over skill descriptions → top-k shortlist → model picks (or the router picks for very small models). |
| FR-S4 | P0 | Tool runtimes: **built-in** (Go) and **subprocess script** (Python via managed `uv`, Node via managed runtime, shell per OS). |
| FR-S5 | P1 | *(built in 2b: wazero, WASI preview 1, no network yet)* **WASM tool runtime** (sandboxed, portable, no host interpreter needed). |
| FR-S6 | P0 | Every skill declares **permissions** (network domains, filesystem paths, exec, subprocess, schedule, secrets). Undeclared access is denied or prompts the user. |
| FR-S7 | P0 | `noskill skills list | show | enable | disable | test | install <git-url|path> | remove`. |
| FR-S8 | P1 | Import skills from other compatible runtimes (Claude Code, Hermes, Codex) by path, unmodified where possible. `noskill skills import` finds them in those runtimes' usual folders, reports what noskill does differently (no `allowed-tools`, `$ARGUMENTS`, `!`command`` lines, hooks; scripts only in the sandbox), shows the permissions, and copies the folder unchanged after a yes. |
| FR-S9 | P1 | Skill **versioning**: every change produces a new version; rollback with `noskill skills rollback <name> [version]`. |
| FR-S10 | P2 | Optional signed skill registry / index for sharing. Built in block 5c: ed25519, a key you make locally, registries as signed folders or git repos ([ADR-0016](docs/adr/0016-signed-skill-registry.md)). |

### 3.3 Agent loop

| ID | P | Requirement |
|---|---|---|
| FR-A1 | P0 | ReAct-style loop: plan → select skill → call tools → observe → repeat → answer, with a max step and token budget, a per-step output cap, and repeat-call detection. Tools are offered by default; per-model tool gating is optional and benchmarked ([ADR-0005](docs/adr/0005-native-tool-calling-default.md), S3). |
| FR-A2 | P0 | **Context management for small windows**: summarise/truncate tool output, keep a running scratchpad, evict old turns. Must work at 8k context. |
| FR-A3 | P0 | Streaming output for CLI and proxy, including thinking blocks when the model emits them. |
| FR-A4 | P0 | Write a **trace** for every run (JSONL + SQLite index). |
| FR-A5 | P1 | Sub-tasks: the loop can delegate a sub-task to another model via the orchestrator and merge the result. |
| FR-A6 | P0 | **Approvals queue** in the daemon for `confirm` actions, learned skills on first use, and anime picks (every new title). Channels: CLI (`noskill approvals`), OS notification with actions where supported; tray app in P1 (FR-I7). Each item has a timeout and a default (skip), and can carry **editable fields** with prefilled values (e.g. the folder to save a show in) that the user can change when approving. |

### 3.4 Learning (self-improvement)

Detailed in [LEARNING.md](docs/LEARNING.md).

| ID | P | Requirement |
|---|---|---|
| FR-L1 | P0 | **Outcome capture**: each trace gets an outcome (success / partial / fail) from explicit user feedback, skill-defined checks, or an LLM judge. |
| FR-L2 | P1 | **Skill synthesis**: after a successful run with ≥ N tool calls (default 5) and no matching skill, draft a new skill (instructions + tool sequence + tests). |
| FR-L3 | P1 | **Verification gate**: drafted or modified skills run their tests in the sandbox and must pass before status moves `draft → candidate → active`. |
| FR-L4 | P1 | **Skill refinement**: on repeated failures of a skill, propose a patch (instructions or script), verify, and A/B against the current version. |
| FR-L5 | P1 | **Curator**: periodically score, merge duplicates, and retire unused learned skills (`active → stale → archived`). Never auto-edit user-authored or built-in skills; fork them instead. |
| FR-L6 | P0 | **Memory**: persistent user facts and preferences (explicit + extracted), retrievable by skills. Viewable and deletable by the user. |
| FR-L7 | P1 | **Routing learning**: the orchestrator updates per-model, per-task-type success estimates from outcomes (see FR-O4). |
| FR-L8 | P2 | **Weight-level learning (opt-in)**: export curated traces as a fine-tuning dataset and run LoRA (MLX on Mac; Unsloth/axolotl on CUDA is deferred). Re-import as a new Ollama model and benchmark before routing to it. Gated on enough good runs and a reviewed dataset; an adopted model stands in for its base only on its task type ([ADR-0015](docs/adr/0015-lora-pipeline.md)). |
| FR-L9 | P2 | **Prompt/skill optimisation**: offline DSPy/GEPA-style optimisation of skill instructions against the skill's eval set. |

### 3.5 Orchestrator (model routing)

Detailed in [ORCHESTRATOR.md](docs/ORCHESTRATOR.md). The orchestrator is itself a built-in skill (`skills/orchestrator`) so it can be inspected, configured and improved like any other skill.

| ID | P | Requirement |
|---|---|---|
| FR-O1 | P0 | Classify each request/sub-task into a **task type** (chat, code, reasoning, research, vision, summarise, extract, creative, …), difficulty and needs, using one `decide` call when a JEV model is available. |
| FR-O2 | P0 | Choose a model from task type + model profiles + user rules (`code → qwen2.5-coder:7b`). Rules always win. |
| FR-O3 | P1 | **Hardware-aware**: account for VRAM/unified memory, which models are loaded, and swap cost; prefer loaded models when quality is close. On a 16 GB Mac models get 6 GB (the other half of the machine is the user's), growing toward Metal's ⅔-of-RAM limit only while memory is free and pressure is normal ([ADR-0012](docs/adr/0012-shared-machine-6gb-budget-with-burst.md)). |
| FR-O4 | P1 | **Learned routing**: contextual bandit over (task type, model) using trace outcomes, latency and user preference weights (quality vs speed). |
| FR-O5 | P1 | **Escalation**: if a small model fails a verifier/check, retry once on a stronger model, and record it for learning. |
| FR-O6 | P1 | `noskill route explain "<prompt>"` shows which model would be picked and why. |

### 3.6 Scheduler and background jobs

| ID | P | Requirement |
|---|---|---|
| FR-J1 | P0 | `noskill schedule add <skill|prompt> --cron <expr> | --every <dur>`; list, pause, resume, remove, run-now. |
| FR-J2 | P0 | Jobs run in the **noskill daemon**, installed as a user service: launchd (macOS), systemd user unit (Linux), Task Scheduler / Windows service (Windows). |
| FR-J3 | P0 | Missed-run policy (skip / run once on wake), overlap policy, timeout, retries with backoff. |
| FR-J4 | P0 | Job results are logged and optionally sent as notifications (OS notification, email, ntfy, Telegram, Discord webhook). |
| FR-J5 | P1 | **Preconditions**: e.g. "run only if volume `RED` is mounted", "only on AC power", "only on unmetered network". |

### 3.7 Interfaces

| ID | P | Requirement |
|---|---|---|
| FR-I1 | P0 | CLI: `init, run, chat, skills, schedule, approvals, models, route, memory, traces, serve, daemon, doctor`. Full reference in [docs/CLI.md](docs/CLI.md). **Top-priority interface.** |
| FR-I2 | P0 | **OpenAI-compatible proxy** (top priority) (`/v1/chat/completions`, `/v1/models`, `/v1/embeddings`): clients get skills transparently. Model name `noskill/auto` routes; a real model name pins it. Details in [docs/PROXY.md](docs/PROXY.md). |
| FR-I3 | P0 | **Ollama-compatible proxy** (`/api/chat`, `/api/generate`, `/api/tags`) on the same port as the OpenAI endpoint (as Ollama itself does), so apps that speak Ollama work by changing `11434` to `11435`. See [docs/PROXY.md](docs/PROXY.md). |
| FR-I4 | — | ~~MCP server~~ **Dropped 2026-10-15**: not planned for this project. Other agents can use the OpenAI/Ollama proxy. |
| FR-I5 | — | ~~MCP client~~ **Dropped 2026-10-15**: not planned. Tools come from built-in, script and WASM runtimes. |
| FR-I6 | P1 | *(built in 2b at `/ui`; polls `/noskill/v1/status`)* Local web status page served by the daemon: jobs, approvals, traces, skills (with learned-skill review queue), model profiles. The tray app opens it for details. |
| FR-I7 | P1 | *(built in 2b as `noskill-tray`; talks HTTP, not IPC: [ADR-0011](docs/adr/0011-tray-and-status-page-over-http.md); login-item install isn't built)* **Tray/menu-bar app** (macOS, Windows, Linux): daemon status, running/next jobs, approvals with approve/skip, pause all, open status page. Separate small binary talking to the daemon over IPC ([ADR-0004](docs/adr/0004-tray-app.md)). |

## 4. Built-in skills (initial set)

Specs live in [skills/](skills/).

| Skill | P | Summary |
|---|---|---|
| [web-search](skills/web-search/SKILL.md) | P0 | Search (SearXNG local, Ollama web search, Brave, DuckDuckGo), fetch, readable-text extraction |
| [research](skills/research/SKILL.md) | P0 | Multi-step research: plan questions → search → read → cross-check → cited report |
| [orchestrator](skills/orchestrator/SKILL.md) | P0 | Model routing (see §3.5) |
| [files](skills/files/SKILL.md) | P0 | Scoped file read/write/move/rename with OS-agnostic paths and drive detection |
| [anime-tracker](skills/anime-tracker/SKILL.md) | P1 | Scheduled discovery of anime you'd likely enjoy (AniList, no account). **You approve each title and pick its folder** (e.g. `/Volumes/RED/ANIME2026`). qBittorrent RSS rules using your existing feeds, 480p preferred; episodes download to `~/Downloads`, then are copied, renamed and verified into your folder, and the download goes to the trash |
| [model-manager](skills/model-manager/SKILL.md) | P0 | Suggest, pull, bench and clean up models that fit this machine's memory budget |
| [skill-author](skills/skill-author/SKILL.md) | P1 | The learning loop's writer: turns runs into skills, fixes learned skills ("make a skill from what we just did") |
| [code-runner](skills/code-runner/SKILL.md) | P1 | Sandboxed Python for exact math, CSV/Excel/JSON work and conversions |
| [watcher](skills/watcher/SKILL.md) | P1 | Scheduled watching of pages, RSS, GitHub releases and searches, with relevance filtering, alerts and digests |
| [local-docs](skills/local-docs/SKILL.md) | P1 | Answers from the user's chosen folders with file/page citations; local hybrid index |
| [downloads-tidy](skills/downloads-tidy/SKILL.md) | P1 | Sorts the loose files in Downloads into Documents, Images, Installers and so on; old installers go to the trash; one approval, undoable. The plan is made in Go |
| [duplicate-finder](skills/duplicate-finder/SKILL.md) | P1 | Finds files with identical content across folders and drives, suggests which copy to keep, trashes the extras after approval (the trash tool re-checks the content) |
| [disk-watch](skills/disk-watch/SKILL.md) | P1 | Free space per drive, what takes the space, large files nobody opened for a year; a model-free nightly job alerts only when a drive runs low |
| [media-renamer](skills/media-renamer/SKILL.md) | P1 | One naming pattern for the episodes in a folder, read with the anime tracker's own parser; one approval, undoable |
| [daily-brief](skills/daily-brief/SKILL.md) | P1 | A morning digest of noskill's own state (jobs, episodes filed, approvals, disk, what runs next) and plain-sentence reminders ("every Monday at 9 submit the report"); both are model-free scheduled jobs |

## 5. Non-functional requirements

| ID | Requirement |
|---|---|
| NFR-1 | **Platforms**: macOS 13+ (arm64, x64), Windows 10/11 (x64, arm64), Linux glibc 2.31+ and musl (x64, arm64). |
| NFR-2 | **Distribution**: single static CGO-free core binary < 40 MB, plus an optional small `noskill-tray` binary; installers via Homebrew, winget, Scoop, apt/rpm, AUR, `curl | sh`, and a signed/notarized macOS build. See [docs/PACKAGING.md](docs/PACKAGING.md). |
| NFR-3 | **Performance**: runtime overhead < 20 ms p50 per agent turn (excluding model time); idle RSS < 50 MB; daemon CPU ~0% idle. |
| NFR-4 | **Offline**: all features except internet-requiring skills work with no network. |
| NFR-5 | **Privacy**: no telemetry by default. Traces and memory stay local, encrypted at rest optional (OS keychain key). |
| NFR-6 | **Reliability**: daemon survives sleep/wake and backend restarts; jobs are durable (SQLite, [DATA_MODEL.md](docs/DATA_MODEL.md)); updates are atomic and roll back on failure. |
| NFR-7 | **Security**: see [SECURITY.md](docs/SECURITY.md). Least privilege per skill, built-in tools through a path/host broker, scripts in an OS sandbox with a copy-in workspace and egress proxy, secrets in OS keychain, signed releases. |
| NFR-8 | **Extensibility**: new backends, tool runtimes and notifiers are Go interfaces; no core change needed. |
| NFR-9 | **Accessibility & i18n**: CLI output works with screen readers (no color-only signals); messages are translatable. |

## 6. Acceptance criteria for MVP (v0.1)

1. On a fresh Mac, Windows and Linux machine with Ollama and `qwen3:4b` (or similar), `noskill init` completes with no manual config.
2. `noskill run "what changed in the latest Ollama release? cite sources"` returns a cited answer using the web-search skill, with a 4B model, at 8k context.
3. The same request through Open WebUI also uses the skill, both via the OpenAI-compatible endpoint and via the Ollama-compatible port.
4. A rule `code → qwen2.5-coder` routes a code question to that model; `noskill route explain` shows why.
5. A scheduled job runs while the CLI is closed, survives a reboot, and sends a notification.
6. A skill attempting to read a path outside its manifest is blocked and the attempt shows in the trace.
7. A scheduled job that hits a `confirm` action waits in the approvals queue, shows an OS notification, and continues after `noskill approvals approve <id>`.

## 7. Out of scope (v1)

See [VISION.md non-goals](docs/VISION.md#non-goals-for-v1).
