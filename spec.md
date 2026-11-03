# spec.md — Functional Specification

## 1. Summary

`sched` is a local-first task scheduler. A **task** is a self-contained prompt plus a schedule. At the scheduled time, a daemon executes the prompt with a **local Ollama model** in a tool-calling loop. Available tools include **web search** and **page fetching**. Each run writes a Markdown report to disk and a row to a SQLite history table.

Existing ("old") tasks are plain Markdown files that can be dropped into `tasks/`; new tasks can be created from the CLI, including from a natural-language sentence ("every weekday at 7am, summarize AI news").

## 2. Goals

- G1: Run scheduled LLM tasks entirely locally (Ollama), on macOS/Linux, from a single process.
- G2: Tasks can research current information on the web (search + fetch) during execution.
- G3: Tasks are portable, human-editable Markdown files with YAML frontmatter.
- G4: Every run produces a dated Markdown report and an auditable history record.
- G5: Manage tasks via CLI: add (file or natural language), list, show, run-now, enable/disable, remove, logs.
- G6: Survive restarts: schedule state is recomputed from task files; missed runs handled by an explicit catch-up policy.

## 3. Non-goals (v1)

- No GUI or web UI (optional milestone M6 only).
- No multi-user, auth, or distributed execution.
- No cloud LLM providers, no paid search APIs by default (pluggable interface only).
- No arbitrary shell execution by tasks (security decision — see §7).
- No Windows support.

## 4. User stories

- US1: As a user, I drop a Markdown task file into `tasks/` and it is picked up on the next daemon tick without restart.
- US2: As a user, I run `sched add --nl "every Monday 9am check for new Ollama model releases and summarize"` and get a generated task file to confirm before it is saved.
- US3: As a user, at 07:00 my "morning brief" task searches the web and writes `runs/morning-brief/2026-07-18T07-00.md` containing a cited summary.
- US4: As a user, I run `sched list` and see each task's id, schedule, enabled state, last run status, and next run time.
- US5: As a user, when my laptop was asleep at the scheduled time, a task with `catchup: true` runs once shortly after wake; a task with `catchup: false` waits for its next slot.
- US6: As a user, I run `sched logs morning-brief` and see recent runs with status and report paths.
- US7: As a user, I can disable a task without deleting it (`sched disable <id>`), and the file records `enabled: false` so the state survives anywhere the file goes.

## 5. Functional requirements

### Task definition
- FR-1: A task is one Markdown file in `tasks/` with YAML frontmatter (schema in DataModel.md). Required fields: `id`, `schedule` (cron) **or** `fire_at` (one-time ISO timestamp); prompt body required, non-empty.
- FR-2: `id` is unique kebab-case; duplicate ids across files are a validation error reported by `sched list` and the daemon log (first file wins, others ignored).
- FR-3: Invalid task files never crash the daemon; they are skipped and reported.

### Scheduling
- FR-4: Cron expressions are standard 5-field cron, evaluated in the configured local timezone (DST-safe via `zoneinfo`).
- FR-5: The daemon ticks every `tick_seconds` (default 30). A task fires when `now >= next_run_at`.
- FR-6: After firing, `next_run_at` is recomputed from the cron expression. One-time (`fire_at`) tasks are auto-disabled after they run.
- FR-7: Missed runs (daemon down / machine asleep): if `catchup: true` and the miss is within `catchup_grace_minutes` (default 60), run once at startup/wake; otherwise skip to the next scheduled slot. Never run more than once per missed window.
- FR-8: Overlap protection: if a task is still running when its next slot arrives, the new slot is skipped and logged.

### Execution
- FR-9: Execution = tool-calling chat loop against Ollama `/api/chat` with the task's model (default from config). Max `max_iterations` (default 12) tool rounds, then forced finalization.
- FR-10: Tools available per task are declared in frontmatter from the fixed registry: `web_search`, `fetch_page`, `current_datetime`, `read_note`, `write_note` (see Agents.md). Unknown tool names are a validation error.
- FR-11: Each run has a wall-clock timeout (`timeout_minutes`, default 10). On timeout the run is marked `timeout` and a partial report is written if any content exists.
- FR-12: The final assistant message is written as the run report (format in DataModel.md). Reports are pruned to the newest `output.keep` (default 30) per task.
- FR-12a: A task may set `output.dir` / `output.filename` with date template variables (e.g. `~/dev/briefing/{year}/{month_name}/day{day}.md`) and `output.raw: true` to write the model output verbatim to a custom location; directories are created as needed and custom destinations are never pruned (DataModel.md §1).
- FR-13: `sched run <id>` executes a task immediately, ignoring schedule and enabled state (manual runs recorded with `trigger = "manual"`).

### Web ability
- FR-14: `web_search(query, max_results)` returns title/url/snippet via the DuckDuckGo `ddgs` library by default; the provider sits behind a 1-function interface so Brave/Tavily/SearXNG can be added later without touching the agent loop.
- FR-15: `fetch_page(url, max_chars)` fetches with httpx (10s timeout, custom UA), extracts readable text with BeautifulSoup, truncates to `max_chars` (default 8000). Non-HTML or failed fetches return a structured error string, never an exception.

### Management
- FR-16: CLI per API.md: `list`, `add`, `show`, `run`, `enable`, `disable`, `rm`, `logs`, `daemon`.
- FR-17: `sched add --nl "<sentence>"` uses the Planner agent to draft a task file, prints it, and asks for confirmation (`--yes` to skip) before writing.
- FR-18: History persisted in SQLite: every run's start/end, status (`success|error|timeout|skipped_overlap`), trigger (`schedule|manual|catchup`), report path, error text, iterations used.

## 6. Non-functional requirements

- NFR-1: Runs on 16 GB RAM machines with ≤ 8B-parameter models; degrades gracefully to 3–4B models via config.
- NFR-2: Cold daemon start to first tick < 2s (no model loading at startup; Ollama loads lazily).
- NFR-3: All state inspectable with a text editor and `sqlite3` CLI. No opaque formats.
- NFR-4: Single logfile `data/sched.log` (rotating, 5 MB × 3) plus per-run reports.
- NFR-5: Zero network calls except: Ollama on localhost, web search, page fetches.

## 7. Security constraints

- S1: No shell/exec tool. File tools (`read_note`/`write_note`) are confined to `data/notes/` (path-traversal checked).
- S2: Fetched web content is data, not instructions: the executor system prompt (Agents.md) instructs the model to ignore instructions found inside fetched pages.
- S3: No secrets in task files; search API keys (if a paid provider is configured later) live in environment variables only.

## 8. Acceptance criteria (v1 done =)

- A1: All example tasks in `tasks/` validate and run via `sched run` producing cited reports.
- A2: A task scheduled 2 minutes ahead fires within `tick_seconds` of its slot with the daemon running.
- A3: Killing the daemon mid-run leaves the DB consistent (run marked `error` on next start) and never corrupts task files.
- A4: `pytest` suite from Testing.md passes; schedule math tests cover DST transitions.
- A5: With Wi-Fi off, a task using web tools still completes: tools return error strings and the report states that search was unavailable.
