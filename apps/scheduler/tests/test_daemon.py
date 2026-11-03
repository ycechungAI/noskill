import dataclasses
import logging
import time
from datetime import datetime, timedelta
from pathlib import Path
from zoneinfo import ZoneInfo

import ollama

from schedlm import daemon, report, schedule, store, tasks
from schedlm.config import Config, DaemonConfig, PathsConfig

TZ = ZoneInfo("UTC")


def make_cfg(tmp_path: Path) -> Config:
    return Config(
        timezone="UTC",
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        ),
    )


def write_task(tmp_path: Path, task_id: str, schedule: str = "* * * * *") -> Path:
    tasks_dir = tmp_path / "tasks"
    tasks_dir.mkdir(parents=True, exist_ok=True)
    path = tasks_dir / f"{task_id}.md"
    path.write_text(
        f'---\nid: {task_id}\nschedule: "{schedule}"\n---\n\nDo the thing.\n'
    )
    return path


def seed_due_task_state(cfg: Config, task_id: str, path: Path, when_past: datetime) -> Path:
    db_path = Path(cfg.paths.data) / "sched.db"
    conn = store.init_db(db_path)
    store.upsert_task_state(
        conn,
        task_id,
        path,
        tasks.content_hash(path),
        True,
        next_run_at=when_past.isoformat(),
    )
    conn.close()
    return db_path


def test_tick_fires_due_task_advances_next_run_and_writes_report(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "mytask")
    now = datetime.now(TZ)
    seed_due_task_state(cfg, "mytask", path, now - timedelta(minutes=5))

    daemon.tick(cfg, executor=daemon.stub_execute)

    db_path = Path(cfg.paths.data) / "sched.db"
    conn = store.init_db(db_path)
    row = store.get_task_state(conn, "mytask")
    assert row["next_run_at"] is not None
    next_dt = datetime.fromisoformat(row["next_run_at"])
    assert next_dt > now

    runs = store.list_runs(conn, "mytask", limit=1)
    assert runs[0]["status"] == "success"
    assert runs[0]["report_path"] is not None

    report_path = Path(runs[0]["report_path"])
    assert report_path.exists()
    assert "hello from the stub executor" in report_path.read_text()


def test_tick_skips_overlapping_task_and_records_it(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "busy")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "busy", path, now - timedelta(minutes=5))

    conn = store.init_db(db_path)
    conn.execute("UPDATE task_state SET running_pid=? WHERE id=?", (99999, "busy"))
    conn.commit()
    conn.close()

    calls = []

    def fake_executor(task, cfg):
        calls.append(task.id)
        return report.RunResult(report="# x\nhi\n", iterations=1, tool_calls=0, model="m")

    daemon.tick(cfg, executor=fake_executor)

    assert calls == []  # executor must never be invoked while "running"

    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "busy", limit=1)
    assert runs[0]["status"] == "skipped_overlap"
    row = store.get_task_state(conn, "busy")
    next_dt = datetime.fromisoformat(row["next_run_at"])
    assert next_dt > now  # slot still advances even though it was skipped


def test_tick_survives_invalid_task_file(tmp_path, caplog):
    cfg = make_cfg(tmp_path)
    good_path = write_task(tmp_path, "good")
    now = datetime.now(TZ)
    seed_due_task_state(cfg, "good", good_path, now - timedelta(minutes=5))

    tasks_dir = tmp_path / "tasks"
    bad_path = tasks_dir / "bad.md"
    bad_path.write_text('---\nschedule: "* * * * *"\n---\n\nDo it.\n')  # missing id

    with caplog.at_level(logging.WARNING):
        daemon.tick(cfg, executor=daemon.stub_execute)

    assert any("invalid task file" in m for m in caplog.messages)

    db_path = Path(cfg.paths.data) / "sched.db"
    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "good", limit=1)
    assert runs[0]["status"] == "success"


