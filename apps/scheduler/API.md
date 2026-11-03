# API.md — CLI reference

Entry point: `sched` (typer app in `cli.py`, declared in `pyproject.toml` `[project.scripts]`). All commands exit `0` on success, `1` on user error (bad id, invalid file), `2` on environment error (Ollama unreachable when required).

## sched list

Table of all tasks (valid and invalid files). The `#` column is a stable, 1-based shortcut (tasks sorted by id) that `sched run` also accepts instead of the task id.

```
#  ID                     SCHEDULE     ENABLED  LAST RUN            STATUS   NEXT RUN
1  daily-briefing         0 7 * * *    yes      2026-07-18 07:00    success  2026-07-19 07:00
2  github-trending        0 8 * * *    yes      2026-07-18 08:00    success  2026-07-19 08:00
3  weekly-market-summary  0 8 * * 6    yes      2026-07-11 08:00    success  2026-07-18 08:00
-  bad-task.md            -            -        INVALID: no id      -        -
```

Flags: `--json` (machine-readable array; each task object includes a `"number"` field matching the `#` column).

## sched add

Two modes:
- `sched add --file <path>`: validate and copy the file into `tasks/` (error if id exists).
- `sched add --nl "<sentence>" [--yes]`: Planner drafts the task, the full file is printed, user confirms `y/N` before it is written (`--yes` skips). Requires Ollama.

## sched show <id-or-number>

Prints the task file verbatim, then computed info: next 3 run times, last 3 runs (status + report path). Accepts a task id or its `#` number from `sched list`.

## sched run <id-or-number-or-all>

Execute now (trigger `manual`), ignoring `enabled` and schedule. `TASK` accepts a task id, its `#` number from `sched list`, or the literal `all` to run every task in id order, continuing past individual failures.

Before executing, checks that Ollama is reachable at `ollama.host` and tries to start it (`ollama serve`, backgrounded, output to `data/ollama-serve.log`) if not — this is what makes an unattended `launchctl`-managed daemon (see README "Running at login") able to start Ollama itself rather than failing every run until it's started by hand.

Prints a live line for every tool call, tool result preview, and model "thinking" output (if the model supports it — silently skipped otherwise) as the run progresses, then the full report text once it finishes, then the report path (or the failure). This applies to every trigger, not just manual runs: the same lines are always written to `data/sched.log`, and `sched daemon` (foreground, not `--once`) mirrors them to stderr too — `sched run`/`sched run all` just also print them straight to stdout for a snappier interactive view.

Exit code: `0` if the run (or, for `all`, every run) succeeded, `2` otherwise.
Flags: `--model <name>` one-off model override (applies to every task when combined with `all`).

## sched enable <id-or-number> / sched disable <id-or-number>

Flips `enabled` **in the task file's frontmatter** (file is source of truth), then updates `task_state`. Disable also clears `next_run_at`; enable schedules the next slot from now, so a slot missed while disabled is not fired.

## sched rm <id-or-number>

Deletes the task file after `y/N` confirm (`--yes` to skip). Run history rows are kept; reports are kept.

## sched logs <id-or-number>

Last 10 runs from SQLite:

```
RUN  TRIGGER   STARTED             DURATION  STATUS   REPORT
44   schedule  2026-07-18 07:00    3m12s     success  runs/morning-brief/2026-07-18T07-00.md
43   manual    2026-07-17 22:14    2m40s     success  runs/morning-brief/2026-07-17T22-14.md
```

Flags: `--last N`, `--errors` (only failed runs), `--json`.

## sched daemon

Foreground tick loop (Architecture.md §4). Logs to stderr + `data/sched.log`. `Ctrl-C` = graceful shutdown. Flags: `--once` (single tick then exit — used by tests and by launchd/cron wrappers).

Like `sched run`, checks Ollama is reachable and tries to start it if not, once at process start — so a `launchctl`/cron-managed daemon can bring Ollama up itself on first use rather than every scheduled run failing until someone starts it manually.

Running at login: on macOS, `./start.sh` installs and repairs the `com.local.sched` LaunchAgent and the agent monitor's two agents (README "Running at login"). Elsewhere, use a systemd user unit that runs `sched daemon`, or `--once` under system cron as the low-tech alternative.

## sched monitor check | heal | serve

The agent monitor (Architecture.md §8). It watches Ollama, the daemon's LaunchAgent, its own status page, and whatever else is listed under `monitor.services` in `config.yaml`.

