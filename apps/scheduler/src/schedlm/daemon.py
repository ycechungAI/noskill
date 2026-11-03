"""Tick loop, catch-up, overlap guard. See Architecture.md §4."""

from __future__ import annotations

import concurrent.futures
import logging
import os
import shutil
import subprocess
import time
from datetime import datetime, tzinfo
from pathlib import Path
from typing import Callable

import ollama

from schedlm import report as report_mod
from schedlm import schedule as schedule_mod
from schedlm import store as store_mod
from schedlm import tasks as tasks_mod
from schedlm.config import Config, ConfigWatcher

logger = logging.getLogger(__name__)

Executor = Callable[[tasks_mod.Task, Config], report_mod.RunResult]

OLLAMA_START_TIMEOUT_SECONDS = 15.0


def stub_execute(task: tasks_mod.Task, cfg: Config) -> report_mod.RunResult:
    """Placeholder executor, kept for tests/dev. Real usage (cli.py) passes
    agent.executor.run_task instead -- see cli.py's `run`/`daemon` commands."""
    return report_mod.RunResult(
        report=f"# {task.name}\n\nhello from the stub executor.\n",
        iterations=0,
        tool_calls=0,
        model=task.model or cfg.ollama.model,
    )


# Where the Ollama installers put the binary. launchd/cron start processes
# with PATH=/usr/bin:/bin, which has none of them.
OLLAMA_BINARY_CANDIDATES = (
    "/usr/local/bin/ollama",
    "/opt/homebrew/bin/ollama",
    "/Applications/Ollama.app/Contents/Resources/ollama",
)


def find_ollama_binary() -> str | None:
    found = shutil.which("ollama")
    if found:
        return found
    return next((p for p in OLLAMA_BINARY_CANDIDATES if os.access(p, os.X_OK)), None)


def ensure_ollama_running(cfg: Config) -> bool:
    """Check that Ollama is reachable at cfg.ollama.host; if not, try to
    start it headlessly (`ollama serve`) and wait briefly for it to come up.

    Safe to call before every run: if something (the Ollama.app menu-bar
    app, `ollama serve`, brew services, ...) is already listening, this is
    just one fast reachability check and nothing is spawned. Returns True
    once Ollama is reachable, False if it could not be reached or started
    (in which case the caller's own Ollama call will fail with a clear
    ConnectionError, per Architecture.md §6).
    """
    client = ollama.Client(host=cfg.ollama.host)
    try:
        client.list()
        return True
    except Exception:
        pass

    logger.info("Ollama not reachable at %s -- attempting to start it", cfg.ollama.host)
    binary = find_ollama_binary()
    if binary is None:
        logger.error(
            "`ollama` binary not found on PATH or in the usual install "
            "locations; cannot auto-start it"
        )
        return False
    log_path = Path(cfg.paths.data) / "ollama-serve.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        with open(log_path, "a") as log_file:
            subprocess.Popen(
                [binary, "serve"],
                stdout=log_file,
                stderr=log_file,
                start_new_session=True,
            )
    except FileNotFoundError:
        logger.error("`ollama` binary not found on PATH; cannot auto-start it")
        return False
    except OSError as e:
        logger.error("failed to launch `ollama serve`: %s", e)
        return False

    deadline = time.monotonic() + OLLAMA_START_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        try:
            client.list()
            logger.info("Ollama is now reachable at %s", cfg.ollama.host)
            return True
        except Exception:
            time.sleep(0.5)

    logger.error(
        "Ollama did not become reachable within %.0fs of starting it",
        OLLAMA_START_TIMEOUT_SECONDS,
    )
    return False


class ExecutionTimeout(Exception):
    """Raised when an executor exceeds task.timeout_minutes (spec FR-11)."""


def _run_with_timeout(
    executor: Executor, task: tasks_mod.Task, cfg: Config
) -> report_mod.RunResult:
    """Run executor(task, cfg) with a wall-clock timeout (Architecture.md §5).

    On timeout the worker thread is deliberately abandoned rather than
    joined -- it either finishes harmlessly in the background (its result
    is discarded) or dies with the process. Joining it here would block on
    exactly the hang we're trying to time out on.
    """
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    future = pool.submit(executor, task, cfg)
    try:
        result = future.result(timeout=task.timeout_minutes * 60)
    except concurrent.futures.TimeoutError as e:
        raise ExecutionTimeout(
            f"exceeded timeout_minutes={task.timeout_minutes}"
        ) from e
    else:
        pool.shutdown(wait=False)
        return result