def test_run_once_end_to_end(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "e2e")
    now = datetime.now(TZ)
    seed_due_task_state(cfg, "e2e", path, now - timedelta(minutes=5))

    daemon.run_once(cfg, executor=daemon.stub_execute)

    db_path = Path(cfg.paths.data) / "sched.db"
    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "e2e", limit=1)
    assert runs[0]["status"] == "success"


def test_startup_recovers_crashed_run(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "crashed")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "crashed", path, now + timedelta(hours=1))

    conn = store.init_db(db_path)
    store.start_run(conn, "crashed", "schedule", now.isoformat())
    conn.close()

    daemon.recover_crashed_runs(cfg)

    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "crashed", limit=1)
    assert runs[0]["status"] == "error"
    assert runs[0]["error"] == "daemon terminated during run"


def test_startup_catchup_fires_when_within_grace(tmp_path):
    cfg = Config(
        timezone="UTC",
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        ),
    )
    tasks_dir = tmp_path / "tasks"
    tasks_dir.mkdir(parents=True, exist_ok=True)
    path = tasks_dir / "catchup-task.md"
    path.write_text(
        '---\nid: catchup-task\nschedule: "0 7 * * *"\ncatchup: true\n---\n\nDo it.\n'
    )
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "catchup-task", path, now - timedelta(minutes=10))

    daemon.startup_catchup(cfg, executor=daemon.stub_execute)

    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "catchup-task", limit=1)
    assert runs[0]["trigger"] == "catchup"
    assert runs[0]["status"] == "success"


def test_startup_catchup_skips_when_beyond_grace(tmp_path):
    cfg = Config(
        timezone="UTC",
        daemon=DaemonConfig(catchup_grace_minutes=60),
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        ),
    )
    tasks_dir = tmp_path / "tasks"
    tasks_dir.mkdir(parents=True, exist_ok=True)
    path = tasks_dir / "late-task.md"
    path.write_text(
        '---\nid: late-task\nschedule: "0 7 * * *"\ncatchup: true\n---\n\nDo it.\n'
    )
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "late-task", path, now - timedelta(minutes=90))

    daemon.startup_catchup(cfg, executor=daemon.stub_execute)

    conn = store.init_db(db_path)
    runs = store.list_runs(conn, "late-task", limit=10)
    assert runs == []  # never executed

    row = store.get_task_state(conn, "late-task")
    next_dt = datetime.fromisoformat(row["next_run_at"])
    assert next_dt > now  # still advanced to a future slot


def test_sync_fills_in_missing_next_run_for_enabled_task(tmp_path):
    # What disable-then-enable used to leave behind: enabled, but no
    # next_run_at, so tick() skipped the task forever.
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "stuck", schedule="0 7 * * *")
    conn = store.init_db(Path(cfg.paths.data) / "sched.db")
    store.upsert_task_state(conn, "stuck", path, tasks.content_hash(path), True)
    now = datetime.now(TZ)

    daemon.sync_task_state(conn, tmp_path / "tasks", TZ, now)

    row = store.get_task_state(conn, "stuck")
    assert row["next_run_at"] == schedule.next_run("0 7 * * *", now, TZ).isoformat()


def test_sync_leaves_disabled_task_without_next_run(tmp_path):
    cfg = make_cfg(tmp_path)
    tasks_dir = tmp_path / "tasks"
    tasks_dir.mkdir(parents=True)
    path = tasks_dir / "off.md"
    path.write_text('---\nid: off\nschedule: "0 7 * * *"\nenabled: false\n---\n\nDo it.\n')
    conn = store.init_db(Path(cfg.paths.data) / "sched.db")
    store.upsert_task_state(conn, "off", path, tasks.content_hash(path), False)

    daemon.sync_task_state(conn, tasks_dir, TZ, datetime.now(TZ))

    assert store.get_task_state(conn, "off")["next_run_at"] is None


