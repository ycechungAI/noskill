-- The watcher skill (FR-J1): what is watched and which items have been seen. Each watch is also a
-- scheduler job (job_id), so `noskill schedule list` shows it.
CREATE TABLE watches (
  name          TEXT PRIMARY KEY,
  kind          TEXT NOT NULL,            -- page | rss | github_releases | search
  target        TEXT NOT NULL,            -- URL, owner/repo, or the search query
  topic         TEXT,
  schedule      TEXT NOT NULL,
  job_id        TEXT,
  etag          TEXT,
  last_modified TEXT,
  last_checked  INTEGER,
  created_at    INTEGER NOT NULL
);

CREATE TABLE watch_items (
  watch       TEXT NOT NULL REFERENCES watches(name) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  title       TEXT,
  url         TEXT,
  first_seen  INTEGER NOT NULL,
  PRIMARY KEY (watch, fingerprint)
);
