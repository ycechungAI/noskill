# Implementation Plan: Ollama Research Orchestrator (STORM)

## Current State

The corrected agent/orchestrator core now exists under `orcastrator/`:
- `agents.py` — `Agent` wraps `ollama.AsyncClient` so `generate()` is truly awaitable.
- `orchestrator.py` — `StormOrchestrator` runs Planner → (Researcher ⇄ Critic) → Writer,
  with real question parsing and a bounded re-research loop.
- `cli.py` — `python -m orcastrator.cli "<topic>"` entry point.

Bugs fixed from the original README sketch: fake-async `generate()`, unparsed planner
output being iterated as a string, and the missing Critic step in the orchestrator class.

This plan covers what's needed to take that core to a reliable, testable tool.

## Phase 1 — Correctness & Connectivity (foundation, do first) — DONE
- [x] Verified `ollama.AsyncClient` against the locally running Ollama daemon
  (`llama3.2:latest`), both a single `Agent.generate()` call and a full
  `StormOrchestrator.run()` pass (Planner -> parallel Researcher/Critic -> Writer),
  which produced a real structured, referenced report (~11 min wall time locally).
- [x] Added a `--host` CLI option; `Agent`/`StormOrchestrator` accept `host=None` and forward
  it to `ollama.AsyncClient(host=...)`, which already falls back to `OLLAMA_HOST` /
  `localhost:11434` when unset.
- [x] Tightened the Planner system prompt's output-format instructions (plain text, one
  question per line, no numbering/bullets/commentary) to match what `_parse_questions`
  expects.
- [x] Switched the default model from `llama3` to `llama3.2`, since that's what's actually
  pulled on this machine.

## Phase 2 — Robustness of the Agent Loop — DONE
- [x] Added a per-request timeout (forwarded to the httpx client) and a bounded retry with
  backoff around `Agent.generate`, retrying only connection/timeout errors (not a bad model
  name or 4xx). Configurable via `timeout`/`retries`, exposed as `--timeout`/`--retries`.
- [x] Capped `MAX_REVISIONS` behavior with an `[unverified]` fallback when the Critic never
  approves, rather than passing the answer through as if validated.
- [x] Added `logging` stage transitions (plan received, question count, per-question
  approve/revise verdicts, writer invoked); shown with `--verbose`. httpx's own request
  logging is quieted so `--verbose` stays focused on pipeline progress.

## Phase 3 — Context Management for the Writer — DONE (budget + truncate)
- [x] Added `_budget_sections`: before the Writer call, if the combined research exceeds a
  character budget (`WRITER_CHAR_BUDGET`, ~12k chars) each section is truncated
  proportionally (with a floor so no topic disappears) so the final prompt doesn't overflow
  and get silently dropped.
- Deferred by decision: outline-then-per-section drafting. Revisit only if the budget proves
  insufficient for long topics on the target models.

## Phase 4 — Testing — DONE
- [x] Unit tests for `_parse_questions` (numbered, bulleted, blank-line, empty) and
  `_budget_sections` — `tests/test_parsing.py`.
- [x] Unit tests for `_research_and_validate` (approve-first, revise-then-approve,
  never-approved) and a fully-mocked `run()`, using scripted fake agents so no daemon is
  needed — `tests/test_research_loop.py`.
- [x] Opt-in integration test gated on `RUN_OLLAMA_TESTS=1` exercising the full
  `StormOrchestrator.run()` against a live model — `tests/test_integration.py`.

## Phase 5 — Output & Export — DEFERRED (Markdown only)
- [x] Markdown output via `cli.py -o report.md`.
- PDF export intentionally deferred to keep the project dependency-light. When wanted, the
  cleanest path is a `pandoc` subprocess on the produced Markdown (graceful skip if pandoc
  is absent) rather than a heavyweight pure-Python renderer.

## Phase 6 — Packaging & DX — DONE
- [x] Added `pyproject.toml` (setuptools) with an `orcastrator` console-script entry point,
  a `dev` extra for pytest, and pytest config. `requirements.txt` retained for the simple
  `pip install -r` path.
