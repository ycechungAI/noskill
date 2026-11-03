from datetime import datetime, timezone

from typer.testing import CliRunner

from schedlm import cli, store
from schedlm.config import Config, PathsConfig

TASK_TEXT = """---
id: sample-task
schedule: "0 8 * * *"
tools: []
---

Do the thing.
"""


def test_enable_after_disable_reschedules(tmp_path, monkeypatch):
    cfg = Config(
        timezone="UTC",
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        ),
    )
    (tmp_path / "tasks").mkdir()
    (tmp_path / "tasks" / "sample-task.md").write_text(TASK_TEXT)
    monkeypatch.setattr(cli, "_load_cfg", lambda: cfg)
    runner = CliRunner()

    assert runner.invoke(cli.app, ["disable", "sample-task"]).exit_code == 0
    conn = store.init_db(tmp_path / "data" / "sched.db")
    assert store.get_task_state(conn, "sample-task")["next_run_at"] is None

    assert runner.invoke(cli.app, ["enable", "sample-task"]).exit_code == 0

    row = store.get_task_state(conn, "sample-task")
    assert datetime.fromisoformat(row["next_run_at"]) > datetime.now(timezone.utc)
    conn.close()
