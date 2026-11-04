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

## Update 2026-11-04: Auto fill for Add a job (FR-J7)

- The noskill status page's Overview → Jobs → "Add a job" now starts with a describe box and an **Auto fill**
  button. `POST /noskill/v1/jobs/draft` (admin) asks the local chat model (`authorModel`, same as learning
  passes) for a schema-constrained JSON draft and fills the form. Nothing is scheduled until the user edits
  and presses Add job. Invalid preconditions are dropped and reported in `notes`.
- Code: `internal/app/jobdraft.go` (DraftJob, prompt, schema), `internal/server/status.go` (draftJob),
  `internal/server/server.go` (route), `internal/server/ui/index.html` (describe box, Auto fill JS).
- Tests: `TestDraftJobFromTheStatusPage` in `internal/server/server_test.go`. `go vet ./...` and `go test ./...`
  pass; vet is clean on all six targets.
- Docs: SPEC.md FR-J7, docs/PROXY.md route row.
- Not yet live-checked against a real model (Ollama). Next: run Auto fill once in the browser with granite4:3b and
  check the filled fields for a skill job, a cron job and a `brief`/`remind:` job.

## Update 2026-11-04 (edit jobs, job results)

- Jobs can be edited on the status page (FR-J8): `POST /noskill/v1/jobs/{id}/edit`, same checks as adding
  (`app.jobFromSpec`), keeps ID/history/enabled, refused while running. Form gained Retries and Timeout.
  Code: `internal/app/jobedit.go`, `scheduler.Store.Update`, `server/status.go` (editJob), `ui/index.html`.
- Job results box under Jobs (`s-results`): latest 20 runs with ok / warning / error. Status carries
  `job_results` and each job's `spec` (form values).
- Honest-status rule (heuristic, `needsAttention` in jobedit.go): a `done` run whose text says "problems",
  "can't reach", "didn't answer", "couldn't", "please add", "i need to" shows as warning. Real data showed
  `anime-nightly` reporting done while Transmission didn't answer. The run status itself still says done:
  the scheduler should record a partial status like the sched side does (open).
- Found, not fixed: Transmission remote access is off (user setting, port 9091); a model reply that was a plan
  ("I need to list…") was stored as done (stage-download-anime-tracker).
- Side effect of testing: starting `noskill serve` without `-no-jobs` ran the catch-up jobs at 04:35
  (anime-nightly, diskwatch, docs-refresh, stage-download-anime-tracker). Test servers now use `-no-jobs`.
  The DMG check also left NoskillApp running (with its sched and orcastrator).
- Open: the Swift app's Ollama pane has no Auto fill picker; the edit form and results box are in the web page only.

## Update 2026-11-04 (build without Xcode)

- Command Line Tools' macOS 27 SDK fails to compile the app (`SwiftUIMacros.StateMacro` plugin missing).
  `build.sh` now picks the newest CLT macOS 26.x SDK when Xcode isn't selected (`NOSKILL_SDK` overrides it);
  `build-app.sh` and `test.sh` honour it. `./build.sh` builds `apps/macos/build/noskill-0.2.0.dmg` on CLT only.
- Open: the Auto fill model picker lives in the web page's Models section; the app's Ollama pane has no picker
  yet (needs the Swift side, written and built against the 26.5 SDK).
- Open: the recommended-model Pull path in the Models section is not yet tried in a browser.

## Update 2026-11-03

- Monorepo move landed (commit `a35c89c`); runtime state migrated, venvs rebuilt, services restarted from
  the new paths.
- Task-verification work landed (commit `a35c89c`); 163 sched tests pass; honest statuses + sources/checks +
  retry implemented and tested.
- Delete bug fixed in sched web UI (two-click); app WKWebView native-dialog gap documented, open.