def first_next_run(task: tasks_mod.Task, tz: tzinfo, now: datetime) -> datetime:
    """When a newly added or re-enabled task should first fire: its next cron
    slot after now, or its `fire_at` time for a one-time task."""
    if task.schedule:
        return schedule_mod.next_run(task.schedule, now, tz)
    next_dt = datetime.fromisoformat(task.fire_at)
    if next_dt.tzinfo is None:
        next_dt = next_dt.replace(tzinfo=tz)
    return next_dt


def sync_task_state(
    conn, tasks_dir: Path, tz: tzinfo, now: datetime
) -> None:
    """Reload task files whose content changed; upsert into task_state.

    New tasks get next_run_at computed from now (Architecture.md §4), and so
    do re-enabled ones, which never fire a slot stored before they were
    disabled. An enabled task with no next_run_at (left by older versions'
    disable-then-enable) gets one too, or tick() would skip it forever.
    Task-state rows whose backing file has been deleted are dropped (history
    rows in `runs` are untouched). Invalid files are logged and skipped
    (spec FR-3) -- the daemon never crashes because one task file is broken.
    """
    valid, invalid = tasks_mod.load_tasks_dir(tasks_dir)
    for path, error in invalid:
        logger.warning("skipping invalid task file %s: %s", path, error)

    existing_rows = {row["id"]: row for row in store_mod.list_task_states(conn)}

    for task_id, row in existing_rows.items():
        if not Path(row["path"]).exists():
            logger.info("task '%s' file removed, dropping from schedule", task_id)
            store_mod.delete_task_state(conn, task_id)

    for task in valid:
        h = tasks_mod.content_hash(task.path)
        row = existing_rows.get(task.id)
        if row is None:
            next_dt = first_next_run(task, tz, now)
            store_mod.upsert_task_state(
                conn, task.id, task.path, h, task.enabled, next_run_at=next_dt.isoformat()
            )
        elif task.enabled and (not row["enabled"] or row["next_run_at"] is None):
            next_dt = first_next_run(task, tz, now)
            logger.info("task '%s' enabled; next run %s", task.id, next_dt.isoformat())
            store_mod.upsert_task_state(
                conn, task.id, task.path, h, True, next_run_at=next_dt.isoformat()
            )
        elif (
            row["content_hash"] != h
            or bool(row["enabled"]) != task.enabled
            or row["path"] != str(task.path)
        ):
            store_mod.upsert_task_state(conn, task.id, task.path, h, task.enabled)


def _advance_next_run_or_disable(conn, task: tasks_mod.Task, tz: tzinfo, now: datetime) -> None:
    """Persist the task's forward-looking state *before* executing it, so a
    crash mid-run can never cause a double-fire (Architecture.md §5)."""
    h = tasks_mod.content_hash(task.path)
    if task.schedule:
        nxt = schedule_mod.next_run(task.schedule, now, tz)
        store_mod.upsert_task_state(
            conn, task.id, task.path, h, task.enabled, next_run_at=nxt.isoformat()
        )
    else:
        # one-time task: auto-disable after firing (spec FR-6)
        tasks_mod.set_enabled(task.path, False)
        store_mod.upsert_task_state(conn, task.id, task.path, h, False)
        store_mod.set_enabled_state(conn, task.id, False, clear_next_run=True)


