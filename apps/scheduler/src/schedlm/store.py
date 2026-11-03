"""SQLite schema, init, and CRUD for task_state/runs. See DataModel.md §2."""

from __future__ import annotations

import sqlite3
from datetime import datetime, timezone
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS task_state (
    id            TEXT PRIMARY KEY,
    path          TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    enabled       INTEGER NOT NULL,
    next_run_at   TEXT,
    last_run_at   TEXT,
    running_pid   INTEGER
);

CREATE TABLE IF NOT EXISTS runs (
    run_id        INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL,
    trigger       TEXT NOT NULL CHECK (trigger IN ('schedule','manual','catchup')),
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    status        TEXT NOT NULL CHECK
        (status IN ('running','success','partial','failed','error','timeout','skipped_overlap')),
    report_path   TEXT,
    error         TEXT,
    model         TEXT,
    iterations    INTEGER,
    tool_calls    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id, started_at DESC);

CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
"""


def init_db(path: Path) -> sqlite3.Connection:
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path))
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript(SCHEMA)
    _migrate(conn)
    conn.commit()
    return conn


def _migrate(conn: sqlite3.Connection) -> None:
    """Forward-only migrations (DataModel.md §2). SQLite can't alter a CHECK
    constraint, so one that lacks 'partial' (pre-verification databases) is
    rebuilt with its rows copied across."""
    row = conn.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name='runs'"
    ).fetchone()
    if row is None or ("partial" in row["sql"] and "failed" in row["sql"]):
        return
    conn.executescript(
        """
        BEGIN;
        CREATE TABLE runs_new (
            run_id        INTEGER PRIMARY KEY AUTOINCREMENT,
            task_id       TEXT NOT NULL,
            trigger       TEXT NOT NULL CHECK (trigger IN ('schedule','manual','catchup')),
            started_at    TEXT NOT NULL,
            finished_at   TEXT,
            status        TEXT NOT NULL CHECK
                (status IN ('running','success','partial','failed','error','timeout','skipped_overlap')),
            report_path   TEXT,
            error         TEXT,
            model         TEXT,
            iterations    INTEGER,
            tool_calls    INTEGER
        );
        INSERT INTO runs_new SELECT * FROM runs;
        DROP TABLE runs;
        ALTER TABLE runs_new RENAME TO runs;
        CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id, started_at DESC);
        COMMIT;
        """
    )


def recover_running_state(conn: sqlite3.Connection) -> None:
    """Mark any 'running' rows left from a previous process as 'error'."""
    now = datetime.now(timezone.utc).isoformat()
    conn.execute(
        "UPDATE runs SET status='error', error=?, finished_at=? WHERE status='running'",
        ("daemon terminated during run", now),
    )
    conn.execute("UPDATE task_state SET running_pid=NULL WHERE running_pid IS NOT NULL")
    conn.commit()


# --- task_state CRUD -------------------------------------------------------


def upsert_task_state(
    conn: sqlite3.Connection,
    task_id: str,
    path: Path,
    content_hash: str,
    enabled: bool,
    next_run_at: str | None = None,
    last_run_at: str | None = None,
) -> None:
    conn.execute(
        """
        INSERT INTO task_state (id, path, content_hash, enabled, next_run_at, last_run_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            path=excluded.path,
            content_hash=excluded.content_hash,
            enabled=excluded.enabled,
            next_run_at=COALESCE(excluded.next_run_at, task_state.next_run_at),
            last_run_at=COALESCE(excluded.last_run_at, task_state.last_run_at)
        """,
        (task_id, str(path), content_hash, int(enabled), next_run_at, last_run_at),
    )
    conn.commit()


def get_task_state(conn: sqlite3.Connection, task_id: str) -> sqlite3.Row | None:
    return conn.execute("SELECT * FROM task_state WHERE id=?", (task_id,)).fetchone()


def list_task_states(conn: sqlite3.Connection) -> list[sqlite3.Row]:
    return conn.execute("SELECT * FROM task_state ORDER BY id").fetchall()


def set_enabled_state(
    conn: sqlite3.Connection, task_id: str, enabled: bool, clear_next_run: bool = False
) -> None:
    if clear_next_run:
        conn.execute(
            "UPDATE task_state SET enabled=?, next_run_at=NULL WHERE id=?",
            (int(enabled), task_id),
        )
    else:
        conn.execute(
            "UPDATE task_state SET enabled=? WHERE id=?", (int(enabled), task_id)
        )
    conn.commit()


def delete_task_state(conn: sqlite3.Connection, task_id: str) -> None:
    conn.execute("DELETE FROM task_state WHERE id=?", (task_id,))
    conn.commit()


# --- runs CRUD --------------------------------------------------------------


def start_run(
    conn: sqlite3.Connection,
    task_id: str,
    trigger: str,
    started_at: str,
    model: str | None = None,
) -> int:
    cur = conn.execute(
        "INSERT INTO runs (task_id, trigger, started_at, status, model) "
        "VALUES (?, ?, ?, 'running', ?)",
        (task_id, trigger, started_at, model),
    )
    conn.commit()
    return cur.lastrowid


def finish_run(
    conn: sqlite3.Connection,
    run_id: int,
    status: str,
    finished_at: str,
    report_path: str | None = None,
    error: str | None = None,
    iterations: int | None = None,
    tool_calls: int | None = None,
    model: str | None = None,
) -> None:
    """`model`, if given, replaces the one recorded by start_run -- the
    executor may have substituted another model (models.resolve_model)."""
    conn.execute(
        """
        UPDATE runs SET status=?, finished_at=?, report_path=?, error=?,
            iterations=?, tool_calls=?, model=COALESCE(?, model)
        WHERE run_id=?
        """,
        (status, finished_at, report_path, error, iterations, tool_calls, model, run_id),
    )
    conn.commit()


def list_runs(
    conn: sqlite3.Connection, task_id: str, limit: int = 10, errors_only: bool = False
) -> list[sqlite3.Row]:
    query = "SELECT * FROM runs WHERE task_id=?"
    params: list = [task_id]
    if errors_only:
        query += " AND status != 'success'"
    query += " ORDER BY started_at DESC LIMIT ?"
    params.append(limit)
    return conn.execute(query, params).fetchall()
