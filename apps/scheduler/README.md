# sched — a local LLM task scheduler

> **Part of the noskill app.** This is the **Tasks** part of the noskill app (`~/dev/noskill`, see
> `docs/APP.md` there), which starts it together with the noskill agent (`~/dev/noskill`) and the
> research reports of orcastrator (`~/dev/orcastrator`) and shows its web page in one window. It
> still runs on its own with `./start.sh` and `./web.sh`.
>
> The app runs `.venv/bin/sched daemon` and `.venv/bin/sched serve --port 8787` from this folder
> and checks port 8787, so keep those commands and that port working, or update the app with them.
> If the login agents from `./start.sh` are installed, the daemon is already running; don't run
> both (see "Running at login").

A single-machine scheduler that runs **LLM-powered tasks at scheduled times** using **local Ollama models**, with **web search** ability. Each run produces a Markdown report on disk. No cloud LLM APIs; the only network access is web search / page fetching over HTTP.

Think of it as a local, private version of "scheduled Claude tasks": you define a task as a Markdown file with a prompt and a cron schedule, a daemon wakes up at the right time, an agent loop (Ollama model + tools) executes the prompt, and the result lands in `runs/<task-id>/<timestamp>.md`.

## Document map (read in this order)

| File | What it contains |
|---|---|
| [spec.md](spec.md) | Goals, requirements (FR/NFR), user stories, acceptance criteria |
| [Architecture.md](Architecture.md) | Components, data flow, directory layout, tech stack, edge cases |
| [DataModel.md](DataModel.md) | Task file format, SQLite schema, run report format, config file |
| [Agents.md](Agents.md) | Agent roles, exact system prompts, tool JSON schemas, agent loop pseudocode, model table |
| [API.md](API.md) | CLI commands with exact flags, output formats, exit codes |
| [Testing.md](Testing.md) | Unit/integration test plan and manual smoke tests |
| [ToDO.md](ToDO.md) | Ordered build milestones M0–M6 with checkboxes and acceptance criteria |
| [tasks/](tasks/) | The 3 real task definitions: daily GitHub trending digest, weekly market summary, comprehensive daily briefing |

## How to build this project (instructions for the builder model)

1. Read `spec.md` fully. Do not add features it does not list.
2. Read `Architecture.md` and `DataModel.md`. They fix every structural decision (language, libraries, file tree, schemas). **Do not substitute libraries or invent new directories.**
3. Implement milestone by milestone from `ToDO.md`, in order. Each milestone has acceptance criteria — verify them before moving on.
4. Use `Agents.md` verbatim for system prompts and tool schemas.
5. Use `Testing.md` to write tests as you go, not at the end.

## Conventions (binding for the builder)

- Python 3.11+, type hints on every public function, `pathlib` over `os.path`.
- Only the dependencies listed in `Architecture.md` § Tech stack. Standard library first.
- Task files on disk are the **source of truth**; SQLite only stores derived state and run history.
- Small modules, small functions. No class hierarchies where a function works.
- Every error path logs and is reflected in the run record — the daemon must never crash because one task failed.
- All times handled timezone-aware via `zoneinfo`; never naive datetimes.

## Quickstart (once built)

```bash
ollama pull qwen3:8b            # default executor model — confirm it supports
                                 # tool calling with `ollama show qwen3:8b`
                                 # (not every local model does; see Agents.md §1)
pip install -e .                # or: uv sync
sched add --file tasks/github-trending.md
sched list
sched run github-trending       # run once, right now
sched daemon                    # foreground loop; Ctrl-C to stop
sched serve                     # web UI at http://127.0.0.1:8787 to add/edit/remove/run tasks
```

Or use the two launcher scripts. They create `.venv` on first run and repair it when needed (base Python upgraded or removed, project folder moved, `pyproject.toml` changed). They work from any directory, through a symlink, and under launchd/cron's minimal `PATH`. Set `SCHED_PYTHON=/path/to/python3` to choose which interpreter builds the venv.

