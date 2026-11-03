# DataModel.md

## 1. Task file format (`tasks/<id>.md`)

Markdown with YAML frontmatter. The filename should match `id` but `id` in frontmatter is authoritative.

```markdown
---
id: morning-brief                # required; unique kebab-case [a-z0-9-]
name: Morning Brief              # optional; defaults to id
schedule: "0 7 * * 1-5"          # 5-field cron, local tz. XOR with fire_at
# fire_at: "2026-07-19T15:00:00" # one-time run (local tz unless offset given)
enabled: true                    # default true
model: qwen3:8b                  # optional; default from config.yaml
tools: [web_search, fetch_page, current_datetime]   # subset of registry; default: all
timeout_minutes: 10              # default 10
catchup: false                   # default false (see spec FR-7)
output:
  keep: 30                       # reports retained per task; default 30 (default dir only)
  # dir: "~/dev/briefing/{year}/{month_name}"   # optional custom destination (template vars, ~ expanded)
  # filename: "day{day}.md"      # optional; default "{iso_datetime}.md"
  # raw: true                    # optional; skip frontmatter/footer wrapper (default false)
---

Search the web for AI and local-LLM news from the last 24 hours.
Write a brief with the 5 most important items. For each: one-line summary,
why it matters, and the source URL. End with a "Worth watching" section.
```

### Source and check requirements (2026-11-03)

Task frontmatter gained two optional keys:

- `sources:` — list of `http(s)://` URLs the task requires the executor to actually fetch with
  `fetch_page`. The executor appends them as a numbered imperative block after the prompt, and
  verification fails the run (`required_source_not_fetched` / `required_source_failed`) when a
  source wasn't (or couldn't be) fetched.
- `checks:` — mapping of opt-in verification knobs: `require_sources` (default true when
  `sources` exist), `min_report_links` (minimum distinct URLs the final report must cite),
  `require_cited_sources` (fail when the report cites no URL the run actually fetched or got
  from search). Unknown check keys → validation error (stricter than unknown frontmatter keys).

Verification then runs at finalization (`agent/verify.py`, executor `finish()`): `failed` when no
verifiable web evidence was gathered, `partial` when checks were unmet, `success` when all the
task's required checks pass. Tasks with requirements get one bounded retry (`CHECKS_FAILED`) that
names each failed check.

### Output destination templating

`output.dir` / `output.filename` support `str.format` variables resolved at run time in the local timezone: `{year}` (2026), `{month02}` (07), `{month_name}` (July), `{day}` (18, no pad), `{day02}` (18), `{iso_date}` (2026-07-18), `{iso_datetime}` (2026-07-18T07-00). `report.py` expands `~`, creates missing directories, and overwrites an existing file at the same resolved path (idempotent re-runs). Pruning (`keep`) applies **only** when `dir` is unset (default `runs/<id>/`); custom destinations are never pruned. With `raw: true` the report body is written verbatim — no frontmatter, no footer. The DB `report_path` always records the resolved path.

Validation rules (implement in `tasks.py`):
1. Frontmatter must parse as YAML mapping; body (prompt) must be non-empty after stripping.
2. Exactly one of `schedule` / `fire_at`. `schedule` must satisfy `croniter.is_valid`; `fire_at` must parse as ISO-8601.
3. `tools` entries must exist in the registry (Agents.md §3).
4. Unknown frontmatter keys → warning, not error (forward compatibility).
5. `sources` entries must be valid `http(s)://` URLs; `checks` keys must be known (see §1).

## 2. SQLite schema (`data/sched.db`, WAL mode)

