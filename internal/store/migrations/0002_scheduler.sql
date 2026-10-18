-- Scheduler (FR-J1, FR-J3, FR-J4): retries, notifications and runs waiting on approvals.
ALTER TABLE jobs ADD COLUMN notify TEXT NOT NULL DEFAULT '["os"]'; -- json: channel names, [] = none
ALTER TABLE jobs ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN retry_at INTEGER;                       -- next retry after a failure
ALTER TABLE jobs ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;     -- retries used for the current occurrence

ALTER TABLE job_runs ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE job_runs ADD COLUMN approval_id TEXT;                   -- set while waiting
ALTER TABLE job_runs ADD COLUMN result TEXT;                        -- short summary of the answer
CREATE INDEX job_runs_status ON job_runs(status);

-- One scheduler at a time, whichever process holds the lease (the daemon, or a foreground `serve`).
CREATE TABLE leases (
  name       TEXT PRIMARY KEY,
  holder     TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