def execute_and_record(
    conn,
    task: tasks_mod.Task,
    cfg: Config,
    tz: tzinfo,
    now: datetime,
    trigger: str,
    executor: Executor,
) -> tuple[str, str | None]:
    """Run one task and record the outcome. Never raises -- a failing task
    must not crash the daemon (spec FR-3 style guarantee for execution)."""
    started_iso = now.isoformat()
    model = task.model or cfg.ollama.model
    run_id = store_mod.start_run(conn, task.id, trigger, started_iso, model=model)
    conn.execute(
        "UPDATE task_state SET running_pid=? WHERE id=?", (os.getpid(), task.id)
    )
    conn.commit()
    logger.info("[%s] run started (trigger=%s)", task.id, trigger)
    try:
        result = _run_with_timeout(executor, task, cfg)
    except ExecutionTimeout as e:
        finished = datetime.now(tz)
        logger.warning("[%s] run timed out: %s", task.id, e)
        store_mod.finish_run(
            conn, run_id, status="timeout", finished_at=finished.isoformat(), error=str(e)
        )
        return "timeout", None
    except ConnectionError as e:
        # Ollama unreachable (Architecture.md §6): the ollama client already
        # raises a plain, actionable ConnectionError -- add the configured
        # host so a config.yaml typo is easy to spot.
        finished = datetime.now(tz)
        message = f"cannot reach Ollama at {cfg.ollama.host}: {e}"
        logger.error("[%s] %s", task.id, message)
        store_mod.finish_run(
            conn, run_id, status="error", finished_at=finished.isoformat(), error=message
        )
        return "error", None
    except ollama.ResponseError as e:
        # Model not pulled / other Ollama-side error (Architecture.md §6):
        # store Ollama's own error text; log a `ollama pull` hint if it
        # looks like a missing-model error.
        finished = datetime.now(tz)
        error_text = e.error or str(e)
        logger.error("[%s] Ollama error: %s", task.id, error_text)
        if "not found" in error_text.lower():
            logger.warning("[%s] hint: try `ollama pull %s`", task.id, model)
        store_mod.finish_run(
            conn, run_id, status="error", finished_at=finished.isoformat(), error=error_text
        )
        return "error", None
    except Exception as e:  # noqa: BLE001 - tasks must never crash the daemon
        finished = datetime.now(tz)
        logger.exception("[%s] run failed", task.id)
        store_mod.finish_run(
            conn, run_id, status="error", finished_at=finished.isoformat(), error=str(e)
        )
        return "error", None
    else:
        # Executor succeeded; writing the report is a separate failure mode
        # (Architecture.md §6 "Report dir unwritable") -- if it fails, the
        # generated content is still dumped into the DB error column so
        # nothing the model produced is lost.
        try:
            finished = datetime.now(tz)
            report_path = report_mod.write_report(
                task.id,
                run_id,
                trigger,
                now,
                finished,
                "success",
                result,
                task.output,
                default_runs_dir=Path(cfg.paths.runs),
            )
        except Exception as e:  # noqa: BLE001
            finished = datetime.now(tz)
            logger.exception("[%s] failed to write report", task.id)
            error_text = f"failed to write report: {e}\n\n--- report content ---\n{result.report}"
            store_mod.finish_run(
                conn, run_id, status="error", finished_at=finished.isoformat(), error=error_text
            )
            return "error", None
        store_mod.finish_run(
            conn,
            run_id,
            status=result.status,
            finished_at=finished.isoformat(),
            report_path=str(report_path),
            iterations=result.iterations,
            tool_calls=result.tool_calls,
            model=result.model,
        )
        logger.info(
            "[%s] run finished status=%s model=%s report=%s",
            task.id,
            result.status,
            result.model,
            report_path,
        )
        return result.status, str(report_path)
    finally:
        conn.execute(
            "UPDATE task_state SET running_pid=NULL, last_run_at=? WHERE id=?",
            (datetime.now(tz).isoformat(), task.id),
        )
        conn.commit()


def recover_crashed_runs(
    cfg: Config, tasks_dir: Path | None = None, db_path: Path | None = None
) -> None:
    """Mark any run left 'running' by a previous, now-dead process as 'error'.
    Cheap and idempotent; safe to call before every kind of invocation."""
    db_path = db_path or (Path(cfg.paths.data) / "sched.db")
    conn = store_mod.init_db(db_path)
    store_mod.recover_running_state(conn)
    conn.close()


def startup_catchup(
    cfg: Config,
    tasks_dir: Path | None = None,
    db_path: Path | None = None,
    executor: Executor = stub_execute,
) -> None:
    """Run once when the long-running `sched daemon` (no --once) launches
    (Architecture.md §4 Startup/catch-up): recover crashed runs, then for
    anything stale, either fire it once (catchup) or skip forward to its
    next future slot.

    NOT used by `sched daemon --once`: that command is meant to work as a
    cron-replacement (API.md), invoked repeatedly and often, so it must
    fire ordinary due tasks unconditionally on every invocation regardless
    of their `catchup` setting -- see run_once(). The catchup gate here
    exists specifically to avoid mass-firing a large backlog the moment a
    long-running daemon wakes up after being down/asleep for a long time;
    it has no equivalent meaning for a process that only ever runs once.
    """
    tasks_dir = tasks_dir or Path(cfg.paths.tasks)
    db_path = db_path or (Path(cfg.paths.data) / "sched.db")
    conn = store_mod.init_db(db_path)
    store_mod.recover_running_state(conn)

    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)
    sync_task_state(conn, tasks_dir, tz, now)

    for row in store_mod.list_task_states(conn):
        if not row["enabled"] or row["next_run_at"] is None:
            continue
        next_dt = datetime.fromisoformat(row["next_run_at"])
        if next_dt > now:
            continue
        try:
            task = tasks_mod.parse_task_file(Path(row["path"]))
        except tasks_mod.TaskValidationError:
            continue

        should_fire = schedule_mod.catchup_decision(
            next_dt, now, task.catchup, cfg.daemon.catchup_grace_minutes
        )
        # Persist the forward-looking state before executing (crash safety),
        # matching the ordering used in tick()'s regular due-firing path.
        _advance_next_run_or_disable(conn, task, tz, now)
        if should_fire:
            execute_and_record(conn, task, cfg, tz, now, "catchup", executor)

    conn.close()