def test_reenabled_task_does_not_fire_its_stale_slot(tmp_path):
    # Disabled by editing the file, then enabled again: the slot stored
    # before it was disabled is long past and must not fire.
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "back", schedule="0 7 * * *")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "back", path, now - timedelta(days=3))
    conn = store.init_db(db_path)
    store.set_enabled_state(conn, "back", False)
    conn.close()

    daemon.tick(cfg, executor=daemon.stub_execute)

    conn = store.init_db(db_path)
    assert store.list_runs(conn, "back", limit=10) == []
    row = store.get_task_state(conn, "back")
    assert datetime.fromisoformat(row["next_run_at"]) > now


def test_execute_and_record_marks_timeout_on_slow_executor(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "slow")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "slow", path, now - timedelta(minutes=5))
    conn = store.init_db(db_path)
    task = dataclasses.replace(tasks.parse_task_file(path), timeout_minutes=0)  # 0s budget

    def slow_executor(task, cfg):
        time.sleep(0.2)
        return report.RunResult(report="too late", iterations=1, tool_calls=0, model="m")

    status, report_path = daemon.execute_and_record(
        conn, task, cfg, TZ, now, "manual", slow_executor
    )

    assert status == "timeout"
    assert report_path is None
    runs = store.list_runs(conn, "slow", limit=1)
    assert runs[0]["status"] == "timeout"
    assert "exceeded timeout_minutes=0" in runs[0]["error"]


def test_execute_and_record_reports_ollama_unreachable(tmp_path):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "unreachable")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "unreachable", path, now - timedelta(minutes=5))
    conn = store.init_db(db_path)
    task = tasks.parse_task_file(path)

    def down_executor(task, cfg):
        raise ConnectionError("Failed to connect to Ollama.")

    status, report_path = daemon.execute_and_record(
        conn, task, cfg, TZ, now, "manual", down_executor
    )

    assert status == "error"
    assert report_path is None
    runs = store.list_runs(conn, "unreachable", limit=1)
    assert runs[0]["status"] == "error"
    assert cfg.ollama.host in runs[0]["error"]


def test_execute_and_record_reports_model_not_found(tmp_path, caplog):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "missing-model")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "missing-model", path, now - timedelta(minutes=5))
    conn = store.init_db(db_path)
    task = tasks.parse_task_file(path)

    def missing_model_executor(task, cfg):
        raise ollama.ResponseError("model 'ghost:latest' not found", 404)

    with caplog.at_level(logging.WARNING):
        status, report_path = daemon.execute_and_record(
            conn, task, cfg, TZ, now, "manual", missing_model_executor
        )

    assert status == "error"
    runs = store.list_runs(conn, "missing-model", limit=1)
    assert runs[0]["error"] == "model 'ghost:latest' not found"
    assert any("ollama pull" in m for m in caplog.messages)


