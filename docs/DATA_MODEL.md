# Data model

## Storage layout

On macOS everything is under `~/.noskill/`. On Linux and Windows `config.toml` is in the config folder and the rest in the data folder ([PLATFORMS.md](PLATFORMS.md#paths)). `NOSKILL_HOME` puts everything in one folder.

```
<home>/
  config.toml           # config folder
  state.db              # main SQLite database (WAL mode)
  docs.db               # local-docs index (separate so it can grow and be rebuilt freely)
  skills/{builtin,user,learned}/      # files are the source of truth for skills
  traces/YYYY/MM/DD/<run-id>.jsonl    # full traces; state.db has the index
  runs/<run-id>/work/                 # sandbox workspaces (deleted after 7 days)
  runtimes/  logs/  run/ (IPC socket)
```

Rules:
- **Files are the source of truth for skills and traces.** The database indexes them and holds state that has no file form (jobs, approvals, memory, stats). Deleting `state.db` loses history and stats, but skills and traces stay, and `noskill doctor --reindex` rebuilds what it can.
- **Single writer:** the daemon. When the daemon isn't running, the CLI takes a file lock and writes directly.
- Times are UTC unix milliseconds. IDs are ULIDs (sortable, generated offline).
- Vectors are little-endian float32 blobs with their `embed_model` recorded; changing the embed model re-embeds in the background.

## Tables (`state.db`)

### Models
| Table | Columns |
|---|---|
| `backends` | `id`, `kind`, `url`, `version`, `last_seen_at` |
| `models` | `id` (e.g. `ollama/qwen3:8b`), `backend_id`, `name`, `family`, `params_b`, `quant`, `ctx`, `capabilities` (json), `tool_strategy`, `resident`, `updated_at` |
| `model_bench` | `model_id`, `task_type` (`_perf` for the speed row; otherwise a router task type with its micro-eval `score`), `tok_s`, `ttft_ms`, `load_ms_cold`, `load_ms_warm`, `mem_bytes`, `ctx`, `measured_at` |
| `bandit` | `task_type`, `difficulty_bucket`, `model_id` (the model's name), `alpha`, `beta`, `updated_at` · PK (task_type, difficulty_bucket, model_id). Evidence only: α = the sum of rewards, β = the sum of their complements; the benchmark prior is added when routing ([ORCHESTRATOR.md](ORCHESTRATOR.md#learned-routing)) |
| `decision_stats` | `name`, `model_id`, `n`, `ece`, `threshold`, `updated_at` |

### Skills
| Table | Columns |
|---|---|
| `skills` | `name` PK, `origin`, `status`, `version`, `path`, `description`, `triggers` (json), `permissions` (json), `content_hash`, `enabled`, `installed_from` (git url + commit), `created_at`, `updated_at` |
| `skill_versions` | `name`, `version`, `content_hash`, `parent_version`, `changelog`, `created_by` (user/learning/install), `created_at` |
| `skill_stats` | `name`, `version`, `uses`, `successes`, `failures`, `skips`, `avg_steps`, `last_used_at` |
| `skill_vectors` | `name`, `version`, `embed_model`, `vector` |
| `skill_kv` | `skill`, `key`, `value` (json), `updated_at`: private storage per skill (watcher items, anime taste profile, rule ids; the anime tracker's `titles.<id>`, `plan.<id>` for downloads it asked about or you added by hand, and the folders it may write to) |

### Runs and learning
| Table | Columns |
|---|---|
| `runs` | `id`, `parent_id`, `source` (cli/proxy/job/tray), `client` (token name), `conversation_fp`, `driver_model`, `status`, `outcome`, `outcome_conf`, `outcome_source`, `steps`, `tokens`, `ms`, `swaps`, `no_learn`, `pinned`, `trace_path`, `tool_calls`, `tool_pattern` (tool names joined with `>`), `task_type` ("" when pinned), `difficulty_bucket`, `reward` (what the run counts in the bandit; NULL = nothing), `started_at`, `ended_at` |
| `run_skills` | `run_id`, `skill`, `version` |
| `undo_log` | `run_id`, `seq`, `op` (move/copy/create/mkdir/trash), `from`, `to`, `hash`, `created_at`, `undone_at`. A move across drives is logged as `copy` then `trash` of the source |
| `learning_queue` | `id`, `kind` (draft/refine/merge), `run_ids` (json), `skill` (refine: the skill; merge: the keeper), `pattern` (draft: the tool pattern; merge: the duplicate skill), `reason`, `status`, `attempts`, `last_error`, `result`, `started_at`, `created_at`, `updated_at` |
| `eval_runs` | `id`, `model`, `suite`, `started_at`, `ended_at`, `tasks`, `passed`, `skipped`, `skill_state` (json: learned skill → version at the time), `note`. One row per `noskill eval` run (migration 0005) |
| `decisions` | `id`, `run_id`, `name` (`router.task_type` / `judge.answered`), `model` (the chat model when emulated), `emulated`, `answer`, `p` (P(yes) or the chosen label's confidence), `threshold` (the one it was held to), `at`. Labelled at query time from the run's outcome (migration 0007, FR-B6c) |
| `decision_thresholds` | `id`, `name`, `model`, `threshold`, `previous`, `samples`, `gain` (Youden's J gained), `source` (auto/manual/revert), `why`, `at`. A history: the newest row per (name, model) is in force; a revert adds a row (migration 0007) |
| `eval_results` | `run_id`, `task_id`, `type`, `source` (builtin / pinned), `passed`, `skipped`, `why`, `steps`, `tokens`, `ms`, `learned` (comma-separated learned skills the task's run used; migration 0006) · PK (run_id, task_id) |

### Jobs and approvals
| Table | Columns |
|---|---|
| `jobs` | `id`, `name`, `target_kind` (skill/prompt), `target`, `args` (json: `prompt`, `allow`, `model`), `schedule` (`cron:<expr>` or `every:<dur>`), `preconditions` (json: strings `volume:<label>`, `ac-power`, `unmetered`, `window:HH:MM-HH:MM`; FR-J5), `missed_policy` (once/skip), `overlap_policy` (skip/queue), `timeout_s`, `retries`, `notify` (json: channel names), `enabled`, `next_run_at`, `retry_at`, `attempt`, `last_run_id`, `created_at` |
| `job_runs` | `id`, `job_id`, `run_id`, `scheduled_for`, `started_at`, `ended_at`, `status` (running/waiting/stopping/done/failed/timeout/stopped/declined/missed/overlap/unmet/interrupted; `unmet` = a precondition didn't hold before the next occurrence, with the reason in `error`), `attempt`, `approval_id` (while waiting), `result` (short summary), `error` |
| `leases` | `name`, `holder` (host/pid/id), `expires_at`: the one process that runs the scheduler ([ADR-0009](adr/0009-scheduler-through-the-database.md)) |
| `approvals` (a `confirm` item's `details` hold `skill`, `tool`, `args`, `key`, `client` and `used`; see SECURITY.md "Approvals") | `id`, `source` (job/skill/learning/install/proxy), `kind`, `summary`, `details` (json), `fields` (json: editable fields `{name, type, prefill, value}`, e.g. anime-tracker's `dest` folder), `run_id`, `conversation_fp`, `resume_token` (json continuation of a parked run, ≤ 4 MB: messages, pending tool calls, model, skills, strategy, steps, flags), `status` (pending/approved/skipped/expired), `answered_via`, `note`, `expires_at`, `created_at`, `resolved_at` |

### Watches
| Table | Columns |
|---|---|
| `watches` | `name`, `kind` (page/rss/github_releases/search), `target`, `topic`, `schedule`, `job_id` (each watch is a scheduler job named `watch: <name>`), `etag`, `last_modified`, `last_checked`, `created_at` (migration 0008) |
| `watch_items` | `watch`, `fingerprint`, `title`, `url`, `first_seen` · PK (watch, fingerprint). What a check has already reported or recorded, so only new items are reported |

### Memory
| Table | Columns |
|---|---|
| `memory` | `id`, `key`, `value`, `source` (explicit/extracted), `confidence`, `created_at`, `updated_at`, `last_used_at` |
| `memory_vectors` | `memory_id`, `embed_model`, `vector` |

### Misc
| Table | Columns |
|---|---|
| `schema_migrations` | `version`, `applied_at` |
| `app_state` | `key`, `value`, `updated_at`: small values the learning worker keeps (`learning.mined_until`, `learning.curated_at`, `learning.trial.<skill>` as JSON `{version, approved, asked, failures}`) |
| `volumes` | `label`, `uuid`, `fs_type`, `last_mount_path`, `last_seen_at`: bindings for `volume:<label>` |

## `docs.db` (local-docs)
`folders` (path, added_at, indexed_at: the folders you added with `noskill docs add`; the tools read only inside them), `files` (id, folder, path, rel, size, mtime, hash, status indexed/unindexed, note: why a file was left out), `chunks` (id, file_id, seq, start, end, section, text: about 1600 characters with 200 overlap; a Markdown heading starts a chunk), `chunks_fts` (SQLite FTS5 over text, kept in step by triggers), `chunk_vectors` (chunk_id, embed_model, vector). Search fuses BM25 and cosine by reciprocal rank. `PRAGMA user_version` is the schema version; a different one drops everything except `folders` and re-indexes, since nothing here can't be rebuilt.

## Migrations
Block 2a added `0003_learning.sql` (`runs.tool_calls` and `tool_pattern`, the queue columns, `app_state`) and `0004_routing.sql` (`runs.task_type`, `difficulty_bucket`, `reward`).
- Forward-only SQL files embedded in the binary (`internal/store/migrations/NNNN_name.sql`).
- Applied at daemon start in one transaction, after copying `state.db` to `state.db.bak-<old-version>` (last 3 kept).
- A downgrade (`noskill update --to <older>`) restores the matching backup if the schema is newer than the binary understands, and says what history is lost.

## Retention
| Data | Default |
|---|---|
| Traces | 90 days, except pinned, eval, and those a learned skill was made from |
| `job_runs` | 180 days |
| Resolved approvals | 30 days |
| Sandbox workspaces | 7 days |
| Undo log | 30 days |

## Export / import
`noskill export [--all] <file.tar.zst>`: config (secret references only, never values), user + learned skills, `state.db`, optionally traces. `noskill import <file>` on another machine; model profiles and volume bindings are re-measured there instead of being copied.