```bash
./start.sh                      # macOS: install/repair the login agents so the daemon and the agent
                                #   monitor run in the background and at every login (see "Running at login")
./start.sh --status             # is it all running, and on which default model?
./start.sh --foreground         # run the daemon in this terminal instead; Ctrl-C to stop
./web.sh                        # foreground web UI at http://127.0.0.1:8787; args pass through, e.g. --port 9000
```

## Models

Tasks run on `ollama.model` from `config.yaml` unless a task sets its own `model:`.
Both can be changed from the web UI: the **Default model** picker at the top
(rewrites `ollama.model` in `config.yaml`) and the **Model** picker on each task
card (rewrites that task's `model:` line; "Default" removes it). A running
daemon picks up either change on its next tick, no restart needed.

The bar at the top of the web UI shows which LLM runs actually use right now,
including while a task is running. Each run's live log starts with
`using model <name>`, and every report footer records the model.

If the chosen model can't be used, runs fall back to the largest installed
model that can call tools rather than failing, and the reason is logged and
shown in the web UI. That covers a model that is no longer installed
(`ollama pull <name>` brings it back) and one that can't call tools. Small
(1–4B) models are more likely to skip searches and invent results, so a run
that tries to finish without any search or fetch is told to research first,
and a report written without any sources gets a warning under its title.

Note: `sched` is also a zsh shell builtin. If a plain `sched ...` silently does
nothing in an interactive zsh terminal, use `command sched ...` instead (a
script or non-interactive shell doesn't have this problem).

## Running at login

`sched daemon` needs to keep running for scheduled tasks to fire. Two ways to
keep it running unattended, in increasing order of "always on":

### Option A — launchd (macOS), the daemon runs continuously

```bash
./start.sh              # install the login agent, or repair it, then show status
./start.sh --status     # running? which pid and default model?
./start.sh --restart    # restart it, e.g. after pulling code changes
./start.sh --uninstall  # stop it and remove it from login
```

`./start.sh` writes `~/Library/LaunchAgents/com.local.sched.plist` and loads it
with `launchctl bootstrap gui/$(id -u)`. macOS then lists it under System
Settings → General → Login Items & Extensions. The agent runs
`start.sh --foreground` from the project folder, so every login also checks and
repairs `.venv`. launchd restarts the daemon if it crashes. Output goes to
`data/daemon.stdout.log` / `data/daemon.stderr.log`, and the daemon's own log
is `data/sched.log`.

Running `./start.sh` again leaves a healthy agent alone. It repairs the agent
if the plist is missing or out of date (for example after you move the project
folder, or if you're replacing a hand-written plist from an older version of
this README), if launchd doesn't have it loaded, or if the daemon isn't
running. While the agent runs, `./start.sh --foreground` refuses to start a
second daemon that would race it for the same tasks.

`./start.sh` also installs and repairs two more login agents, the **agent
monitor** (Architecture.md §8):

- `local.agent-monitor` runs `sched monitor serve`, a status page at
  http://127.0.0.1:8790 showing whether Ollama, the daemon, the monitor itself
  and anything listed under `monitor.services` in `config.yaml` are up. launchd
  keeps it running.
- `local.agent-monitor.heal` runs `sched monitor heal` at login and every 10
  minutes. Whatever is still down after a 15-second grace period is restarted
  (LaunchAgents with `launchctl kickstart`, or loaded if they aren't; Ollama
  with `open -a Ollama`). Each restart is logged to `data/monitor/heal.log`.

`./start.sh --status` lists each service's current state, and
`./start.sh --uninstall` removes all three agents.

Don't run any of this with `sudo`: these are per-user LaunchAgents
(`gui/<uid>`), not system-wide LaunchDaemons.

### Option B — system cron, the low-tech alternative

No persistent process; a cron job invokes `sched daemon --once` on a fixed
interval, and each invocation checks for and fires anything due right now:

```cron
*/5 * * * * cd /absolute/path/to/scheduler && ./.venv/bin/sched daemon --once >> data/cron.log 2>&1
```

`--once` fires any due task **unconditionally**, regardless of its `catchup`
setting — this is deliberate (see Architecture.md §4's "`sched daemon --once`
is not 'startup'" note) so it works correctly as a cron-driven substitute for
the long-running daemon. Pick an interval close to your tightest task
schedule; a task can only be caught as promptly as `--once` is invoked.