def test_execute_and_record_dumps_report_content_when_write_fails(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    path = write_task(tmp_path, "unwritable")
    now = datetime.now(TZ)
    db_path = seed_due_task_state(cfg, "unwritable", path, now - timedelta(minutes=5))
    conn = store.init_db(db_path)
    task = tasks.parse_task_file(path)

    def ok_executor(task, cfg):
        return report.RunResult(
            report="# Precious Content\n\ndo not lose me", iterations=1, tool_calls=0, model="m"
        )

    def broken_write_report(*args, **kwargs):
        raise OSError("disk full")

    monkeypatch.setattr(daemon.report_mod, "write_report", broken_write_report)

    status, report_path = daemon.execute_and_record(
        conn, task, cfg, TZ, now, "manual", ok_executor
    )

    assert status == "error"
    assert report_path is None
    runs = store.list_runs(conn, "unwritable", limit=1)
    assert runs[0]["status"] == "error"
    assert "Precious Content" in runs[0]["error"]
    assert "disk full" in runs[0]["error"]


class _FakeOllamaClientForEnsure:
    def __init__(self, reachable_from_call=None):
        self.list_calls = 0
        self._reachable_from_call = reachable_from_call

    def list(self):
        self.list_calls += 1
        if self._reachable_from_call is not None and self.list_calls >= self._reachable_from_call:
            return {"models": []}
        raise ConnectionError("not reachable")


def test_ensure_ollama_running_already_up_spawns_nothing(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    fake_client = _FakeOllamaClientForEnsure(reachable_from_call=1)
    monkeypatch.setattr(daemon.ollama, "Client", lambda host=None: fake_client)
    spawn_calls = []
    monkeypatch.setattr(
        daemon.subprocess, "Popen", lambda *a, **k: spawn_calls.append((a, k))
    )

    assert daemon.ensure_ollama_running(cfg) is True
    assert spawn_calls == []


def test_ensure_ollama_running_starts_it_when_down(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    # unreachable for the first probe, reachable from the 2nd `.list()` call
    # onward (i.e. after the (fake) spawn "succeeds").
    fake_client = _FakeOllamaClientForEnsure(reachable_from_call=2)
    monkeypatch.setattr(daemon.ollama, "Client", lambda host=None: fake_client)
    spawn_calls = []
    monkeypatch.setattr(
        daemon.subprocess, "Popen", lambda *a, **k: spawn_calls.append((a, k))
    )
    monkeypatch.setattr(daemon.time, "sleep", lambda s: None)  # don't actually wait
    monkeypatch.setattr(daemon, "find_ollama_binary", lambda: "/fake/bin/ollama")

    assert daemon.ensure_ollama_running(cfg) is True
    assert len(spawn_calls) == 1
    assert spawn_calls[0][0][0] == ["/fake/bin/ollama", "serve"]


def test_find_ollama_binary_checks_install_locations_off_path(tmp_path, monkeypatch):
    # launchd/cron PATH (/usr/bin:/bin) has no ollama; the app bundle does.
    fake = tmp_path / "ollama"
    fake.write_text("#!/bin/sh\n")
    fake.chmod(0o755)
    monkeypatch.setattr(daemon.shutil, "which", lambda name: None)
    monkeypatch.setattr(daemon, "OLLAMA_BINARY_CANDIDATES", ("/nope/ollama", str(fake)))

    assert daemon.find_ollama_binary() == str(fake)


def test_ensure_ollama_running_no_binary_anywhere(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    fake_client = _FakeOllamaClientForEnsure(reachable_from_call=None)
    monkeypatch.setattr(daemon.ollama, "Client", lambda host=None: fake_client)
    monkeypatch.setattr(daemon, "find_ollama_binary", lambda: None)
    spawn_calls = []
    monkeypatch.setattr(daemon.subprocess, "Popen", lambda *a, **k: spawn_calls.append(a))

    assert daemon.ensure_ollama_running(cfg) is False
    assert spawn_calls == []


def test_ensure_ollama_running_binary_missing(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    fake_client = _FakeOllamaClientForEnsure(reachable_from_call=None)
    monkeypatch.setattr(daemon.ollama, "Client", lambda host=None: fake_client)

    def raise_not_found(*a, **k):
        raise FileNotFoundError("no such file")

    monkeypatch.setattr(daemon.subprocess, "Popen", raise_not_found)

    assert daemon.ensure_ollama_running(cfg) is False


def test_ensure_ollama_running_gives_up_after_timeout(tmp_path, monkeypatch):
    cfg = make_cfg(tmp_path)
    fake_client = _FakeOllamaClientForEnsure(reachable_from_call=None)  # never comes up
    monkeypatch.setattr(daemon.ollama, "Client", lambda host=None: fake_client)
    monkeypatch.setattr(daemon.subprocess, "Popen", lambda *a, **k: None)
    monkeypatch.setattr(daemon, "OLLAMA_START_TIMEOUT_SECONDS", 0.05)
    monkeypatch.setattr(daemon.time, "sleep", lambda s: None)

    assert daemon.ensure_ollama_running(cfg) is False