```sql
CREATE TABLE IF NOT EXISTS task_state (
    id            TEXT PRIMARY KEY,          -- task id
    path          TEXT NOT NULL,             -- absolute path of task file
    content_hash  TEXT NOT NULL,             -- sha256 of file bytes
    enabled       INTEGER NOT NULL,
    next_run_at   TEXT,                      -- UTC ISO-8601, NULL if one-time done
    last_run_at   TEXT,
    running_pid   INTEGER                    -- NULL when not running
);

CREATE TABLE IF NOT EXISTS runs (
    run_id        INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT NOT NULL,
    trigger       TEXT NOT NULL CHECK (trigger IN ('schedule','manual','catchup')),
    started_at    TEXT NOT NULL,             -- UTC ISO-8601
    finished_at   TEXT,
    status        TEXT NOT NULL CHECK
        (status IN ('running','success','partial','error','timeout','skipped_overlap')),
    report_path   TEXT,
    error         TEXT,
    model         TEXT,
    iterations    INTEGER,                   -- tool-loop rounds used
    tool_calls    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id, started_at DESC);

CREATE TABLE IF NOT EXISTS meta (            -- schema_version etc.
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
```

Rules: prompts are never stored in the DB (files are the source of truth). `store.py` owns all SQL; no other module writes SQL strings.

## 3. Run report format (`runs/<task-id>/<YYYY-MM-DDTHH-MM>.md`)

The final assistant message is the body. `report.py` prepends frontmatter and appends a footer:

```markdown
---
task: morning-brief
run_id: 42
trigger: schedule
started: 2026-07-18T07:00:02+08:00
status: success
checks_failed: unbacked_report: the report cites no URL this run actually fetched or found via search -- every figure in it is unbacked
---

# Morning Brief — 2026-07-18
...model-generated content with cited URLs...

---
_model: qwen3:8b · duration: 3m12s · iterations: 5 · tool calls: 7_
```

Filename timestamps use the local timezone with `:` replaced by `-`. After writing, prune oldest files beyond `output.keep`.

## 4. Config file (`config.yaml`, all keys optional with these defaults)

```yaml
timezone: local            # "local" resolves to the system's real IANA zone via the
                           # /etc/localtime symlink (e.g. "America/Toronto"), so DST
                           # transitions are handled correctly (spec FR-4); only if that
                           # can't be determined does it fall back to the current fixed
                           # UTC offset (datetime.now().astimezone().tzinfo), which does
                           # NOT shift across DST changes. See schedule.py::resolve_tz.
ollama:
  host: http://localhost:11434
  model: qwen3:8b          # default executor model
  planner_model: qwen3:8b
daemon:
  tick_seconds: 30
  max_concurrent: 1        # v1: only 1 honored
  catchup_grace_minutes: 60
agent:
  max_iterations: 12
  fetch_max_chars: 8000
  search_max_results: 5
  think: true              # request extended "thinking" output from models that
                            # support it (Ollama's `think` chat param); auto-disabled
                            # for the rest of a run the first time a model rejects it
                            # with a 400 "does not support thinking" error
search:
  provider: ddgs           # only v1 value
paths:                     # all relative to project root
  tasks: tasks
  runs: runs
  data: data
monitor:                   # `sched monitor` (Architecture.md §8)
  port: 8790               # status page, 127.0.0.1 only
  check_interval_seconds: 30
  heal_grace_seconds: 15   # a down service is re-checked after this before it's restarted
  services: []             # merged by name with the defaults (ollama, sched, monitor):
                           #   - name: noskill            # new name: added
                           #     check: {launchd: dev.noskill.daemon, http: "http://127.0.0.1:11435/"}
                           #     heal: {launchd: dev.noskill.daemon}   # or {command: [argv...]}
                           #   - {name: ollama, enabled: false}       # drops a default
```

`config.py` exposes a frozen `Config` dataclass; missing file ⇒ all defaults; unknown keys ⇒ warning.

The long-running daemon and the web UI re-read `config.yaml` when it changes (`config.ConfigWatcher`), keeping the last good config if the new file doesn't parse. The web UI's default-model picker rewrites just the `ollama.model` line (`config.set_default_model`), keeping other keys and comments. The per-task picker likewise rewrites just a task file's `model:` line (`tasks.set_model`; "Default" removes the line).
