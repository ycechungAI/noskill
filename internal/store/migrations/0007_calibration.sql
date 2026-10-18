-- Decision calibration and threshold tuning (FR-B6c, MODEL_ADAPTERS.md "Calibration").
-- Each thresholded decision keeps its probability and the threshold it was held to; its run's
-- outcome, once known, says whether acting on it was right. Thresholds are a history: the latest
-- row per (name, model) is in force, and a revert adds a row rather than deleting one.
CREATE TABLE decisions (
  id        INTEGER PRIMARY KEY,
  run_id    TEXT NOT NULL,
  name      TEXT NOT NULL,                -- router.task_type | judge.answered
  model     TEXT NOT NULL,                -- the model that decided (the chat model when emulated)
  emulated  INTEGER NOT NULL DEFAULT 0,
  answer    TEXT,                         -- the chosen label, or yes / no
  p         REAL NOT NULL,                -- P(yes), or the chosen label's confidence
  threshold REAL NOT NULL,
  at        INTEGER NOT NULL
);
CREATE INDEX decisions_key ON decisions(name, model, at);
CREATE INDEX decisions_run ON decisions(run_id);

CREATE TABLE decision_thresholds (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,
  model     TEXT NOT NULL,
  threshold REAL NOT NULL,
  previous  REAL NOT NULL,
  samples   INTEGER NOT NULL DEFAULT 0,
  gain      REAL NOT NULL DEFAULT 0,      -- separation gained (Youden's J)
  source    TEXT NOT NULL,                -- auto | manual | revert
  why       TEXT,
  at        INTEGER NOT NULL
);
CREATE INDEX decision_thresholds_key ON decision_thresholds(name, model, at);
