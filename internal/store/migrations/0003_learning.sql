-- Learning loop (LEARNING.md, FR-L2–L5).
-- The tool pattern of each run (tool names joined with ">"), so "the same pattern 3 times in 30 days"
-- is one query instead of reading traces.
ALTER TABLE runs ADD COLUMN tool_calls INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN tool_pattern TEXT;
CREATE INDEX runs_pattern ON runs(tool_pattern, started_at);

ALTER TABLE learning_queue ADD COLUMN reason TEXT;              -- why it was queued: novel | repeated | asked | failures …
ALTER TABLE learning_queue ADD COLUMN pattern TEXT;             -- the tool pattern, so a pattern is drafted once
ALTER TABLE learning_queue ADD COLUMN result TEXT;              -- the skill and version made, or where the draft was rejected to
ALTER TABLE learning_queue ADD COLUMN started_at INTEGER;
CREATE INDEX learning_queue_status ON learning_queue(status, created_at);

-- Small named values the learning worker keeps between runs (the last run it mined, …).
CREATE TABLE app_state (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
