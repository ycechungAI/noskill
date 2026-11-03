"""Typer CLI entry point. Commands per API.md."""

from __future__ import annotations

import dataclasses
import json
import logging
import shutil
from datetime import datetime
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import Optional

import typer

from schedlm import daemon as daemon_mod
from schedlm import report as report_mod
from schedlm import schedule as schedule_mod
from schedlm import store as store_mod
from schedlm import tasks as tasks_mod
from schedlm.agent.executor import run_task as agent_run_task
from schedlm.agent.planner import PlannerError, nl_to_task
from schedlm.config import Config, load_config

app = typer.Typer(add_completion=False, no_args_is_help=True)

_LOG_FORMAT = "%(asctime)s %(levelname)s %(message)s"


CONFIG_PATH = Path("config.yaml")


def _load_cfg() -> Config:
    return load_config(CONFIG_PATH)


def _setup_file_logging(cfg: Config) -> None:
    """Rotating logfile at data/sched.log (5 MB x 3), per Architecture.md §7.
    Idempotent -- safe to call once per process even across multiple
    typer callback/command invocations in the same run."""
    root = logging.getLogger("schedlm")
    if root.handlers:
        return
    log_path = Path(cfg.paths.data) / "sched.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    handler = RotatingFileHandler(log_path, maxBytes=5 * 1024 * 1024, backupCount=3)
    handler.setFormatter(logging.Formatter(_LOG_FORMAT))
    root.addHandler(handler)
    root.setLevel(logging.INFO)


@app.callback()
def _main() -> None:
    """Set up logging once before any command runs."""
    _setup_file_logging(_load_cfg())


def _tasks_dir(cfg: Config) -> Path:
    return Path(cfg.paths.tasks)


def _db_path(cfg: Config) -> Path:
    return Path(cfg.paths.data) / "sched.db"


def _open_store(cfg: Config):
    conn = store_mod.init_db(_db_path(cfg))
    store_mod.recover_running_state(conn)
    return conn


def _find_task(task_id: str, valid: list[tasks_mod.Task]) -> tasks_mod.Task | None:
    return next((t for t in valid if t.id == task_id), None)


def _numbered_tasks(valid: list[tasks_mod.Task]) -> list[tasks_mod.Task]:
    """Stable order used for both `sched list`'s '#' column and `sched run
    <number>` -- sorted by id so numbers stay consistent across commands as
    long as the task set itself hasn't changed."""
    return sorted(valid, key=lambda t: t.id)


def _resolve_task_ref(
    ref: str, valid: list[tasks_mod.Task]
) -> tasks_mod.Task | None:
    """Resolve a task id or its 1-based `sched list` number."""
    task = _find_task(ref, valid)
    if task is not None:
        return task
    if ref.isdigit():
        numbered = _numbered_tasks(valid)
        idx = int(ref)
        if 1 <= idx <= len(numbered):
            return numbered[idx - 1]
    return None


def _print_table(headers: list[str], rows: list[list[str]]) -> None:
    widths = [len(h) for h in headers]
    for row in rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(str(cell)))

    def fmt(cells: list[str]) -> str:
        return "  ".join(str(c).ljust(widths[i]) for i, c in enumerate(cells))

    typer.echo(fmt(headers))
    for row in rows:
        typer.echo(fmt(row))


@app.command("list")
def list_tasks(json_: bool = typer.Option(False, "--json")) -> None:
    """Table of all tasks (valid and invalid files). The '#' column is a
    shortcut you can pass to `sched run` instead of the task id."""
    cfg = _load_cfg()
    valid, invalid = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
    numbered = _numbered_tasks(valid)
    conn = _open_store(cfg)
    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)

    entries = []
    for i, task in enumerate(numbered, start=1):
        runs = store_mod.list_runs(conn, task.id, limit=1)
        last = runs[0] if runs else None
        next_run = None
        if task.enabled:
            if task.schedule:
                next_run = schedule_mod.next_run(task.schedule, now, tz).strftime(
                    "%Y-%m-%d %H:%M"
                )
            else:
                next_run = f"once@{task.fire_at}"
        entries.append(
            {
                "number": i,
                "id": task.id,
                "schedule": task.schedule or f"once@{task.fire_at}",
                "enabled": task.enabled,
                "last_run": last["started_at"] if last else None,
                "status": last["status"] if last else None,
                "next_run": next_run,
            }
        )

    if json_:
        out = {
            "tasks": entries,
            "invalid": [{"path": str(p), "error": e} for p, e in invalid],
        }
        typer.echo(json.dumps(out, indent=2))
        return

    if not entries and not invalid:
        typer.echo("No tasks found.")
        return

    headers = ["#", "ID", "SCHEDULE", "ENABLED", "LAST RUN", "STATUS", "NEXT RUN"]
    rows = [
        [
            str(e["number"]),
            e["id"],
            e["schedule"],
            "yes" if e["enabled"] else "no",
            e["last_run"] or "-",
            e["status"] or "-",
            e["next_run"] or "-",
        ]
        for e in entries
    ]
    for path, error in invalid:
        rows.append(["-", path.name, "-", "-", f"INVALID: {error}", "-", "-"])
    _print_table(headers, rows)