- [x] Documented env vars (`OLLAMA_HOST`, `RUN_OLLAMA_TESTS`, `OLLAMA_TEST_MODEL`) in the
  README rather than adding a `.env` mechanism, since all config is CLI flags + `OLLAMA_HOST`.

## Phase 7 — Bug-fix pass (2026-07-06) — DONE
A static review (no ability to run `pytest` in the review sandbox — see caveat below) found
four real issues, all fixed:
- [x] `agents.py`'s retry tuple missed `httpx.ReadError`/`WriteError`/`RemoteProtocolError`
  (only `ConnectionError` and `httpx.TimeoutException` were caught). These are legitimate
  transient failures (e.g. the daemon drops the connection mid-response on a long generate
  call) that should retry like a timeout does. Fixed by catching `httpx.TransportError`
  instead of just `TimeoutException` (it's the common parent).
- [x] `_research_and_validate`'s approved-path parsing (`verdict.split(":", 1)[1]`) would
  raise `IndexError` on a malformed critic reply like `"APPROVED"` with no colon. Switched to
  `partition` with a fallback to the last researcher answer.
- [x] Nothing ever closed the 4 agents' `ollama.AsyncClient` (httpx connection pool)
  instances. Added `Agent.aclose()` / `StormOrchestrator.aclose()`, called from `cli.py` in a
  `try/finally` around `run()`.
- [x] `cli.py` let `RuntimeError` (no planner questions) and connection failures surface as
  raw tracebacks; now caught at the top level with a clean `error: ...` message + exit code 1.
- [x] Added regression tests: `tests/test_agent_retry.py` (per-exception-type retry coverage,
  confirms non-retryable errors don't retry) and a malformed-verdict case added to
  `tests/test_research_loop.py`.

**Caveat:** this review's sandbox had no network access (couldn't resolve `exceptiongroup`
for pytest on the available Python 3.10) and the repo's checked-in `.venv` is a macOS/3.12
venv that doesn't run from this Linux shell — so the fixes were verified by manual trace
against the existing test patterns, not by an actual `pytest` run. Run `pytest` locally
before merging to confirm.

## Phase 8 — Candidate future features
In rough priority order (see `Todo.md` for the checklist form):
1. ~~**Retrieval grounding**~~ — DONE: multi-source web retrieval (Wikipedia, OpenAlex,
   DuckDuckGo, arXiv, Semantic Scholar, Stack Exchange, HN) with verified links, inline
   citations, and critic source-checking.
2. ~~**Bounded concurrency**~~ — DONE: memory-aware `asyncio.Semaphore` fan-out with a
   `--max-concurrency` override.
3. **Cloud LLM providers (multi-provider expansion)** — add OpenAI, Google Gemini,
   X.ai (Grok), and OpenRouter backends behind the same Backend interface as the local
   servers (Ollama, LM Studio, Jan.ai), selected via `--backend` / the web UI Server
   dropdown, with API keys from environment variables and per-provider docs. Keep the
   orchestrator provider-agnostic and existing backends fully backward compatible.
4. **Resumable runs** — checkpoint planner/researcher/critic state to disk given ~10 minute
   wall-clock times, so a crash doesn't mean starting over.
5. ~~**Live progress output**~~ — DONE: web dashboard with timer, stage tracker, and
   per-question statuses.
6. **PDF/HTML export** — still deferred; `pandoc` subprocess over the Markdown output,
   skipped gracefully when `pandoc` is missing.
7. **Outline-then-per-section Writer drafting** — still deferred; revisit only if
   `_budget_sections` truncation proves insufficient in practice.
8. **`--version` flag**, and **CLI-level tests** (argument parsing, `--output` file path —
   currently untested).

## Explicitly Out of Scope (per README's own framing as options)
- LangGraph/CrewAI/Autogen integration — the hand-rolled `asyncio`-based orchestrator in
  Section 5/6 is the chosen path; do not add a framework dependency unless the current
  approach hits a concrete limitation (e.g. need for persisted/resumable state).
