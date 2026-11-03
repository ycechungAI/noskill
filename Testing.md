# Testing.md

Test layout: `tests/` with `pytest`. Fake external services — never call real Ollama or the web in tests. `respx` mocks httpx; the `ollama` client is faked with a small stub class injected via a factory function (`executor.py` must take the client as a parameter with a default, so tests can pass `FakeOllama`).

## 1. Unit tests

### tests/test_schedule.py
- next run for `0 7 * * 1-5` from a Friday 08:00 → Monday 07:00.
- DST spring-forward and fall-back: schedule `30 2 * * *` around a DST change in `America/New_York` produces valid, monotonically increasing times.
- `fire_at` in the past → task reported as due exactly once, then disabled.
- catch-up: lateness 10 min with grace 60 → due (`trigger=catchup`); lateness 90 min → not due, next slot in the future.

### tests/test_tasks.py
- Round-trip: parse example task file → dataclass → write → identical semantics.
- Missing id / both `schedule`+`fire_at` / bad cron / unknown tool → `TaskValidationError` with the offending field named.
- Unknown frontmatter key → parsed fine, warning recorded.
- Duplicate ids across two files → first wins, second reported.

### tests/test_store.py
- Init creates schema; second init is a no-op (idempotent migrations).
- Run lifecycle: insert running → finalize success → visible in `logs` query ordering.
- Startup recovery: rows left `running` are flipped to `error`.

### tests/test_tools.py
- `web_search` with ddgs monkeypatched: results serialize to the documented JSON shape; raised exception → `{"error": ...}` string, no exception escapes.
- `fetch_page` via respx: HTML → text extraction drops script/style; 404 and timeout → error strings; result wrapped in `<fetched_content url=...>`.
- `read_note`/`write_note`: round-trip; `../evil` and `a/b` names rejected.

### tests/test_planner.py
- FakeOllama returns valid JSON → Task draft with cron `0 9 * * 1`.
- First reply invalid JSON, second valid → succeeds (retry path).
- Both invalid → raises the documented CLI error.

### tests/test_report.py
- Default path: report lands in `runs/<id>/<ts>.md` with frontmatter + footer; pruning keeps newest `keep`.
- Templated path: `output.dir: "{year}/{month_name}"`, `filename: "day{day}.md"` resolves against a frozen datetime; missing dirs created; re-run overwrites same file; no pruning outside default dir.
- `raw: true`: body written verbatim (no frontmatter/footer); DB `report_path` records the resolved absolute path.

## 2. Integration tests

### tests/test_executor.py (FakeOllama scripted conversations)
- Script: assistant calls `web_search` → tool result → assistant calls `fetch_page` → final message. Assert report content, `iterations == 3`, `tool_calls == 2`.
- Iteration cap: fake always returns tool calls → loop stops at `max_iterations`, forced-finalization message appended, final call made **without** tools.
- Tool result truncation at 12,000 chars; compressor fallback path hard-truncates when compressor fails.

### tests/test_daemon.py
- `sched daemon --once` with a due task (FakeOllama): run row created, `next_run_at` advanced **before** execution, report file written, prune keeps `output.keep` newest.
- Overlap: task marked running → tick skips it, records `skipped_overlap`.
- Invalid file in `tasks/` → tick completes, warning logged.

## 3. Manual smoke test (run after M3, then before calling v1 done)

1. `ollama pull qwen3:8b` and confirm `ollama show qwen3:8b` lists `tools` capability.
2. `sched add --file tasks/github-trending.md && sched run github-trending` → report exists, contains ≥3 URLs, footer shows model/duration.
3. Edit the task's schedule to 2 minutes from now, run `sched daemon`, wait → fires within 30 s of the slot (spec A2).
4. Turn Wi-Fi off, `sched run github-trending` → completes with a report stating search was unavailable (spec A5).
   Then `sched run daily-briefing` (Wi-Fi on) → file appears at `~/dev/briefing/<year>/<Month>/day<N>.md`, raw (no frontmatter), starting with `# Daily Briefing —`.
5. `sched add --nl "every monday at 9am check for new ollama model releases and summarize with links"` → sensible draft; confirm; `sched list` shows it.
6. Kill the daemon mid-run; restart; `sched logs` shows the interrupted run as `error` (spec A3).