@app.command("add")
def add_task(
    file: Optional[str] = typer.Option(None, "--file"),
    nl: Optional[str] = typer.Option(None, "--nl"),
    yes: bool = typer.Option(False, "--yes"),
) -> None:
    """Add a task from a file or a natural-language sentence."""
    if bool(file) == bool(nl):
        typer.echo("Error: pass exactly one of --file or --nl.", err=True)
        raise typer.Exit(1)

    cfg = _load_cfg()
    tasks_dir = _tasks_dir(cfg)

    if nl:
        try:
            task = nl_to_task(nl, cfg)
        except PlannerError as e:
            typer.echo(f"Error: {e}", err=True)
            raise typer.Exit(1)

        existing, _ = tasks_mod.load_tasks_dir(tasks_dir)
        if any(t.id == task.id for t in existing):
            typer.echo(
                f"Error: a task with id '{task.id}' already exists.", err=True
            )
            raise typer.Exit(1)

        dest = tasks_dir / f"{task.id}.md"
        task = dataclasses.replace(task, path=dest)
        typer.echo(tasks_mod.render_task_file(task))

        if not yes and not typer.confirm(f"Save this task to {dest}?"):
            typer.echo("Aborted.")
            raise typer.Exit(0)

        tasks_dir.mkdir(parents=True, exist_ok=True)
        tasks_mod.write_task_file(task)
        typer.echo(f"Added task '{task.id}' -> {dest}")
        return

    src = Path(file)
    if not src.exists():
        typer.echo(f"Error: file not found: {src}", err=True)
        raise typer.Exit(1)

    try:
        task = tasks_mod.parse_task_file(src)
    except tasks_mod.TaskValidationError as e:
        typer.echo(f"Error: invalid task file: {e}", err=True)
        raise typer.Exit(1)

    existing, _ = tasks_mod.load_tasks_dir(tasks_dir)
    existing_ids = {t.id: t.path for t in existing}
    if task.id in existing_ids:
        if existing_ids[task.id].resolve() == src.resolve():
            typer.echo(f"Task '{task.id}' already present at {src}.")
            return
        typer.echo(
            f"Error: a task with id '{task.id}' already exists "
            f"({existing_ids[task.id]}).",
            err=True,
        )
        raise typer.Exit(1)

    tasks_dir.mkdir(parents=True, exist_ok=True)
    dest = tasks_dir / src.name
    if dest.resolve() != src.resolve():
        shutil.copyfile(src, dest)
    typer.echo(f"Added task '{task.id}' -> {dest}")


@app.command("show")
def show_task(task_id: str) -> None:
    """Print the task file verbatim plus computed schedule/run info. Accepts
    a task id or its '#' number from `sched list`."""
    cfg = _load_cfg()
    valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
    task = _resolve_task_ref(task_id, valid)
    if task is None:
        typer.echo(f"Error: no task matching '{task_id}' (id or number)", err=True)
        raise typer.Exit(1)

    typer.echo(task.path.read_text())

    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)
    if not task.enabled:
        typer.echo("Next runs: task is disabled")
    elif task.schedule:
        cursor = now
        upcoming = []
        for _ in range(3):
            cursor = schedule_mod.next_run(task.schedule, cursor, tz)
            upcoming.append(cursor.strftime("%Y-%m-%d %H:%M %Z"))
        typer.echo("Next runs: " + ", ".join(upcoming))
    else:
        typer.echo(f"Next run: once at {task.fire_at}")

    conn = _open_store(cfg)
    runs = store_mod.list_runs(conn, task_id, limit=3)
    if runs:
        typer.echo("Last runs:")
        for r in runs:
            typer.echo(
                f"  #{r['run_id']} {r['started_at']} {r['status']} "
                f"{r['report_path'] or ''}"
            )
    else:
        typer.echo("Last runs: none yet")


def _set_enabled(task_id: str, enabled: bool) -> None:
    cfg = _load_cfg()
    valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
    task = _resolve_task_ref(task_id, valid)
    if task is None:
        typer.echo(f"Error: no task matching '{task_id}' (id or number)", err=True)
        raise typer.Exit(1)

    tasks_mod.set_enabled(task.path, enabled)
    conn = _open_store(cfg)
    next_run_at = None
    if enabled:
        tz = schedule_mod.resolve_tz(cfg)
        next_run_at = daemon_mod.first_next_run(task, tz, datetime.now(tz)).isoformat()
    store_mod.upsert_task_state(
        conn, task.id, task.path, tasks_mod.content_hash(task.path), enabled,
        next_run_at=next_run_at,
    )
    if not enabled:
        store_mod.set_enabled_state(conn, task.id, False, clear_next_run=True)
    typer.echo(f"Task '{task.id}' {'enabled' if enabled else 'disabled'}.")


