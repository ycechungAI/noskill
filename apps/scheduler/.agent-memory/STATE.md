# Current state

_Last updated: 2026-10-26 by Claude Code_

## In progress

- Agent monitor is committed (195636c) and installed: `src/schedlm/monitor.py`, `sched monitor
  check|heal|serve`, status page `web/static/monitor.html` on http://127.0.0.1:8790, and
  `./start.sh` installs/repairs three LaunchAgents: `com.local.sched`, `local.agent-monitor`
  (serve, KeepAlive) and `local.agent-monitor.heal` (heal at login + every 600 s). Survived the
  2026-10-26 00:35 reboot, except the heal agent (see Gotchas). See Architecture.md §8.
- The user's local `config.yaml` (untracked) has a `monitor.services` entry for noskill
  (`dev.noskill.daemon`, port 11435).
- Untracked local files that stay out of commits: `.DS_Store`, `src/.DS_Store`,
  `src/schedlm/.DS_Store`, `.claude/settings.local.json`, `config.yaml`.

- Fixed 2026-10-26: **re-enabled tasks never fired.** Disable sets
  `task_state.next_run_at = NULL`, and enable (`cli.py` / `webapi.py` `_set_enabled`) used to
  leave it NULL (COALESCE in `upsert_task_state`), so `tick()` skipped those tasks while
  `sched list` / the web UI (which compute NEXT RUN from the cron string) looked fine. No
  scheduled run fired from 2026-10-21 to 2026-10-26. Now both enable paths call
  `daemon.first_next_run`, and `sync_task_state` gives any re-enabled task (or enabled task with a
  NULL `next_run_at`) a fresh slot from now. Tests: `tests/test_cli.py`, 3 in `test_daemon.py`, 1
  in `test_webapi.py`. The live `data/sched.db` was healed by running the new `sync_task_state`
  once (next: daily-briefing 2026-10-27 07:00). Committed as a966347; the daemon was restarted
  on it (pid 59872) and healed a blanked `next_run_at` on its first tick.

## Next steps

- `local.agent-monitor.plist.bak` is a leftover of an older monitor (code was at
  /Volumes/RED/ycechung/dev/cowork/local-agent-monitor, gone); safe to delete if the user agrees.

## Gotchas

- See `Agents.md`'s reliability note: a 3B-class model can call tools correctly, receive an
  "unavailable" error back, and still hallucinate a plausible answer instead of following the
  "don't invent facts" instruction. Treat the "low-RAM fallback" model column as a capability
  floor, not a quality-equivalent substitute, for anything report-trustworthiness-sensitive.
- launchd's `state = ` can be two words ("not running"); parse to end of line.
- `sfltool dumpbtm` needs admin and hangs waiting for it; use `launchctl print-disabled` instead.
- Between 2026-10-25 02:06 and the 2026-10-26 reboot, something (not this agent) moved
  `local.agent-monitor.heal.plist.bak` over the generated heal plist (mtime Oct 12, the .bak
  vanished), so the heal agent didn't load at login. `./start.sh` repaired it on 2026-10-26. The
  monitor doesn't watch the heal agent itself, so nothing restores it automatically.
- Installed Ollama models as of 2026-10-26: `granite4.2:3b` (the only one with tools),
  `gemma3:1b` (chat only), `nomic-embed-text` (embedding), `phonon` (TTS). `config.yaml` and all
  three task pins now use `granite4.2:3b` (were `qwen3.5:4b` / `granite4:3b`, both deleted).
  `resolve_model` now also swaps out models that can't chat, the Planner resolves its model too,
  and `/api/models` hides non-chat models (committed 1d56add). A running `sched daemon` /
  `sched serve` only gets the new code after a restart.
- Two sessions share this checkout's git index: staging can race. Commit from a private index
  (`GIT_INDEX_FILE=... git read-tree/apply --cached/commit-tree`) when another session is active.

## Update 2026-11-01

- sched is now the **Tasks** part of the noskill app (`~/dev/noskill/docs/APP.md`); README says so.
  The app runs `.venv/bin/sched daemon` and `.venv/bin/sched serve --port 8787` from this folder.
- Fixed the copy from user `neolinux`: owner permission bits were missing (added u+rw, u+x on
  folders), and `.venv` pointed at `/Users/neolinux` and Homebrew Python. Rebuilt `.venv` on
  uv-managed Python 3.12 (old one kept as `.venv.old-neolinux`, now ignored via `.venv.*/`).
  144 tests pass. Uncommitted: README.md, .gitignore.
- The default model `granite4.2:3b` isn't installed; runs fall back to `granite4:3b`.

## Update 2026-11-03 (retired: moved into noskill repo)

- Working tree retired; this checkout keeps only `.agent-memory/`, `.gitignore` and the `MOVED.md` tombstone.
- Code moved to `apps/scheduler/` in the noskill repo; branch `feature/describe-task` archived there.
- During the move the task-verification work landed in the moved copy: `agent/verify.py`, task
  frontmatter `sources:`/`checks:`, executor retry + statuses, store CHECK for `partial`/`failed`.
- Live proof: `sched run daily-game-deals` -> fetch sources failed -> honest `FAILED (status=failed)` ->
  retry -> final -> `partial` (sources failed, unbacked, min_report_links) -> "report written with
  verification warnings (status=partial)". Daemon bug found and fixed: `write_report` got hardcoded
  `status: "success"` while the honest verdict went to the DB/CLI; now uses `result.status`.
