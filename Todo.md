# Todo

## Done
- [x] Fix `Agent.generate` to use `ollama.AsyncClient` instead of the blocking top-level call.
- [x] Parse the Planner's raw reply into an actual list of questions (`_parse_questions`).
- [x] Add the `Critic` agent into `StormOrchestrator` with an approve/revise loop.
- [x] Wire up `orcastrator/cli.py` as a runnable entry point.
- [x] Update `README.md` code samples to match the corrected implementation and call out the fixed bugs.

## Phase 1 — Correctness & Connectivity
- [x] Manually verify `Agent.generate()` against a running local Ollama daemon
      (confirmed with `llama3.2:latest`, and a full `StormOrchestrator.run()` pass produced
      a real structured report end-to-end).
- [x] Add `--host` CLI flag / respect `OLLAMA_HOST` for non-default Ollama endpoints.
- [x] Tighten the Planner system prompt so its output reliably matches `_parse_questions`'s
      expected format (one question per line).
- [x] Default model switched to `llama3.2` (what's actually pulled locally; `llama3` isn't).

## Phase 2 — Robustness
- [x] Add timeout + retry around `Agent.generate` (configurable `timeout`/`retries`, exposed
      via `--timeout`/`--retries`; retries only connection/timeout errors with backoff).
- [x] Mark unresolved-after-`MAX_REVISIONS` answers as `[unverified]` in the combined text
      passed to the Writer, instead of treating them as validated.
- [x] Add `logging` calls for stage transitions (plan received, question count, per-question
      approve/revise verdicts, writer invoked); surfaced via `--verbose`.

## Phase 3 — Context Management
- [x] Add a size check before the Writer call (`_budget_sections`); truncate oversized
      sections proportionally to a character budget so the final prompt doesn't overflow.
- [ ] (Deferred) Split the Writer step into outline-then-per-section drafting — not needed at
      current budget; revisit if long topics still overflow. Per-section drafting deferred by
      decision (chose budget+truncate).

## Phase 4 — Testing
- [x] Unit tests for `_parse_questions` (numbered, bulleted, blank-line, empty-input cases)
      and `_budget_sections` (`tests/test_parsing.py`).
- [x] Unit tests for `_research_and_validate` and full mocked `run()` with scripted fake
      agents (`tests/test_research_loop.py`).
- [x] Opt-in integration test for `StormOrchestrator.run()` gated on `RUN_OLLAMA_TESTS=1`
      (`tests/test_integration.py`).

## Phase 5 — Output & Export
- [x] Markdown output via `--output` (already present).
- [ ] (Deferred by decision) PDF export — keeping the project dependency-light for now;
      Markdown output only. Documented as a future option in `Plan.md`.

## Phase 6 — Packaging & DX
- [x] Add `pyproject.toml` with a `orcastrator` console-script entry point, `dev` extra
      (pytest), and pytest config.
- [x] Document required env vars (`OLLAMA_HOST`, `RUN_OLLAMA_TESTS`, `OLLAMA_TEST_MODEL`) in
      the README.

## Phase 7 — Bug fixes (2026-07-06 review)
- [x] Broaden `_RETRYABLE` in `agents.py` from `(ConnectionError, httpx.TimeoutException)` to
      `(ConnectionError, httpx.TransportError)`. The ollama client re-wraps
      `httpx.ConnectError` as a plain `ConnectionError`, so daemon-down was already retried —
      but `httpx.ReadError`/`WriteError`/`RemoteProtocolError` (dropped connection mid-response)
      were not, since they're `TransportError` but not `TimeoutException`. Now covered.
- [x] Guard `_research_and_validate`'s critic-approved path: `verdict.split(":", 1)[1]` raised
      `IndexError` if the critic replied `"APPROVED"` with no colon/refined-answer. Switched to
      `str.partition` with a fallback to the researcher's last answer.
- [x] Added `Agent.aclose()` / `StormOrchestrator.aclose()` and wired it into `cli.py`
      (`try/finally`) — the 4 per-agent `ollama.AsyncClient` instances (each holding an httpx
      connection pool) were never closed.
- [x] `cli.py` now catches `RuntimeError`/`ConnectionError` at the top level and prints
      `error: ...` + exits 1, instead of a raw traceback (e.g. when the Planner returns no
      questions, or the daemon is unreachable after retries are exhausted).
