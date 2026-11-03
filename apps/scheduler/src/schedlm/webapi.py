"""FastAPI app backing `sched serve` -- browser UI to list/add/edit/remove/run
tasks. See Architecture.md §5 "Web UI" and API.md `sched serve`.

Tasks are addressed by **filename** for file-level operations (read/create/
update/delete) so a broken (invalid) task file can still be opened and fixed
through the UI, and by **task id** for operations that need a valid parsed
Task (enable/disable/run/runs) -- mirrors cli.py's split between raw file
access and id-based commands.
"""

from __future__ import annotations

import dataclasses
import json
import queue
import re
import threading
import uuid
from datetime import datetime
from pathlib import Path
from typing import Iterator

import ollama
from fastapi import FastAPI, HTTPException
from fastapi.responses import HTMLResponse, PlainTextResponse, StreamingResponse
from pydantic import BaseModel

from schedlm import config as config_mod
from schedlm import daemon as daemon_mod
from schedlm import models as models_mod
from schedlm import schedule as schedule_mod
from schedlm import store as store_mod
from schedlm import tasks as tasks_mod
from schedlm.agent import planner as planner_mod
from schedlm.agent.executor import run_task as agent_run_task
from schedlm.config import Config

STATIC_DIR = Path(__file__).parent / "web" / "static"

_FILENAME_RE = re.compile(r"^[^/\\]+\.md$")

# token -> Queue of {"event": str} | {"done": True, "status": str, "report_path": str|None}
_RUN_QUEUES: dict[str, "queue.Queue"] = {}


class CreateTaskBody(BaseModel):
    content: str
    filename: str | None = None


class DraftTaskBody(BaseModel):
    text: str
    draft: str | None = None  # the task file so far, when this is a follow-up


class UpdateTaskBody(BaseModel):
    content: str


class ModelBody(BaseModel):
    model: str | None = None  # None/"" on a task = use config.yaml's default


def draft_request(text: str, draft: str | None) -> str:
    """The sentence for the planner: the person's words, plus the current draft
    when they're asking for a change to it ("make it 8am instead")."""
    text = text.strip()
    if not draft or not draft.strip():
        return text
    return (
        "Here is the current task file:\n\n"
        + draft.strip()
        + "\n\nChange it as follows, and keep everything this doesn't mention: "
        + text
        + "\nA repeating task stays repeating (a cron `schedule`, not `fire_at`) "
        "unless the change asks for a one-off run."
    )


def _free_id(task_id: str, taken: set[str]) -> str:
    """`task_id`, or `task_id-2`, `-3`... when a task already uses it."""
    if task_id not in taken:
        return task_id
    n = 2
    while f"{task_id}-{n}" in taken:
        n += 1
    return f"{task_id}-{n}"


def _tasks_dir(cfg: Config) -> Path:
    return Path(cfg.paths.tasks)


def _db_path(cfg: Config) -> Path:
    return Path(cfg.paths.data) / "sched.db"


def _open_store(cfg: Config):
    conn = store_mod.init_db(_db_path(cfg))
    store_mod.recover_running_state(conn)
    return conn


def _numbered_tasks(valid: list[tasks_mod.Task]) -> list[tasks_mod.Task]:
    return sorted(valid, key=lambda t: t.id)


def _safe_task_path(cfg: Config, filename: str) -> Path:
    if not _FILENAME_RE.match(filename) or ".." in filename:
        raise HTTPException(400, "invalid filename")
    return _tasks_dir(cfg) / filename


def _sync(cfg: Config) -> None:
    tasks_dir = _tasks_dir(cfg)
    conn = _open_store(cfg)
    tz = schedule_mod.resolve_tz(cfg)
    daemon_mod.sync_task_state(conn, tasks_dir, tz, datetime.now(tz))
    conn.close()


def _set_enabled(cfg: Config, task_id: str, enabled: bool) -> dict:
    valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
    task = next((t for t in valid if t.id == task_id), None)
    if task is None:
        raise HTTPException(404, f"no task '{task_id}'")
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
    conn.close()
    return {"id": task.id, "enabled": enabled}


def _task_summary(conn, task: tasks_mod.Task, number: int, tz, now: datetime) -> dict:
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
    return {
        "number": number,
        "id": task.id,
        "name": task.name,
        "filename": task.path.name,
        "schedule": task.schedule or f"once@{task.fire_at}",
        "enabled": task.enabled,
        "last_run": last["started_at"] if last else None,
        "status": last["status"] if last else None,
        "last_model": last["model"] if last else None,
        "model": task.model,  # per-task override; None = config default
        "next_run": next_run,
    }


