# Current state

_Last updated: 2026-11-03 by Flint (OpenCode) after the monorepo move + task-verification work_

## In progress

- **Move: scheduler + orcastrator now live inside the noskill repo** (`apps/scheduler/`, `apps/orcastrator/`,
  via `git subtree add --squash` on branches `feature/describe-task` and `feature/setup-flag`).
  - Old folders `~/dev/scheduler` and `~/dev/orcastrator` retired as working trees: git history kept via
    their `.git`, working content gutted to a `MOVED.md` tombstone (to do, see Next steps).
  - Repointed: `apps/macos/Sources/NoskillApp/Config.swift` defaults, `~/Library/Application Support/noskill/app.json`
    (`schedulerDir`/`orcastratorDir` now `…/dev/noskill/apps/{scheduler,orcastrator}`), docs/APP.md,
    docs/ADR-0017 (2026-11-03 addendum), noskill README, macOS app test fixtures, both moved repos'
    READMEs. The app now runs the three parts from the monorepo folders.
  - Runtime state migrated: copied `data/` + `runs/` + `config.yaml` from `~/dev/scheduler` to
    `apps/scheduler/`, rebuilt both `.venv`s there (apps/scheduler/.venv via `_venv.sh`, apps/orcastrator/.venv
    via start.sh --setup), restarted noskill serve / sched daemon + serve / orcastrator-web from the new
    folders; verified ports 11435 / 8787 / 8400 listening again.
  - Commit: `a35c89c` "Tasks: honest run verification, sources-driven instruction following, delete fix, monorepo move".

- **Task verification work (2026-11-03)** — triggered by the daily-game-deals incident (fabricated deal table,
  `status: success` after a useless single search).
  Implemented in `apps/scheduler/src/schedlm/`:
  - `agent/verify.py`: deterministic checks (no_web_data; required_source_not_fetched/failed via new task
    frontmatter `sources:`; unbacked_report via `checks.require_cited_sources`; min_report_links).
  - Task frontmatter gained optional `sources:` (URLs that must be fetched with `fetch_page`) and `checks:`
    (require_sources, min_report_links, require_cited_sources) + validation.
  - Executor: appends imperative `_sources_block()` after the prompt; on finalization runs verification; one
    bounded retry (`CHECKS_FAILED`) naming failures; honest status success|partial|failed recorded.
  - Store: `runs.status` CHECK now allows `partial` (+ live-DB migration in `init_db`); report frontmatter
    gained `checks_failed:`; CLI prints partial-as-warnings.
  - Verdict chosen for the user's question ("learn new skills vs more precise instruction following"):
    **precise instruction following + deterministic verification** (recommended), rather than the noskill
    skill-learning loop — the fabrication here was instruction-shape/URL-shape failure on a plain task loop.

- **Fixes this session**:
  - Delete on the sched Tasks tab was dead *inside the noskill app*: the embedded WKWebView had no UI delegate
    so JS `window.confirm()` silently returned false; replaced with a two-step JS button (`data-del`
    disarm/undo). Native JS `alert()` (error paths) still wants a WKUIDelegate (possible follow-up).
  - `fetch_page`: accepts JSON APIs (`application/json` / `.json` URLs); prepends note when URL has a `#`
    fragment (browser-only state; fetch returns the unfiltered page).

- **Task rewrite**: `daily-game-deals` now runs on the CheapShark JSON deals source (`sources:` +
  `checks:` in task file) instead of an impossible isthereanydeal `#filter:…;sort:expiry` fetch. ITAD tasks
  can still use `sources:` for unfiltered browsing fetches.

## Next steps

- Live-check: `sched run daily-game-deals` once the stack is healthy, watch honest status. (Just restarted:
  noskill serve / sched daemon + serve / orcastrator-web from new monorepo paths; ports 11435/8787/8400
  re-listening — verify ollama is up.)
- Decide whether the noskill Go side (`internal/app`, FR jobs) gets the same verification shape.
- Add WKUIDelegate to the app's WKWebView for JS alert/confirm in embedded pages.
- Gut the retired repo folders to `MOVED.md` tombstones (todo) — don't delete before the above.

## Gotchas

- `granite4.2:3b`: tool-calling fix via `ThinkLevel` scoping to the `granite4.2` prefix (see Update 2026-10-26).
- Any binary that runs scripts must call `platform.MaybeRunSandboxChild()` first in `main` (ADR-0008) — easy to
  forget when adding a new `cmd/` entry point.
- After `git subtree add`, file owner/perm bits can be off: `chmod -R u+rwX` on the added dirs.

## Update 2026-11-03

- Monorepo move landed (commit `a35c89c`); runtime state migrated, venvs rebuilt, services restarted from
  the new paths.
- Task-verification work landed (commit `a35c89c`); 163 sched tests pass; honest statuses + sources/checks +
  retry implemented and tested.
- Delete bug fixed in sched web UI (two-click); app WKWebView native-dialog gap documented, open.