- [x] New tests: `tests/test_agent_retry.py` (retry coverage per exception type, non-retryable
      errors don't retry) and a malformed-verdict case in `tests/test_research_loop.py`.
- Note: sandboxed review environment couldn't run `pytest` (no network to resolve
  `exceptiongroup`, and the checked-in `.venv` is a macOS/3.12 venv not usable from this
  Linux/3.10 shell) — behavior was verified by manual trace against the existing test
  patterns, not by executing the suite. Run `pytest` locally to confirm before relying on this.

## Phase 8 — Web UI & usability (2026-07-14 review)
- [x] Local web UI: `orcastrator-web` (FastAPI + uvicorn) serving a single-page app at
      `http://127.0.0.1:8400` (`orcastrator/web.py`, `orcastrator/static/index.html`).
      Topic form, model picker auto-populated from the Ollama daemon (`/api/models`),
      collapsible advanced settings (host/timeout/retries), live pipeline progress over
      Server-Sent Events, rendered Markdown report with Copy/Download buttons, Stop button,
      and friendly banners when Ollama is down or the pipeline fails.
- [x] `StormOrchestrator` gained an optional `on_progress` callback so UIs get live stage
      updates without scraping logging output.
- [x] CLI error handling broadened: `ollama.ResponseError` (e.g. model not pulled) and
      `httpx.TransportError` now print `error: ...` with a hint (`ollama serve` /
      `ollama pull <model>`) instead of a traceback; Ctrl-C exits 130 cleanly.
- [x] Web tests with a stubbed orchestrator (`tests/test_web.py`) — no Ollama needed.

## Phase 9 — Speed presets (2026-07-14, "runs too long" feedback)
- [x] `PRESETS` in `orchestrator.py`: fast (4 questions, critic off, 300-token answers),
      balanced (6 questions, critic on, 500-token answers), thorough (original behavior).
- [x] Orchestrator knobs: `max_questions` (planner prompt + hard truncation of parsed list),
      `use_critic` (skip the validation loop entirely), `answer_tokens` (Ollama `num_predict`
      cap on researcher/critic replies, plus a "be concise" nudge in the researcher prompt).
      Planner always capped at 400 tokens. `Agent` gained a `num_predict` option.
- [x] CLI `--preset` (default balanced); web UI "Speed" dropdown (default fast) with
      per-preset time estimates; `/api/research` accepts `preset`.
- [x] Tests: critic-disabled path, max_questions truncation, preset plumbing through the
      web endpoint.

## Phase 10 — Live progress dashboard (2026-07-14, "more visual + timer" feedback)
- [x] Structured progress events: `on_progress` now receives dicts (`_emit(kind, message,
      **fields)`) instead of plain strings. Kinds: `stage` (planning/researching/writing,
      the researching event carries the full question list + `use_critic`) and `question`
      (per-question `index` + `status`: researching/reviewing/revising/approved/done/
      unverified). `_research_and_validate` now takes a stable question `index`.
- [x] Web UI dashboard ([static/index.html](orcastrator/static/index.html)): elapsed timer
      (mm:ss, ticks each second, freezes on completion and reports final time), a
      Plan→Research→Write stage tracker with active-pulse and done-checkmark states + colored
      connectors, per-question status rows with colored badges + spinners, a
      "N / total completed" counter, and the old flat log demoted to a collapsible
      `<details>` activity log. Respects `prefers-reduced-motion`.
- [x] Tests updated for the dict event shape; new assertions on stage/question events.
      Verified end-to-end in-browser (balanced preset, 6/6 approved, timer 02:55).

## Phase 11 — One-command launcher (2026-07-14)
- [x] `./start.sh`: creates `.venv` + installs deps on first run (and re-installs if the
      venv predates the web dependencies), starts `ollama serve` only if the daemon isn't
      already up (waits up to 30s for readiness, stops it again on exit via trap only when
      the script started it), warns if no models are pulled, opens the browser
      (`open`/`xdg-open`), and runs `orcastrator-web` in the foreground so Ctrl-C tears
      everything down. Passes flags through (e.g. `./start.sh --port 9000`).