@app.command("enable")
def enable_task(task_id: str) -> None:
    _set_enabled(task_id, True)


@app.command("disable")
def disable_task(task_id: str) -> None:
    _set_enabled(task_id, False)


@app.command("rm")
def remove_task(
    task_id: str, yes: bool = typer.Option(False, "--yes")
) -> None:
    cfg = _load_cfg()
    valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
    task = _resolve_task_ref(task_id, valid)
    if task is None:
        typer.echo(f"Error: no task matching '{task_id}' (id or number)", err=True)
        raise typer.Exit(1)

    if not yes and not typer.confirm(f"Delete task file {task.path}?"):
        typer.echo("Aborted.")
        raise typer.Exit(0)

    task.path.unlink()
    conn = _open_store(cfg)
    store_mod.delete_task_state(conn, task.id)
    typer.echo(f"Removed task '{task.id}' (run history kept).")


@app.command("logs")
def show_logs(
    task_id: str,
    last: int = typer.Option(10, "--last"),
    errors: bool = typer.Option(False, "--errors"),
    json_: bool = typer.Option(False, "--json"),
) -> None:
    cfg = _load_cfg()
    # A number resolves against currently-valid tasks; a plain id passes
    # through unchanged so history for a since-removed task is still
    # reachable (run history outlives the task file, per API.md `rm`).
    resolved_id = task_id
    if task_id.isdigit():
        valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
        numbered = _numbered_tasks(valid)
        idx = int(task_id)
        if 1 <= idx <= len(numbered):
            resolved_id = numbered[idx - 1].id

    conn = _open_store(cfg)
    rows = store_mod.list_runs(conn, resolved_id, limit=last, errors_only=errors)

    if json_:
        typer.echo(json.dumps([dict(r) for r in rows], indent=2))
        return

    if not rows:
        typer.echo(f"No runs recorded for '{resolved_id}'.")
        return

    headers = ["RUN", "TRIGGER", "STARTED", "DURATION", "STATUS", "REPORT"]
    table_rows = []
    for r in rows:
        duration = "-"
        if r["finished_at"]:
            start = datetime.fromisoformat(r["started_at"])
            end = datetime.fromisoformat(r["finished_at"])
            duration = report_mod.format_duration(end - start)
        table_rows.append(
            [
                str(r["run_id"]),
                r["trigger"],
                r["started_at"],
                duration,
                r["status"],
                r["report_path"] or "-",
            ]
        )
    _print_table(headers, table_rows)


def _run_one(
    cfg: Config, conn, task: tasks_mod.Task, tz, now: datetime
) -> tuple[str, str | None]:
    """Execute one task with live progress printed to the terminal, then
    print the finished report (or the failure) before returning."""
    store_mod.upsert_task_state(
        conn, task.id, task.path, tasks_mod.content_hash(task.path), task.enabled
    )

    typer.echo(f"=== [{task.id}] starting (model={task.model or cfg.ollama.model}) ===")

    def on_event(text: str) -> None:
        typer.echo(f"[{task.id}] {text}")

    def executor_with_events(t, c):
        return agent_run_task(t, c, on_event=on_event)

    status, report_path = daemon_mod.execute_and_record(
        conn, task, cfg, tz, now, "manual", executor_with_events
    )

    if status == "success":
        typer.echo(f"[{task.id}] done -- report: {report_path}")
        typer.echo(f"----- {task.id} report -----")
        typer.echo(Path(report_path).read_text().rstrip())
        typer.echo(f"----- end {task.id} report -----")
    else:
        typer.echo(
            f"[{task.id}] FAILED (status={status}) -- see `sched logs {task.id}`",
            err=True,
        )
    return status, report_path


