# Current state

_Last updated: 2026-10-24 by Claude Code_

## In progress

- Uncommitted changes on top of `3dfeac0 start` adding `orchestrator.exclude` (FR-O2): a config
  list of model names the router must never pick, even when installed and memory is free
  (`run --model` can still name one on purpose). Touches:
  - `internal/config/config.go` — new `Exclude []string` field on `Orchestrator`
  - `internal/config/check.go` — validates no entry in `exclude` is blank
  - `internal/orchestrator/route.go` — router rejects a model found in `Cfg.Exclude`
  - `internal/orchestrator/orchestrator_test.go` — new test coverage
  - Docs trimmed alongside it: `docs/CONFIG.md`, `docs/ORCHESTRATOR.md`, `docs/PROXY.md`,
    `docs/START.md`, `docs/WEEK_RUN.md`, `start.sh`, `start.ps1` (small doc/script cleanups, not
    all directly tied to `exclude` — worth a closer look before committing to confirm they're
    meant to land together).

## Next steps

- Review the full diff (`git diff`) and decide whether the docs/start-script edits belong in the
  same commit as `orchestrator.exclude`, or should be split out.
- Run `go vet ./...` and `go test ./...` across all six OS/arch targets (see AGENTS.md's build
  loop) before committing.
- Commit referencing `FR-O2`.

## Gotchas

- `qwen3:4b` always reasons (~1,000 tokens/step, ~3 min/task) — too slow for test runs; use
  `--model granite4:3b` (main, ~9s/task, tool-calling) or `gemma3:1b` (backup, plain tasks only,
  no tools) instead. `llama3.2` was already tried and dropped (3/10 on checked `files` tasks).
- Any binary that runs scripts must call `platform.MaybeRunSandboxChild()` first in `main`
  (ADR-0008) — easy to forget when adding a new `cmd/` entry point.

## Update 2026-10-25

- `orchestrator.exclude` and the status-page model actions / Add-a-job form are committed and live.
- New job kind `anime` (`noskill schedule add anime:download,file ...`): the scheduler runs the
  anime tracker's stages directly with no model (`Jobs.animeRun`, FR-J1). The live `anime-nightly`
  and `anime-follow` jobs use it (a model-driven follow run took 13 min and wandered; this takes ~10 s).
- Gotcha: a rebuilt `~/.noskill/bin/noskill` is a new binary to Little Snitch and to Full Disk
  Access: its Nyaa calls fail with "connect: bad file descriptor" until the user allows it again.
