-- 0001: initial schema (DATA_MODEL.md). Times are UTC unix milliseconds; IDs are ULIDs.
-- JSON columns hold TEXT; vectors are little-endian float32 BLOBs.

-- Models
CREATE TABLE backends (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,
  url          TEXT NOT NULL,
  version      TEXT,
  last_seen_at INTEGER
);

CREATE TABLE models (
  id            TEXT PRIMARY KEY,            -- e.g. ollama/qwen3:8b
  backend_id    TEXT NOT NULL REFERENCES backends(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  family        TEXT,
  params_b      REAL,
  quant         TEXT,
  ctx           INTEGER,
  capabilities  TEXT NOT NULL DEFAULT '[]',  -- json
  tool_strategy TEXT,
  resident      INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE model_bench (
  model_id     TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
  task_type    TEXT NOT NULL,
  score        REAL,
  tok_s        REAL,
  ttft_ms      REAL,
  load_ms_cold REAL,
  load_ms_warm REAL,
  mem_bytes    INTEGER,
  ctx          INTEGER,
  measured_at  INTEGER NOT NULL,
  PRIMARY KEY (model_id, task_type)
);

CREATE TABLE bandit (
  task_type         TEXT NOT NULL,
  difficulty_bucket INTEGER NOT NULL,
  model_id          TEXT NOT NULL,
  alpha             REAL NOT NULL,
  beta              REAL NOT NULL,
  updated_at        INTEGER NOT NULL,
  PRIMARY KEY (task_type, difficulty_bucket, model_id)
);

CREATE TABLE decision_stats (
  name       TEXT NOT NULL,
  model_id   TEXT NOT NULL,
  n          INTEGER NOT NULL DEFAULT 0,
  ece        REAL,
  threshold  REAL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (name, model_id)
);

-- Skills (files are the source of truth; these index them)
CREATE TABLE skills (
  name           TEXT PRIMARY KEY,
  origin         TEXT NOT NULL,              -- builtin | user | learned | imported
  status         TEXT NOT NULL,              -- draft | candidate | active | stale | archived
  version        TEXT,
  path           TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  triggers       TEXT NOT NULL DEFAULT '[]', -- json
  permissions    TEXT NOT NULL DEFAULT '{}', -- json
  content_hash   TEXT,
  enabled        INTEGER NOT NULL DEFAULT 1,
  installed_from TEXT,                       -- git url + commit
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE TABLE skill_versions (
  name           TEXT NOT NULL,
  version        TEXT NOT NULL,
  content_hash   TEXT NOT NULL,
  parent_version TEXT,
  changelog      TEXT,
  created_by     TEXT NOT NULL,              -- user | learning | install
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (name, version)
);

CREATE TABLE skill_stats (
  name         TEXT NOT NULL,
  version      TEXT NOT NULL,
  uses         INTEGER NOT NULL DEFAULT 0,
  successes    INTEGER NOT NULL DEFAULT 0,
  failures     INTEGER NOT NULL DEFAULT 0,
  skips        INTEGER NOT NULL DEFAULT 0,
  avg_steps    REAL,
  last_used_at INTEGER,
  PRIMARY KEY (name, version)
);

CREATE TABLE skill_vectors (
  name        TEXT NOT NULL,
  version     TEXT NOT NULL,
  embed_model TEXT NOT NULL,
  vector      BLOB NOT NULL,
  PRIMARY KEY (name, version, embed_model)
);

CREATE TABLE skill_kv (
  skill      TEXT NOT NULL,
  key        TEXT NOT NULL,
  value      TEXT NOT NULL,                  -- json
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (skill, key)
);

-- Runs and learning
CREATE TABLE runs (
  id              TEXT PRIMARY KEY,
  parent_id       TEXT,
  source          TEXT NOT NULL,             -- cli | proxy | job | tray
  client          TEXT,                      -- token name
  conversation_fp TEXT,
  driver_model    TEXT,
  status          TEXT NOT NULL,             -- running | done | failed | waiting | cancelled
  outcome         TEXT,                      -- success | partial | fail
  outcome_conf    REAL,
  outcome_source  TEXT,
  steps           INTEGER NOT NULL DEFAULT 0,
  tokens          INTEGER NOT NULL DEFAULT 0,
  ms              INTEGER NOT NULL DEFAULT 0,
  swaps           INTEGER NOT NULL DEFAULT 0,
  no_learn        INTEGER NOT NULL DEFAULT 0,
  pinned          INTEGER NOT NULL DEFAULT 0,
  trace_path      TEXT,
  started_at      INTEGER NOT NULL,
  ended_at        INTEGER
);
CREATE INDEX runs_started ON runs(started_at);
CREATE INDEX runs_fp ON runs(conversation_fp);

CREATE TABLE run_skills (
  run_id  TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  skill   TEXT NOT NULL,
  version TEXT,
  PRIMARY KEY (run_id, skill)
);

CREATE TABLE undo_log (
  run_id    TEXT NOT NULL,
  seq       INTEGER NOT NULL,
  op        TEXT NOT NULL,                   -- move | create | trash | copy | mkdir
  "from"    TEXT,
  "to"      TEXT,
  hash      TEXT,
  undone_at INTEGER,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE learning_queue (
  id         TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,                  -- draft | refine | merge
  run_ids    TEXT NOT NULL DEFAULT '[]',     -- json
  skill      TEXT,
  status     TEXT NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE eval_results (
  week               TEXT NOT NULL,
  task_id            TEXT NOT NULL,
  model_id           TEXT NOT NULL,
  success            INTEGER NOT NULL,
  steps              INTEGER,
  tokens             INTEGER,
  ms                 INTEGER,
  learned_skill_used TEXT,
  PRIMARY KEY (week, task_id, model_id)
);

-- Jobs and approvals
CREATE TABLE jobs (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  target_kind    TEXT NOT NULL,              -- skill | prompt
  target         TEXT NOT NULL,
  args           TEXT NOT NULL DEFAULT '{}', -- json
  schedule       TEXT NOT NULL,
  preconditions  TEXT NOT NULL DEFAULT '[]', -- json
  missed_policy  TEXT NOT NULL,
  overlap_policy TEXT NOT NULL,
  timeout_s      INTEGER NOT NULL,
  retries        INTEGER NOT NULL,
  enabled        INTEGER NOT NULL DEFAULT 1,
  next_run_at    INTEGER,
  last_run_id    TEXT
);

CREATE TABLE job_runs (
  id            TEXT PRIMARY KEY,
  job_id        TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  run_id        TEXT,
  scheduled_for INTEGER NOT NULL,
  started_at    INTEGER,
  ended_at      INTEGER,
  status        TEXT NOT NULL,
  error         TEXT
);
CREATE INDEX job_runs_job ON job_runs(job_id, scheduled_for);

CREATE TABLE approvals (
  id              TEXT PRIMARY KEY,
  source          TEXT NOT NULL,             -- job | skill | learning | install | proxy
  kind            TEXT NOT NULL,
  summary         TEXT NOT NULL,
  details         TEXT NOT NULL DEFAULT '{}', -- json
  fields          TEXT NOT NULL DEFAULT '[]', -- json: [{name, type, prefill, value}]
  run_id          TEXT,
  conversation_fp TEXT,
  resume_token    TEXT,
  status          TEXT NOT NULL,             -- pending | approved | skipped | expired
  answered_via    TEXT,
  note            TEXT,
  expires_at      INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  resolved_at     INTEGER
);
CREATE INDEX approvals_status ON approvals(status, expires_at);

-- Memory
CREATE TABLE memory (
  id           TEXT PRIMARY KEY,
  key          TEXT NOT NULL,
  value        TEXT NOT NULL,
  source       TEXT NOT NULL,                -- explicit | extracted
  confidence   REAL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  last_used_at INTEGER
);

CREATE TABLE memory_vectors (
  memory_id   TEXT NOT NULL REFERENCES memory(id) ON DELETE CASCADE,
  embed_model TEXT NOT NULL,
  vector      BLOB NOT NULL,
  PRIMARY KEY (memory_id, embed_model)
);

-- Misc
CREATE TABLE volumes (
  label           TEXT NOT NULL,
  uuid            TEXT NOT NULL PRIMARY KEY,
  fs_type         TEXT,
  last_mount_path TEXT,
  last_seen_at    INTEGER
);
