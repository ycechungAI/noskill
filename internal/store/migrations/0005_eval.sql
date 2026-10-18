-- Evaluation runs (`noskill eval`, FR-B4, LEARNING.md "Evaluation").
-- One row per run of a task suite on one model, and one per task. Learned-skill versions are kept
-- with the run so a regression can be traced to the version published since the last good run.
-- The weekly eval_results table planned in 0001 was never written; runs replace it.
DROP TABLE eval_results;

CREATE TABLE eval_runs (
  id          TEXT PRIMARY KEY,
  model       TEXT NOT NULL,
  suite       TEXT NOT NULL,          -- all | quick | the type list that was asked for
  started_at  INTEGER NOT NULL,
  ended_at    INTEGER,
  tasks       INTEGER NOT NULL DEFAULT 0,
  passed      INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  skill_state TEXT,                   -- json: learned skill name → version
  note        TEXT
);
CREATE INDEX eval_runs_model ON eval_runs(model, started_at);

CREATE TABLE eval_results (
  run_id   TEXT NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
  task_id  TEXT NOT NULL,
  type     TEXT NOT NULL,
  source   TEXT NOT NULL DEFAULT 'builtin',
  passed   INTEGER NOT NULL,
  skipped  INTEGER NOT NULL DEFAULT 0,
  why      TEXT,
  steps    INTEGER,
  tokens   INTEGER,
  ms       INTEGER,
  PRIMARY KEY (run_id, task_id)
);