@app.command("run")
def run_task(
    ref: str = typer.Argument(
        ..., metavar="TASK", help="A task id, its '#' number from `sched list`, or 'all'."
    ),
    model: Optional[str] = typer.Option(None, "--model"),
) -> None:
    """Execute one task, or every task ('all'), immediately (trigger=manual)."""
    cfg = _load_cfg()
    valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))

    typer.echo("Checking Ollama...")
    if not daemon_mod.ensure_ollama_running(cfg):
        typer.echo(
            f"Error: could not reach or start Ollama at {cfg.ollama.host}. "
            "Start it manually (`ollama serve` or the Ollama app) and try again.",
            err=True,
        )
        raise typer.Exit(2)

    conn = _open_store(cfg)
    tz = schedule_mod.resolve_tz(cfg)

    if ref.lower() == "all":
        numbered = _numbered_tasks(valid)
        if not numbered:
            typer.echo("No tasks found.")
            raise typer.Exit(0)
        results = []
        for task in numbered:
            if model:
                task = dataclasses.replace(task, model=model)
            status, _ = _run_one(cfg, conn, task, tz, datetime.now(tz))
            results.append((task.id, status))
        conn.close()

        succeeded = sum(1 for _, s in results if s == "success")
        typer.echo(f"\n{succeeded}/{len(results)} task(s) succeeded.")
        failed = [tid for tid, s in results if s != "success"]
        if failed:
            typer.echo(f"Failed: {', '.join(failed)}", err=True)
            raise typer.Exit(2)
        return

    task = _resolve_task_ref(ref, valid)
    if task is None:
        typer.echo(f"Error: no task matching '{ref}' (id, number, or 'all')", err=True)
        raise typer.Exit(1)
    if model:
        task = dataclasses.replace(task, model=model)

    status, _ = _run_one(cfg, conn, task, tz, datetime.now(tz))
    conn.close()

    if status != "success":
        raise typer.Exit(2)


@app.command("daemon")
def daemon(once: bool = typer.Option(False, "--once")) -> None:
    """Foreground tick loop."""
    cfg = _load_cfg()
    if once:
        daemon_mod.run_once(cfg, executor=agent_run_task, ensure_ollama=True)
    else:
        # Mirror logs to stderr in foreground mode only (Architecture.md §7);
        # --once is typically cron/launchd-driven with no one watching.
        stream_handler = logging.StreamHandler()
        stream_handler.setFormatter(logging.Formatter(_LOG_FORMAT))
        logging.getLogger("schedlm").addHandler(stream_handler)
        daemon_mod.run_forever(
            cfg, executor=agent_run_task, ensure_ollama=True, config_path=CONFIG_PATH
        )


@app.command("serve")
def serve(
    host: str = typer.Option("127.0.0.1", "--host"),
    port: int = typer.Option(8787, "--port"),
) -> None:
    """Web UI + API to list/add/edit/remove/run tasks from a browser."""
    import uvicorn

    from schedlm.webapi import create_app

    cfg = _load_cfg()
    typer.echo(
        f"Serving sched web UI at http://{host}:{port} (default model: {cfg.ollama.model})"
    )
    web_app = create_app(cfg, config_path=CONFIG_PATH)
    uvicorn.run(web_app, host=host, port=port, log_level="info")


monitor_app = typer.Typer(no_args_is_help=True)
app.add_typer(
    monitor_app,
    name="monitor",
    help="Check that Ollama, the daemon and other local agents are up; restart them if not.",
)


def _print_statuses(rows: list[tuple[bool, str, str]]) -> None:
    _print_table(
        ["", "SERVICE", "STATUS"],
        [["ok" if ok else "DOWN", name, detail] for ok, name, detail in rows],
    )


@monitor_app.command("check")
def monitor_check() -> None:
    """Check every service once and print the result; exit 1 if any is down."""
    from schedlm import monitor as monitor_mod

    statuses = monitor_mod.check_all(monitor_mod.services_for(_load_cfg()))
    _print_statuses([(s.ok, s.name, s.detail) for s in statuses])
    raise typer.Exit(0 if all(s.ok for s in statuses) else 1)


@monitor_app.command("heal")
def monitor_heal(
    quiet: bool = typer.Option(False, "--quiet", help="Print nothing if everything is up."),
) -> None:
    """Check every service and restart the ones that are down (what the
    local.agent-monitor.heal login agent runs every 10 minutes)."""
    from schedlm import monitor as monitor_mod

    summary = monitor_mod.heal_all(_load_cfg())
    rows = []
    for r in summary["results"]:
        if r["ok"]:
            rows.append((True, r["name"], r["detail"]))
        else:
            what = f"{r['detail']} -> {r['action']} -> {r['detail_after']}"
            rows.append((r["ok_after"], r["name"], what))
    if not quiet or not all(r["ok"] for r in summary["results"]):
        typer.echo(summary["ran_at"])
        _print_statuses(rows)
    raise typer.Exit(0 if summary["all_ok"] else 1)


@monitor_app.command("serve")
def monitor_serve(
    host: str = typer.Option("127.0.0.1", "--host"),
    port: Optional[int] = typer.Option(None, "--port", help="Default: monitor.port (8790)."),
) -> None:
    """Status page for the monitored services (what the local.agent-monitor
    login agent runs)."""
    from schedlm import monitor as monitor_mod

    cfg = _load_cfg()
    typer.echo(f"Serving agent monitor at http://{host}:{port or cfg.monitor.port}")
    monitor_mod.serve(CONFIG_PATH, host=host, port=port)


if __name__ == "__main__":
    app()