- `sched monitor check`: checks every service once and prints `ok`/`DOWN` with details. Exits 1 if any service is down.
- `sched monitor heal [--quiet]`: checks every service, then restarts whatever is still down after `monitor.heal_grace_seconds`. Prints each result; with `--quiet`, prints only when something was down. Exits 1 if something is still down afterwards. The `local.agent-monitor.heal` login agent runs this at login and every 10 minutes.
- `sched monitor serve [--host 127.0.0.1] [--port 8790]`: the status page; the `local.agent-monitor` login agent keeps it running. Routes:

| Route | Purpose |
|---|---|
| `GET /` | Status page |
| `GET /api/status[?refresh=1]` | `{"services": [{"name", "label", "ok", "detail", "can_heal", "checked_at"}], "last_heal": <last-heal.json or null>, "heal_log": [...], "events": [...]}`. `refresh=1` checks now instead of returning the latest background check |
| `GET /api/health` | `{"ok": true}` |
| `POST /api/services/{name}/heal` | Restarts one service now and returns `{"ok", "action", "status"}`. Requires the header `X-Sched-Monitor: 1` (`403` without it); `404` for an unknown name |

## sched serve

`sched serve [--host 127.0.0.1] [--port 8787]`: FastAPI app (Architecture.md §5 "Web UI") serving a browser page at `http://<host>:<port>/` to list, add, edit, remove, enable/disable, and run tasks — everything `sched list`/`add`/`show`/`enable`/`disable`/`rm`/`run` do from the terminal, from a browser instead. Binds to `127.0.0.1` by default (single-user local tool, no auth built in — don't bind it to a non-loopback address on a shared machine).

JSON API it serves (all under `/api`), usable directly (e.g. from `curl` or a script) without the page:

| Route | Purpose |
|---|---|
| `GET /api/tasks` | `{"tasks": [...], "invalid": [...]}` — same fields as `sched list --json`, plus `model` (the task's own override, or null) and `last_model` (model its latest run used) |
| `GET /api/tasks/file/{filename}` | Raw file content (works for invalid files too, so a broken task can be fixed) |
| `POST /api/tasks/file` | Create: `{"content": "<raw task file text>", "filename": "id.md" (optional)}`; validates via the same rules as `sched add --file`; `400` with the exact `TaskValidationError` message on invalid content, `409` on a duplicate id or existing filename |
| `POST /api/tasks/draft` | Describe a task in plain words: `{"text": "every weekday at 7am, summarise the AI news", "draft": "<task file so far>" (optional, for a follow-up change)}`. The planner model writes it (same as `sched add --nl`) and the reply is `{"id", "content"}` with a free id; **nothing is saved**. The New task panel's describe box uses it to fill the editor. `400` on empty text, `422` when the model can't produce a valid task, `503` when Ollama can't be reached |
| `PUT /api/tasks/file/{filename}` | Overwrite an existing file's content (same validation); `409` if the new content's id is already used by a *different* file |
| `DELETE /api/tasks/file/{filename}` | Delete the file (run history kept, same as `sched rm`) |
| `POST /api/tasks/{id}/enable` / `/disable` | Same as `sched enable`/`disable` |
| `GET /api/tasks/{id}/runs?last=10` | Same rows as `sched logs {id} --json` |
| `GET /api/runs/{run_id}/report` | Raw report file content |
| `POST /api/tasks/{id}/run` | Starts a run now (trigger `manual`); checks/starts Ollama first (`503` if it can't be reached), returns `{"run_token": ...}` immediately |
| `GET /api/models` | `{"ollama_up", "host", "default", "effective_default", "note", "can_change_default", "installed": [{"name", "size", "tools"}]}`. `effective_default` is what runs actually use: it differs from `default` when that model isn't installed or can't call tools, and `note` says why |
| `PUT /api/models/default` | `{"model": "<name>"}`: saves it as `ollama.model` in `config.yaml` (only that line changes); `400` on an invalid name, `409` if the server has no config file path |
| `PUT /api/tasks/{id}/model` | `{"model": "<name>"}` sets the task file's `model:` line; `{"model": null}` removes it so the task uses the default |
| `GET /api/runs/stream/{token}` | `text/event-stream` (SSE) of the run in progress: one `data: {"event": "..."}` line per tool call/result/thinking/finalization step (exactly what `sched run` prints to the terminal), ending with `data: {"done": true, "status": "...", "report_path": ...}` |

Task files stay the source of truth: every write goes through the same `tasks.parse_task_text` validator as the CLI, and a successful create/edit immediately syncs `task_state` (no waiting for the next daemon tick to pick it up).