def tick(
    cfg: Config,
    tasks_dir: Path | None = None,
    db_path: Path | None = None,
    executor: Executor = stub_execute,
) -> None:
    """One ordinary tick: sync task files, fire anything on-time due, guard
    against overlap. Assumes startup() has already resolved stale backlog."""
    tasks_dir = tasks_dir or Path(cfg.paths.tasks)
    db_path = db_path or (Path(cfg.paths.data) / "sched.db")
    conn = store_mod.init_db(db_path)

    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)
    sync_task_state(conn, tasks_dir, tz, now)

    fired_any = False
    for row in store_mod.list_task_states(conn):
        if not row["enabled"] or row["next_run_at"] is None:
            continue
        next_dt = datetime.fromisoformat(row["next_run_at"])
        if next_dt > now:
            continue
        try:
            task = tasks_mod.parse_task_file(Path(row["path"]))
        except tasks_mod.TaskValidationError as e:
            logger.warning(
                "task '%s' became invalid, skipping this tick: %s", row["id"], e
            )
            continue

        if row["running_pid"] is not None:
            logger.warning(
                "task '%s': previous run still in progress, skipping this slot "
                "(overlap protection, spec FR-8)",
                task.id,
            )
            _advance_next_run_or_disable(conn, task, tz, now)
            run_id = store_mod.start_run(
                conn, task.id, "schedule", now.isoformat(), model=task.model or cfg.ollama.model
            )
            store_mod.finish_run(
                conn, run_id, status="skipped_overlap", finished_at=now.isoformat()
            )
            fired_any = True
            continue

        _advance_next_run_or_disable(conn, task, tz, now)
        execute_and_record(conn, task, cfg, tz, now, "schedule", executor)
        fired_any = True

    if fired_any:
        logger.info("tick complete at %s", now.isoformat())
    conn.close()


def run_once(
    cfg: Config,
    tasks_dir: Path | None = None,
    db_path: Path | None = None,
    executor: Executor = stub_execute,
    ensure_ollama: bool = False,
) -> None:
    """A single ordinary tick (`sched daemon --once`): recover crashed runs,
    then fire whatever is due unconditionally. Deliberately does NOT go
    through startup_catchup()'s catchup gate, so this works correctly when
    invoked repeatedly via system cron as a lightweight daemon replacement
    (API.md) -- see startup_catchup()'s docstring for why.

    `ensure_ollama` defaults to False so tests using stub_execute never touch
    real Ollama; cli.py passes True for real usage."""
    if ensure_ollama:
        ensure_ollama_running(cfg)
    recover_crashed_runs(cfg, tasks_dir, db_path)
    tick(cfg, tasks_dir, db_path, executor)


def run_forever(
    cfg: Config,
    tasks_dir: Path | None = None,
    db_path: Path | None = None,
    executor: Executor = stub_execute,
    ensure_ollama: bool = False,
    config_path: Path | None = None,
) -> None:
    """`config_path`, if given, is re-read whenever it changes, so e.g. a
    default model switched from the web UI applies from the next tick."""
    watcher = ConfigWatcher(config_path, cfg) if config_path is not None else None
    if ensure_ollama:
        ensure_ollama_running(cfg)
    startup_catchup(cfg, tasks_dir, db_path, executor)
    logger.info(
        "daemon started (tick_seconds=%s, default model=%s)",
        cfg.daemon.tick_seconds,
        cfg.ollama.model,
    )
    try:
        while True:
            if watcher is not None:
                cfg = watcher.current()
            tick(cfg, tasks_dir, db_path, executor)
            time.sleep(cfg.daemon.tick_seconds)
    except KeyboardInterrupt:
        logger.info("daemon shutting down (SIGINT)")