## Phase 12 — Chat mode + extended thinking (2026-07-15)
- [x] Research mode toggle in the web UI (default on). Off = plain chatbot against the
      selected model: new `POST /api/chat` endpoint streams tokens from
      `ollama.AsyncClient.chat` as plain text; the client keeps the multi-turn history and
      sends it whole each turn (server stateless). Chat bubbles stream live, render Markdown
      once complete, and errors mid-stream append as a readable line.
- [x] Extended thinking checkbox (research mode only) replaces the Speed dropdown:
      unchecked → `fast` preset (~1–2 min), checked → `thorough` preset (~5–10 min).
      The `balanced` preset remains available via the CLI `--preset`.
- [x] Test: `test_chat_streams_reply_text` with a fake Ollama client. Verified in-browser:
      multi-turn chat streaming, and a full research run post-rework (01:43, fast preset).

## Phase 13 — Deeper extended-thinking pipeline (2026-07-15)
- [x] New `extended` preset (web Extended-thinking checkbox now maps to it; also available
      via CLI `--preset extended`). Adds `extended: bool` to `StormOrchestrator` and two
      agents: a Challenger (cross-examines each answer: weakest claims/missing angles) and
      a Reviewing Editor (critiques the whole draft).
- [x] Per-question debate round when extended: research → challenge → rebuttal (researcher
      addresses each critique) → critic approve/revise as before. New question statuses
      `challenging`/`rebutting` in the dashboard.
- [x] Report phase when extended: draft → editorial review → rewrite (three stages) instead
      of a single write. Stage tracker is now dynamic — the planning event announces the
      run's stage list (3 stages normally, 5 extended: Plan → Research → Draft → Review →
      Rewrite) and the UI builds the tracker from it.
- [x] Tests: mocked extended end-to-end (stage sequence + status sequence + announced
      stages), standard-run stage announcement, extended preset plumbing via `/api/research`.

## Phase 14 — Thinking models + memory-aware concurrency (2026-07-15, gemma4:12b failure)
- [x] Root cause of the gemma4:12b pipeline failure: it's a thinking-capable model, and
      Ollama's hidden reasoning tokens count against `num_predict` — the planner's 400-token
      cap was consumed entirely by thinking, returning an empty response
      (`done_reason: length`) → "Planner returned no research questions". Fix:
      `_configure_thinking()` checks `/api/show` capabilities at run start and sets
      `think=False` on all agents for thinking models (also measured much faster: 70 vs
      553 eval tokens for the same planner reply).
- [x] Memory-aware research concurrency: after planning (model now loaded),
      `_pick_concurrency()` reads the model's footprint from `/api/ps` and system RAM
      (`os.sysconf`), then sizes the fan-out — <25% of RAM → 4 at a time, <45% → 2,
      else 1 (gemma4:12b = 7.5 of 16 GiB = 47% → sequential). Prevents big models from
      queueing 8+ requests server-side past the client timeout. `asyncio.Semaphore`
      bounds the gather; `--max-concurrency` overrides; the measurement is surfaced in
      the UI activity log and waiting questions show a "Queued" badge.
- [x] Tests: `_concurrency_for` tiers, semaphore bounding (peak-parallelism probe).

## Phase 15 — LM Studio backend (2026-07-15)
- [x] `orcastrator/backends.py`: Backend abstraction with `OllamaBackend` (native API via
      the official client) and `LMStudioBackend` (OpenAI-compatible API via httpx, no new
      dependency). Interface: `generate`, `chat_stream`, `list_models`, `capabilities`,
      `loaded_model_bytes`, `close`, plus a per-backend `default_concurrency` (4 / 2).
- [x] `Agent` now delegates to a shared backend (one connection pool per orchestrator);
      retry semantics unchanged. Orchestrator takes `backend="ollama"|"lmstudio"`;
      thinking-detection and memory measurement go through the backend and degrade
      gracefully where LM Studio's API doesn't expose them.
- [x] LM Studio reasoning-model handling: `<think>…</think>` stripped from pipeline
      replies; empty capped replies retried uncapped (no API switch to disable thinking);
      chat UI hides think blocks with a streaming-safe client-side filter.
- [x] Web UI: Server dropdown (Ollama / LM Studio) — models list, host placeholder, and
      requests all follow it. CLI: `--backend`, `$LMSTUDIO_HOST`.
