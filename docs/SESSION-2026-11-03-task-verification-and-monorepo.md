# 2026-11-03 — Task verification, monorepo move, app delete fix

Session work that landed on branch `feature/monorepo-insights` (commits `a35c89c`,
`a39e872`, `ee18b9b`, `27b363d` + the webapi running-indicator commit).

## Why

The scheduled task `daily-game-deals` ran against an isthereanydeal.com link with a
`#filter:…;sort:expiry` fragment and produced a fabricated deals table, recording
`status: success` — the run only did one useless `web_search` and never fetched the
deals. The user asked for better task following plus honest checks (learn-skills vs
precise instruction following → we chose the latter plus deterministic verification).

## What was built (scheduler, now `apps/scheduler/`)

- `src/schedlm/agent/verify.py` (new): deterministic run checks — `no_web_data`,
  `required_source_not_fetched`, `required_source_failed`, `unbacked_report`,
  `min_report_links`.
- Task frontmatter gained optional `sources:` (URLs that must be fetched via
  `fetch_page`) and `checks:` (`require_sources`, `min_report_links`,
  `require_cited_sources`).
- Executor: appends an imperative `_sources_block()` after the task prompt, runs
  verification at finalization, one bounded `CHECKS_FAILED` retry naming the failures,
  then records honest status `success` / `partial` / `failed`.
- Store: `runs.status` CHECK extended to include `partial` and `failed`, with a live-DB
  migration for older schemas.
- Report frontmatter gained `checks_failed:`; CLI prints `partial` as a warning.
- Tools: `fetch_page` accepts JSON APIs (`.json`/content-type), notes `#`-fragments
  (browser-only state), descriptive UA, 30s timeout.
- Web API: task list shows `running` while a task is in flight (`task_state.running_pid`).

## Live verification

`sched run daily-game-deals` → sources failed (network) → honest `FAILED` → retry →
final → `partial` (sources failed / unbacked / min_report_links) → "report written with
verification warnings (status=partial)". Later runs: `success`.

## Monorepo move

scheduler and orcastrator moved into this repo via `git subtree add --squash` as
`apps/scheduler/` and `apps/orcastrator/`; live `data/` + `runs/` + `config.yaml` migrated;
venvs rebuilt there; services restarted from new paths; app.json / `Config.swift` / docs /
ADR-0017 / tests repointed; old folders retired to `MOVED.md` tombs.

## App

`apps/macos/build.sh` runs the app test suite then the Swift build (+ Go agent); the DMG
is packaged by `apps/macos/make-dmg.sh`. Root-level convenience script: `./build.sh`.

## Fixes after the first app build (same day)

- **"Error" badge while a task is running** (the user's main annoyance): the web
  UI's `_open_store()` called `recover_running_state()` on EVERY request, which
  flipped any `runs` row in `status='running'` to `'error'` ("daemon terminated
  during run") and nulled `running_pid` — mid-run. The recovery is now
  pid-aware (`store._pid_alive`): a live `running_pid` means genuinely running and
  is never touched; recovery only marks rows whose owner process is dead.
  Live-verified: manual run showed `running`, then `success`.
- **Live running indicator**: `_task_summary()` reports `running` while
  `task_state.running_pid` is set.
- **Starter tasks, soft-persistent on first run**: `starter_tasks/` ships the four
  user-made tasks (daily-briefing, daily-game-deals, github-trending,
  weekly-market-summary). On first run, an untouched (no `*.md`) tasks dir is
  seeded once and marked with `.starters-seeded`; after that the user can delete
  them and they never reappear. Non-empty installs are never seeded over.
- **Task editor "instructions omitted"**: the AI "Fill in" flow now always hands
  the model the text currently in the editor, so it edits instead of reinventing
  a thinner task from the one-line description. The raw-file textarea (the whole
  task: frontmatter + body) is labeled as such, lists every supported frontmatter
  key, and `NEW_TASK_TEMPLATE` shows the `sources:`/`checks:` knobs. The tasks
  page is served with `Cache-Control: no-store` so the embedded WKWebView can't
  show a stale page.

## Known limits / follow-ups

- No vision-capable local model installed (granite4.3b, gemma3:1b) → image-input runs fail
  with "Cannot read … (this model does not support image input)".
- Native JS `alert()` in the embedded WKWebView still wants a UI delegate (delete is fixed
  JS-side via two-click button).