def create_app(cfg: Config, config_path: Path | None = None) -> FastAPI:
    """`config_path`, if given, is re-read when it changes and is where
    PUT /api/models/default saves the new default model."""
    app = FastAPI(title="sched web UI")
    watcher = config_mod.ConfigWatcher(config_path, cfg) if config_path is not None else None

    def current() -> Config:
        return watcher.current() if watcher is not None else cfg

    @app.get("/", response_class=HTMLResponse)
    def index() -> str:
        return (STATIC_DIR / "index.html").read_text()

    @app.get("/api/tasks")
    def list_tasks() -> dict:
        cfg = current()
        tasks_dir = _tasks_dir(cfg)
        valid, invalid = tasks_mod.load_tasks_dir(tasks_dir)
        numbered = _numbered_tasks(valid)
        conn = _open_store(cfg)
        tz = schedule_mod.resolve_tz(cfg)
        now = datetime.now(tz)
        entries = [
            _task_summary(conn, t, i, tz, now) for i, t in enumerate(numbered, start=1)
        ]
        conn.close()
        return {
            "tasks": entries,
            "invalid": [{"filename": p.name, "error": e} for p, e in invalid],
        }

    @app.get("/api/tasks/file/{filename}")
    def get_task_file(filename: str) -> dict:
        cfg = current()
        path = _safe_task_path(cfg, filename)
        if not path.exists():
            raise HTTPException(404, f"no such file: {filename}")
        return {"filename": filename, "content": path.read_text()}

    @app.post("/api/tasks/file", status_code=201)
    def create_task_file(body: CreateTaskBody) -> dict:
        cfg = current()
        tasks_dir = _tasks_dir(cfg)
        placeholder = tasks_dir / (body.filename or "<new>.md")
        try:
            task = tasks_mod.parse_task_text(body.content, placeholder)
        except tasks_mod.TaskValidationError as e:
            raise HTTPException(400, str(e)) from e

        filename = body.filename or f"{task.id}.md"
        if not _FILENAME_RE.match(filename):
            raise HTTPException(
                400, "filename must be a bare '<name>.md', no path separators"
            )
        dest = tasks_dir / filename
        if dest.exists():
            raise HTTPException(409, f"file already exists: {filename}")

        existing, _ = tasks_mod.load_tasks_dir(tasks_dir)
        if any(t.id == task.id for t in existing):
            raise HTTPException(409, f"a task with id '{task.id}' already exists")

        tasks_dir.mkdir(parents=True, exist_ok=True)
        dest.write_text(body.content)
        _sync(cfg)
        return {"filename": filename, "id": task.id}

    @app.post("/api/tasks/draft")
    def draft_task(body: DraftTaskBody) -> dict:
        """Turn a plain-words description into a task file with the local
        model (the same planner as `sched add --nl`). Nothing is saved: the
        page shows the draft in the New task editor to check and save."""
        if not body.text.strip():
            raise HTTPException(400, "describe the task first")
        cfg = current()
        tasks_dir = _tasks_dir(cfg)
        try:
            task = planner_mod.nl_to_task(draft_request(body.text, body.draft), cfg)
        except planner_mod.PlannerError as e:
            raise HTTPException(422, str(e)) from e
        except (ollama.ResponseError, ConnectionError, OSError) as e:
            raise HTTPException(503, f"couldn't reach the local model: {e}") from e
        existing, _ = tasks_mod.load_tasks_dir(tasks_dir)
        task = dataclasses.replace(task, id=_free_id(task.id, {t.id for t in existing}))
        return {"id": task.id, "content": tasks_mod.render_task_file(task)}

    @app.put("/api/tasks/file/{filename}")
    def update_task_file(filename: str, body: UpdateTaskBody) -> dict:
        cfg = current()
        path = _safe_task_path(cfg, filename)
        if not path.exists():
            raise HTTPException(404, f"no such file: {filename}")
        try:
            task = tasks_mod.parse_task_text(body.content, path)
        except tasks_mod.TaskValidationError as e:
            raise HTTPException(400, str(e)) from e

        tasks_dir = _tasks_dir(cfg)
        existing, _ = tasks_mod.load_tasks_dir(tasks_dir)
        clash = next(
            (t for t in existing if t.id == task.id and t.path.name != filename), None
        )
        if clash is not None:
            raise HTTPException(
                409, f"a different file ({clash.path.name}) already uses id '{task.id}'"
            )

        path.write_text(body.content)
        _sync(cfg)
        return {"filename": filename, "id": task.id}

    @app.delete("/api/tasks/file/{filename}")
    def delete_task_file(filename: str) -> dict:
        cfg = current()
        path = _safe_task_path(cfg, filename)
        if not path.exists():
            raise HTTPException(404, f"no such file: {filename}")
        task_id = None
        try:
            task_id = tasks_mod.parse_task_text(path.read_text(), path).id
        except tasks_mod.TaskValidationError:
            pass
        path.unlink()
        if task_id is not None:
            conn = _open_store(cfg)
            store_mod.delete_task_state(conn, task_id)
            conn.close()
        return {"deleted": filename}

    @app.post("/api/tasks/{task_id}/enable")
    def enable_task(task_id: str) -> dict:
        cfg = current()
        return _set_enabled(cfg, task_id, True)

    @app.post("/api/tasks/{task_id}/disable")
    def disable_task(task_id: str) -> dict:
        cfg = current()
        return _set_enabled(cfg, task_id, False)

    @app.get("/api/tasks/{task_id}/runs")
    def task_runs(task_id: str, last: int = 10) -> list[dict]:
        cfg = current()
        conn = _open_store(cfg)
        rows = store_mod.list_runs(conn, task_id, limit=last)
        conn.close()
        return [dict(r) for r in rows]

    @app.get("/api/runs/{run_id}/report", response_class=PlainTextResponse)
    def run_report(run_id: int) -> str:
        cfg = current()
        conn = _open_store(cfg)
        row = conn.execute(
            "SELECT report_path FROM runs WHERE run_id=?", (run_id,)
        ).fetchone()
        conn.close()
        if row is None or not row["report_path"]:
            raise HTTPException(404, "no report for that run")
        report_path = Path(row["report_path"])
        if not report_path.exists():
            raise HTTPException(404, f"report file missing: {report_path}")
        return report_path.read_text()

    @app.post("/api/tasks/{task_id}/run")
    def start_run(task_id: str) -> dict:
        cfg = current()
        valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
        task = next((t for t in valid if t.id == task_id), None)
        if task is None:
            raise HTTPException(404, f"no task '{task_id}'")

        if not daemon_mod.ensure_ollama_running(cfg):
            raise HTTPException(
                503, f"could not reach or start Ollama at {cfg.ollama.host}"
            )

        token = uuid.uuid4().hex
        q: queue.Queue = queue.Queue()
        _RUN_QUEUES[token] = q

        def worker() -> None:
            conn = _open_store(cfg)
            tz = schedule_mod.resolve_tz(cfg)
            now = datetime.now(tz)
            store_mod.upsert_task_state(
                conn, task.id, task.path, tasks_mod.content_hash(task.path), task.enabled
            )

            def on_event(text: str) -> None:
                q.put({"event": text})

            def executor_with_events(t, c):
                return agent_run_task(t, c, on_event=on_event)

            status, report_path = daemon_mod.execute_and_record(
                conn, task, cfg, tz, now, "manual", executor_with_events
            )
            q.put({"done": True, "status": status, "report_path": report_path})
            conn.close()

        threading.Thread(target=worker, daemon=True).start()
        return {"run_token": token, "task_id": task.id}

    @app.get("/api/models")
    def list_models() -> dict:
        cfg = current()
        info = {
            "host": cfg.ollama.host,
            "default": cfg.ollama.model,
            "can_change_default": config_path is not None,
        }
        client = ollama.Client(host=cfg.ollama.host, timeout=5)
        try:
            installed = models_mod.list_installed(client, with_capabilities=True)
            # Embedding/speech models can't run a task; don't offer them.
            installed = [m for m in installed if m.chat is not False]
        except Exception:  # noqa: BLE001 - Ollama being down is a state to show
            return {
                **info,
                "ollama_up": False,
                "effective_default": cfg.ollama.model,
                "note": None,
                "installed": [],
            }
        effective, note = models_mod.resolve_model(client, cfg.ollama.model, needs_tools=True)
        return {
            **info,
            "ollama_up": True,
            "effective_default": effective,
            "note": note,
            "installed": [{"name": m.name, "size": m.size, "tools": m.tools} for m in installed],
        }

    @app.put("/api/models/default")
    def set_default_model(body: ModelBody) -> dict:
        if config_path is None:
            raise HTTPException(409, "this server was started without a config file to save to")
        if not body.model or not config_mod.valid_model_name(body.model):
            raise HTTPException(400, "invalid model name")
        try:
            config_mod.set_default_model(config_path, body.model)
        except ValueError as e:
            raise HTTPException(400, str(e))
        return {"default": body.model}

    @app.put("/api/tasks/{task_id}/model")
    def set_task_model(task_id: str, body: ModelBody) -> dict:
        model = body.model or None
        if model is not None and not config_mod.valid_model_name(model):
            raise HTTPException(400, "invalid model name")
        cfg = current()
        valid, _ = tasks_mod.load_tasks_dir(_tasks_dir(cfg))
        task = next((t for t in valid if t.id == task_id), None)
        if task is None:
            raise HTTPException(404, f"no task '{task_id}'")
        try:
            tasks_mod.set_model(task.path, model)
        except tasks_mod.TaskValidationError as e:
            raise HTTPException(400, str(e))
        conn = _open_store(cfg)
        store_mod.upsert_task_state(
            conn, task.id, task.path, tasks_mod.content_hash(task.path), task.enabled
        )
        conn.close()
        return {"id": task.id, "model": model}

    @app.get("/api/runs/stream/{token}")
    def stream_run(token: str) -> StreamingResponse:
        if token not in _RUN_QUEUES:
            raise HTTPException(404, "unknown or already-finished run token")

        def gen() -> Iterator[str]:
            q = _RUN_QUEUES[token]
            try:
                while True:
                    item = q.get()
                    yield f"data: {json.dumps(item)}\n\n"
                    if item.get("done"):
                        break
            finally:
                _RUN_QUEUES.pop(token, None)

        return StreamingResponse(gen(), media_type="text/event-stream")

    return app