- [x] Tests (35 total): LMStudioBackend payloads/parsing/think-strip/uncapped-retry/SSE
      via httpx.MockTransport; agent retry against stub backend; chat endpoint backend
      plumbing. Verified live: real LM Studio model listing on :1234, full pipeline + chat
      + think-hiding against a simulated OpenAI-compatible server.

## Phase 16 — Grounded research + Ollama cloud models (2026-07-16)
- [x] **Retrieval grounding** (top backlog item): `orcastrator/retrieval.py` —
      `WikipediaRetriever` (keyless REST APIs; DuckDuckGo scraping proved brittle/blocked).
      Per question: search → fetch top-3 plain-text extracts → hand to the Researcher as
      numbered sources with a cite-every-claim instruction; sources re-attached to every
      revision/rebuttal prompt (stateless calls). Critic verifies claims against the same
      sources (system prompt gains source-checking when web research is on).
- [x] **Traceable references**: per-section citations renumbered into report-wide numbering
      (`_renumber_citations`), References section appended programmatically — a URL only
      appears if its content was actually fetched (link verification by construction; the
      model never writes URLs). Unsourced sections are explicitly marked. Writer instructed
      to preserve [n] markers through draft/rewrite.
- [x] **Ollama cloud models**: curated `OLLAMA_CLOUD_MODELS` shown in the picker under an
      "Ollama Cloud — needs `ollama signin`" group; `ensure_model()` auto-pulls the manifest
      on first use with a clear signin error; cloud models skip the local memory check and
      run at full research parallelism. CLI `--model x:…-cloud` works the same.
- [x] UI: "Web research" checkbox (default on), "Searching" question status, markdown link
      rendering for references. CLI: `--no-web`.
- [x] Tests (44): retrieval parsing/truncation/failure, grounded-run prompts + renumbering +
      references, unsourced fallback, cloud-tag detection, ensure_model pull path.

## Phase 17 — Multi-source retrieval + clickable inline citations (2026-07-16)
- [x] Beyond Wikipedia: `MultiRetriever` merges Wikipedia + OpenAlex (scholarly papers,
      abstracts rebuilt from inverted index, DOI links) + DuckDuckGo Instant Answer API
      (the official JSON API — the HTML endpoint serves a bot CAPTCHA). Results deduped by
      URL, capped at 4/question, one failing engine doesn't sink the others. Non-Wikipedia
      URLs get a HEAD liveness check so every reference is still verified.
- [x] Clickable inline citations: `_linkify_citations` turns `[n]` into `[[n]](url)` →
      rendered as superscript `[n]` links in the report body (new `a.cite` styling);
      References list links too. `_strip_citations` for the citations-off path.
- [x] "Inline citations" checkbox (default on, shown only when Web research is on) and
      `--no-citations`: removes body `[n]` markers but keeps the verified References section.
- [x] Tests (52): abstract reconstruction, multi-retriever merge/dedupe/failure-tolerance,
      linkify + strip, grounded run inline-links, citations-off keeps references.

## Backlog — future features (not started, unprioritized beyond order below)
- [ ] **Cloud LLM providers (multi-provider expansion).** Beyond the local backends
      (Ollama, LM Studio, Jan.ai), add backend classes for OpenAI, Google Gemini,
      X.ai (Grok), and OpenRouter behind the same Backend interface, selected via the
      existing `--backend` flag / web UI Server dropdown. API keys via environment
      variables (e.g. `OPENAI_API_KEY`, `GEMINI_API_KEY`, `XAI_API_KEY`,
      `OPENROUTER_API_KEY`), documented per provider. Goals: provider flexibility,
      unified interface (the orchestrator stays provider-agnostic), and backward
      compatibility for existing backends. Tests: per-provider list/ensure/generate
      unit tests plus backward-compat coverage. (Folded in from a scratch plan file.)
- [x] **Real retrieval grounding.** Done — Phases 15/17: multi-source web retrieval
      (Wikipedia, OpenAlex, DDG, arXiv, Semantic Scholar, Stack Exchange, HN) with
      verified links, inline citations, and critic source-checking.
- [x] **Bounded concurrency.** Done — Phase 14: memory-aware `asyncio.Semaphore`
      fan-out with `--max-concurrency` override.
- [ ] **Resumable runs.** A run can take ~10+ minutes; a crash/timeout partway through means
      redoing everything. Consider checkpointing planner output + per-question results to a
      `.orcastrator_state.json` so a run can resume instead of restarting.
