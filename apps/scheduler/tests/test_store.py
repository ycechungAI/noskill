from pathlib import Path

from schedlm import store


def test_init_idempotent(tmp_path):
    db_path = tmp_path / "sched.db"
    conn1 = store.init_db(db_path)
    conn1.close()
    conn2 = store.init_db(db_path)
    tables = {
        r["name"]
        for r in conn2.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        ).fetchall()
    }
    assert {"task_state", "runs", "meta"} <= tables
    conn2.close()


def test_task_state_upsert_and_get(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    store.upsert_task_state(conn, "t1", Path("tasks/t1.md"), "hash1", True)
    row = store.get_task_state(conn, "t1")
    assert row["id"] == "t1"
    assert row["enabled"] == 1
    assert row["content_hash"] == "hash1"

    store.upsert_task_state(conn, "t1", Path("tasks/t1.md"), "hash2", False)
    row = store.get_task_state(conn, "t1")
    assert row["content_hash"] == "hash2"
    assert row["enabled"] == 0


def test_set_enabled_state_clears_next_run(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    store.upsert_task_state(
        conn, "t1", Path("tasks/t1.md"), "hash1", True, next_run_at="2026-07-19T00:00:00"
    )
    store.set_enabled_state(conn, "t1", False, clear_next_run=True)
    row = store.get_task_state(conn, "t1")
    assert row["enabled"] == 0
    assert row["next_run_at"] is None


def test_run_lifecycle(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    run_id = store.start_run(conn, "t1", "manual", "2026-07-18T07:00:00", model="qwen3:8b")
    row = store.list_runs(conn, "t1", limit=1)[0]
    assert row["status"] == "running"
    assert row["run_id"] == run_id

    store.finish_run(
        conn,
        run_id,
        status="success",
        finished_at="2026-07-18T07:03:12",
        report_path="runs/t1/x.md",
        iterations=5,
        tool_calls=7,
    )
    row = store.list_runs(conn, "t1", limit=1)[0]
    assert row["status"] == "success"
    assert row["report_path"] == "runs/t1/x.md"
    assert row["iterations"] == 5


def test_list_runs_errors_only(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    ok = store.start_run(conn, "t1", "manual", "2026-07-18T07:00:00")
    store.finish_run(conn, ok, status="success", finished_at="2026-07-18T07:01:00")
    bad = store.start_run(conn, "t1", "manual", "2026-07-18T08:00:00")
    store.finish_run(
        conn, bad, status="error", finished_at="2026-07-18T08:00:05", error="boom"
    )

    all_runs = store.list_runs(conn, "t1", limit=10)
    assert len(all_runs) == 2
    errors = store.list_runs(conn, "t1", limit=10, errors_only=True)
    assert len(errors) == 1
    assert errors[0]["run_id"] == bad


def test_startup_recovery_marks_running_as_error(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    run_id = store.start_run(conn, "t1", "schedule", "2026-07-18T07:00:00")
    store.upsert_task_state(
        conn, "t1", Path("tasks/t1.md"), "hash1", True
    )
    conn.execute(
        "UPDATE task_state SET running_pid=? WHERE id=?", (12345, "t1")
    )
    conn.commit()

    store.recover_running_state(conn)

    row = store.list_runs(conn, "t1", limit=1)[0]
    assert row["run_id"] == run_id
    assert row["status"] == "error"
    assert row["error"] == "daemon terminated during run"
    assert row["finished_at"] is not None

    task_row = store.get_task_state(conn, "t1")
    assert task_row["running_pid"] is None


# --- verification / statuses (2026-11-03: honest run statuses) -----------------


def test_init_allows_partial_status(tmp_path):
    conn = store.init_db(tmp_path / "sched.db")
    run_id = store.start_run(conn, "t1", "manual", "2026-11-03T05:00:00", model="m")
    store.finish_run(
        conn,
        run_id,
        status="partial",
        finished_at="2026-11-03T05:02:00",
    )
    rows = store.list_runs(conn, "t1", limit=5)
    assert rows[0]["status"] == "partial"


def test_init_migrates_old_schema_lacking_partial(tmp_path):
    db = tmp_path / "sched.db"
    conn = store.init_db(db)
    run_id = store.start_run(conn, "t1", "manual", "2026-11-03T05:00:00", model="m")
    store.finish_run(
        conn, run_id, status="success", finished_at="2026-11-03T05:02:00"
    )
    conn.close()

    conn2 = store.sqlite3.connect(str(db))
    conn2.execute("DROP TABLE runs")
    conn2.execute(
        """
        CREATE TABLE runs (
            run_id        INTEGER PRIMARY KEY AUTOINCREMENT,
            task_id       TEXT NOT NULL,
            trigger       TEXT NOT NULL CHECK (trigger IN ('schedule','manual','catchup')),
            started_at    TEXT NOT NULL,
            finished_at   TEXT,
            status        TEXT NOT NULL CHECK
                (status IN ('running','success','error','timeout','skipped_overlap')),
            report_path   TEXT,
            error         TEXT,
            model         TEXT,
            iterations    INTEGER,
            tool_calls    INTEGER
        );
        """
    )
    conn2.execute(
        "INSERT INTO runs (task_id, trigger, started_at, status) VALUES ('t1','manual','2026-11-03T05:00:00','success')"
    )
    conn2.commit()
    conn2.close()

    conn3 = store.init_db(db)  # must migrate without raising and keep the row
    rows = store.list_runs(conn3, "t1", limit=5)
    assert rows[0]["status"] == "success"
    conn3.close()
