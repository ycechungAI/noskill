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
- Status page can bench decision (JEV) and embedding models (`App.BenchOther`, `internal/app/benchother.go`)
  and pull staple models that aren't installed (`internal/app/staples.go`, task kind `pull`). Baseline
  embedding results: `docs/EMBEDDING_BENCH.md`. The live daemon needs a rebuild to get these.

## Update 2026-10-25 (housekeeping skills)

- Added 5 built-in skills: downloads-tidy, duplicate-finder, disk-watch, media-renamer, daily-brief (`skills/`,
  `internal/chores`, `internal/tools/builtin/chores.go`, `internal/app/chores.go`). New job kinds `remind`, `brief`,
  `diskwatch`; CLI `noskill remind` and `noskill brief`. Live-checked with granite4:3b in a scratch home.
- Not done: the live daemon still runs the older build; none of the five is scheduled yet (no `brief`/`diskwatch` jobs
  added to the user's config); disk-watch `volumes` setting untested on Windows/Linux (uses platform.ListVolumes).
- Status page: Brief section (digest + reminders box) between Skills and Models; endpoints `/noskill/v1/brief`,
  `/noskill/v1/reminders`. Housekeeping eval suite `internal/eval/tasks/chores.yaml` (11 tasks; granite4:3b scores
  8-10 of 11 run to run; tidy-01/tidy-02/rename-01 are the flaky ones). Eval tasks can now set `aged_days` and put files in subfolders.
- `./web.sh`: serves the docs site (8767) and the Phase 0 spike status (8765) in one run; the noskill status page has a
  Spikes tab that frames both (CSP `frame-src` allows only those two local ports). Windows: web.ps1 (written, not run on Windows). Both are the `web` item in start.sh / start.ps1.
- internal/deepresearch (the deep-research pipeline) is written and tested but not yet wired to a skill/tool/CLI/status page.

## Update 2026-10-26 (granite4.2:3b tool-calling fix)

- The user installed `granite4.2:3b` (replacing `granite4:3b`, no longer on this machine) and its eval score
  came in low: 76/96, against `granite4:3b`'s best recorded 84/96 (`eval history`). Root cause, checked live:
  the agent loop forces `think=false` on every non-final step; granite4.2's template mishandles full-off
  thinking, so it skipped tool calls (docs_search, tidy_apply, rename_apply) and once leaked a stray
  `</think>` into a plain answer. `think="low"` fixed both, and was faster than the `false` run's rambling.
  `granite4:3b` wasn't affected (it scored well at `think=false` already).
- Fix: `backends.ChatRequest.ThinkLevel` (string, sent as Ollama's `think` field instead of the bool) +
  `agent.Request.ToolThinkLevel`, applied on non-final steps only. `internal/app/run.go`'s `toolThinkLevel`
  scopes it to model name prefix `granite4.2` (not the whole `granite` family or every thinking-capable
  model) to avoid the latency problem `think=false` exists to prevent (docs/MODEL_ADAPTERS.md "Other
  small-model tactics"). Two full-suite re-runs after the fix: 82/96 then 84/96 — matches `granite4:3b`'s
  best. Code/math/summarise dips in the first re-run were confirmed as ordinary sampling noise (a
  category-only re-run came back clean), not caused by the change.
- Not done: no live check yet with the *old* `granite4:3b` tag re-pulled to confirm zero regression there
  (it's simply not installed on this machine right now); the fix is scoped narrowly enough that this seems
  low-risk, but worth a check if that model comes back into use.