- [x] **Live progress output.** Done — Phase 10: web dashboard with elapsed timer, stage
      tracker, and per-question status badges (CLI still logs via `--verbose`).
- [ ] **PDF/HTML export.** Still deferred per `Plan.md` — a `pandoc` subprocess on the produced
      Markdown, skipped gracefully if `pandoc` isn't installed.
- [ ] **Outline-then-per-section Writer drafting.** Still deferred — revisit only if
      `_budget_sections` truncation proves insufficient for long topics in practice.
- [ ] **`--version` flag** using `importlib.metadata.version("orcastrator")`.
- [ ] **CLI-level tests.** `tests/` covers parsing/orchestrator logic and (opt-in) full
      integration, but nothing exercises `cli.py`'s argument parsing or file-output path.

## Phase 19 — Jan.ai backend (2026-07-16)
- [x] `orcastrator/backends.py`: new `JanBackend` for Jan.ai's local OpenAI-compatible API
      (no auth), modeled on `LMStudioBackend` — same `generate`/`chat_stream`/`list_models`/
      `capabilities`/`loaded_model_bytes`/`close` interface, default host `http://localhost:1337`
      (override via `$JAN_HOST`), `default_concurrency = 2`.
- [x] `ensure_server()` detect-and-error: Jan has no CLI to start the server, so it probes
      `/v1/models` and raises a clear message pointing at Settings → "Local API Server" when
      down; `ensure_model` is a no-op (models load via Jan's Hub / local-files UI).
- [x] Orchestrator calls `ensure_server()` up front in `run()` (no-op for Ollama/LM Studio),
      emitting an info note when it returns one; `<think>` stripping and uncapped-retry-on-empty
      mirror LM Studio.
- [x] CLI: `--backend jan` added to choices; `--host` help text updated; description
      updated. Web UI: new "Jan.ai" option in the Server `<select>` and the JS `BACKENDS`
      map (label "Jan.ai", default host `http://localhost:1337`, no fallback model). Page
      copy mentions Jan.ai.
- [x] README: "Backends" section and `--backend`/env-var docs updated for Jan.ai; new
      `JAN_HOST` variable documented.
- [x] Tests: `JanBackend` generate/list/chat-stream/ensure_model/ensure_server (up + down)
      via httpx.MockTransport; `BACKENDS` registry includes all three backends;
      `create_backend("jan")` works.

## Phase 20 — Expand web research sources + configurable engines (2026-07-16)
- [x] `orcastrator/retrieval.py`: five new keyless retrievers — `SearchEngineRetriever`
      (DuckDuckGo HTML web search: ranked links + snippets, `_extract_uddg` decodes the
      `/l/?uddg=` redirect, link-alive check drops dead results), `ArxivRetriever`
      (Atom feed → title/abstract/abs URL), `SemanticScholarRetriever` (Graph API, 429
      backoff, falls back to the paper page when no `url`), `StackExchangeRetriever`
      (Stack Exchange API, default site stackoverflow; entity-unescaped title + excerpt),
      `HackerNewsRetriever` (Algolia API; skips Ask HN posts with no external URL).
- [x] `MultiRetriever` is now engine-driven: `enabled: list[str] | None` builds only the
      named engines from an `ALL_ENGINES` / `_ENGINE_REGISTRY` registry; `None` = all;
      unknown names logged and skipped (no crash). `MAX_SOURCES` raised 4 → 5.
- [x] Source selection flows through: `StormOrchestrator(sources=...)`, CLI `--sources`
      (comma-separated, validated against `ALL_ENGINES`, `parser.error` on unknown), and
      web `/api/research?sources=`. Web UI gains a **Research sources** toggle group under
      Web research (DDG Instant off by default to avoid dupes with DDG web search).
- [x] README: "Grounded research" now lists all eight engines + documents `--sources` and
      the source toggles; usage example added.
- [x] Tests: each new retriever covered via httpx.MockTransport (parsing, snippet/title
      cleanup, dead-link drop, 429 retry, Ask-HN skip); `MultiRetriever` subset selection,
      unknown-name ignore, default-all, and empty-selection paths. Old merge/dedup/failure
      tests updated to the `_engines` dict.
