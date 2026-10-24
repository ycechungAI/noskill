# AGENTS.md

Guidance for AI coding agents working in this repo.

## State
Phase 3 (MIT, Go). The source of truth is [SPEC.md](SPEC.md) (requirements with IDs like FR-S3) and the docs in [docs/](docs/). Blocks 1a–2d are in place (the Phase 2 learning exit check was accepted as passed in 2d: steps −45%; success −8% from one task that used no learned skill); Block 3 is partly built (preconditions, external drives, editable approvals, anime-tracker), and so is block 4's decision calibration (FR-B6c); see [ROADMAP.md](docs/ROADMAP.md) for what's next. Settings go in [CONFIG.md](docs/CONFIG.md), tables in [DATA_MODEL.md](docs/DATA_MODEL.md), proxy behaviour in [PROXY.md](docs/PROXY.md); keep them in sync with the code.

The module path is `noskill`, a placeholder until the GitHub org is chosen ([PACKAGING.md](docs/PACKAGING.md#name-and-signing-checks)). Change it in `go.mod` and every import once that's decided.

Build and test: `go vet ./...` and `go test ./...`. Before finishing a change, vet all six targets: `for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do GOOS=${t%/*} GOARCH=${t#*/} go vet ./...; done`. Any binary that runs scripts must call `platform.MaybeRunSandboxChild()` first in `main` (ADR-0008).

## Rules
- Reference requirement IDs (e.g. `FR-O2`) in commits, PRs and code comments where a requirement is implemented.
- If an implementation needs to deviate from the spec, update the spec (and add an ADR in `docs/adr/` for architectural choices) in the same change.
- Every feature must work on macOS, Windows and Linux. No POSIX-only assumptions (paths, signals, shells) outside `internal/platform/`.
- Design for small models first (3B–8B, 8k context). Run live checks with `--model granite4:3b` (the main model: tools, about 9 s a task) or `gemma3:1b` (the backup: plain tasks only, no tools). `qwen3:4b` always reasons (about 1,000 tokens a step, ~3 minutes a task) and is too slow for test runs; `llama3.2` was dropped (3 of 10 on the checked `files` tasks). A trial must take minutes: use `--tasks N`, and never call an hour-long run a dry run.
- Skills are agentskills.io-compatible. Never break loading of plain `SKILL.md` files without a `noskill:` block.
- No hard deletes of user files anywhere; use the OS trash.

## Layout (Go)
✓ = exists (blocks 1a–2d, part of 3 and 4); the rest is planned.
```
cmd/noskill/            ✓ CLI entry
cmd/noskill-tray/       ✓ tray app: thin, draws internal/tray's menu with fyne.io/systray (CGO allowed only here; ADR-0004, ADR-0011)
internal/tray/          ✓ the tray's client, menu model and icon (no CGO)
internal/eval/          ✓ checked tasks (embedded YAML), checks, reports, comparison
internal/agent/         ✓ loop, loop guards, context fitting, JSONL traces
internal/skills/        ✓ loader, selector, learned-skill status, import from other runtimes (compat.go)
internal/tools/         ✓ executor, script runtime, builtin tools, wasm
internal/orchestrator/  ✓ classifier, memory budget, router, bandit, escalation and hand-off rules
internal/backends/      ✓ interface + registry; ollama and openai (1b); mlx and vllm (Phase 4, built early)
internal/backendtest/   ✓ scripted fake backend for tests
internal/decide/        ✓ Decider: jev (/v1/systemone) + emulated fallback; calibration and threshold tuning (FR-B6c)
internal/perms/         ✓ permission broker: scopes, tokens, host checks, file ops + undo log
internal/sandbox/       ✓ copy-in workspace, egress proxy, script runner, doctor self-test
internal/config/        ✓ layered config + `config check`
internal/ids/           ✓ ULIDs
internal/logging/       ✓ slog setup
internal/learning/      ✓ traces, mining, skill-author passes, verify, library, refine, merge (ADR-0010); skill optimisation and tuned built-ins (FR-L9, ADR-0014); training datasets (FR-L8, ADR-0015)
internal/scheduler/     ✓ jobs, cron, lease, retries, preconditions (FR-J5)
internal/approvals/     ✓ approvals queue + confirm adapter
internal/notify/        ✓ ntfy, Telegram, Discord, email, OS channels
internal/app/           ✓ the engine: plan, run, undo, outcomes, memory hooks
internal/cli/           ✓ every command (cmd/noskill is a thin main)
internal/memory/        ✓ facts and preferences
internal/volumes/       ✓ volume:<label> bindings to drive UUIDs
internal/watch/         ✓ watcher skill logic: pages, feeds, releases, search items and what's new (no I/O)
internal/docs/          ✓ local-docs index in docs.db: extract, chunk, FTS5 + vector search
internal/anime/         ✓ anime-tracker: torrent-app detection, Transmission + qBittorrent, Nyaa, AniList, library scan, naming, filing
internal/secrets/       ✓ OS keychain, no CGO
internal/service/       ✓ launchd, systemd --user, Task Scheduler
internal/update/        ✓ verified self-update, rollback
internal/registry/      ✓ signed skill folders and registry indexes: manifests, ed25519 sign/verify (FR-S10, ADR-0016)
internal/probe/         ✓ tool-strategy probe
internal/schema/        ✓ tool-argument validation and repair
internal/server/        ✓ Host/Origin/token checks, OpenAI + Ollama proxy, /noskill/v1 status page at /ui, admin calls for the tray
internal/platform/      ✓ per-OS: paths, trash, memory, sandbox (macOS only), clone, volumes list and IDs, power, installed apps, app prefs, plain notifications
internal/store/         ✓ sqlite + forward-only migrations
scripts/docsite/        ✓ builds the docs site from the Markdown (static HTML; fails on broken links; `go run ./scripts/docsite`)
skills/                 ✓ 10 built-in skills embedded (web-search, research, storm, files, orchestrator, model-manager, code-runner, anime-tracker, watcher, local-docs); skill-author is a spec for the learning loop
```

## Shared project memory

Before starting work, read `.agent-memory/STATE.md` and `.agent-memory/DECISIONS.md` for current
context, open threads, and why past decisions were made — another agent or tool may have left off
mid-task. When you finish a meaningful chunk of work, update `.agent-memory/STATE.md` with what
changed and what's next, and append to `.agent-memory/DECISIONS.md` if you made a non-obvious call.
