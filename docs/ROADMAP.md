# Roadmap

Each phase ends with a demo and acceptance checks. Durations assume 1–2 developers part time and are rough.

## Development model per task
Work is grouped into blocks so the model changes as rarely as possible. At the end of each block the agent stops and says which model the next block needs.

- **Opus 5.5 (high):** decisions and anything hard to undo or easy to get subtly wrong:
  - spike analysis and the decision write-ups (results, ADRs, spec changes);
  - the Go skeleton and the core interfaces that later code copies;
  - security (sandbox, permissions, approvals, local API protection);
  - concurrency (agent loop, daemon, scheduler, model swaps);
  - the learning loop (synthesis, verification, promotion, bandit);
  - reviewing each phase before its exit check.
- **Sonnet 5.5 (high):** well-specified work that follows a spec or an existing pattern:
  - spike harnesses and test runs;
  - CLI commands, config and SQLite plumbing;
  - further backends once the first exists;
  - built-in skills written to their spec;
  - packaging and CI;
  - tray and status UI;
  - tests and doc updates.

| Block | Model | Covers |
|---|---|---|
| 0a | Sonnet | Name and code-signing checks. S2, S4, S5, S6, S7 and S8 harnesses, runs and raw tables. S6 needs your qBittorrent export first. S7 and S8 need a Windows and a Linux machine |
| 0b | **Opus** | S9 sandbox feasibility. Write-ups and decisions for S2 and S4–S9. Phase 0 go/no-go |
| 1a ✓ | **Opus** | Go skeleton: package layout, config layering, SQLite schema and migrations, `internal/platform` interfaces, backend and `decide` interfaces, agent loop with loop guards, orchestrator v1, permission broker and sandbox, proxy auth (Host, Origin and token checks). **Done 2026-10-15**; see "Block 1a hand-off" below |
| 1b ✓ | Sonnet | `ollama` and `openai` backends, the tool-strategy probe, skill loader and selector, script runtime, full CLI, the rest of the proxy API surface, approvals queue and notifications, daemon service install per OS, memory and outcome capture, the 5 built-in skills, release pipeline |
| 1c ✓ | **Opus** | Phase 1 review against SPEC §6. Fix what the review finds. **Done 2026-10-15**: built the scheduler and run continuations; see "Block 1c hand-off" below |
| 2a ✓ | **Opus** | Trace format, skill synthesis, verification gate, promotion and rollback, refinement and curator, memory slots and swap rules, bandit, escalation, `code-runner` sandbox. **Done 2026-10-15**; see "Block 2a hand-off" below |
| 2b ✓ | Sonnet | Benchmarks and model profiles, the `noskill eval` harness and its ~60 tasks, WASM runtime, status page, tray app, `route explain` output (MCP dropped). **Done 2026-10-16**; see "Block 2b hand-off" below |
| 2c ✓ | **Opus** | Phase 2 review and the learning exit check. **Done 2026-10-16**: the exit check ran and **failed** (+9% success from one task, −10% median steps); see "Block 2c hand-off" below |
| 2d ✓ | **Opus** | Learned skills that cut steps: draft pass (d) script tools, near-duplicate families, retrieval, and the exit check again. **Done 2026-10-16**: the exit check narrowly failed its own rule (median steps −45%, but success 100% → 92% from one task that used no learned skill) and was **accepted as passed** on the Phase 2 exit wording; see "Block 2d hand-off" below |
| 3 | Sonnet | Preconditions, external drives, `anime-tracker` end to end, editable approvals, `watcher` and `local-docs`, skill import. **Opus** reviews the file moves and trash handling (no hard deletes) before the week-long run |
| 4 | Sonnet | MLX and vLLM adapters, `/v1/systemone` passthrough. **Opus** for automatic threshold tuning (**done 2026-10-16**, ahead of the rest; see "Block 3 progress and Block 4 calibration" below) |
| 5 | **Opus** | Registry signatures, LoRA and DSPy/GEPA pipelines, cloud fallback rules. Sonnet for the docs site and authoring guide. Split into 5a–5d below |

## Phase 0: Validate (~3 weeks)
Detailed experiments, datasets and pass/fail numbers: [PHASE0_TESTS.md](PHASE0_TESTS.md) (S1–S9).

- [x] Answer key [OPEN_QUESTIONS](OPEN_QUESTIONS.md) (Q1 JEV, Q3 standalone, Q5 qBittorrent RSS, Q7 16 GB).
- [x] Remaining questions answered: name `noskill`, Go, MIT, learning ask-then-auto, cloud opt-in later, interfaces CLI + proxy + tray.
- [x] Check `noskill` name availability ([PACKAGING.md](PACKAGING.md#name-and-signing-checks)): package names are free; the GitHub org `noskill` and the main domains are taken, so you choose an alternative.
- [x] Check Windows code-signing options: SignPath Foundation (free for OSI-licensed projects) first, OV certificate as fallback, Azure Artifact Signing only for US/Canada individuals or qualifying organisations ([PACKAGING.md](PACKAGING.md#name-and-signing-checks)).
- [ ] Enrol in the Apple Developer Program ($99/year, yours to do).
- [ ] Build datasets: `tools-50`, `router-200`, `noul-40` done; `router-200` second labeller in progress; `rss-100` needs your qBittorrent export.
- [x] S1 Tool calling on small models: native vs constrained vs prompted → native default, constrained fallback ([results](spikes/S1-results.md), [ADR-0005](adr/0005-native-tool-calling-default.md)).
- [x] S2 Proxy compatibility and overhead → one port for `/v1` and `/api` works; +0.1–0.6 ms; findings for the real proxy ([results](spikes/S2-results.md)).
- [x] S3 JEV decision models on 16 GB → `tev1:0.8b` asking one question on 16 GB, `tev1` above, prompt-cache ceiling ([results](spikes/S3-results.md), [ADR-0006](adr/0006-decision-model-on-16gb.md)).
- [x] S4 Memory slots and swap cost → 8 GB budget on 16 GB, 4B default primary, swaps 4–7 s for 7–8B ([results](spikes/S4-results.md), [ADR-0007](adr/0007-16gb-memory-budget-and-model-defaults.md)). W1 and cold-from-reboot not run.
- [x] S5 Hermes Agent teardown → "beat this" table; ADR-0003 stands ([results](spikes/S5-results.md)). Windows/Linux installs not tested.
- [ ] S6 qBittorrent Web API + `same_series` matching on your own feeds.
- [ ] S7 Daemon, scheduler and actionable notifications on each OS. macOS part partly run ([S7-results.md](spikes/S7-results.md)): launchd install/uninstall passes without admin; notification shown (user-reported) but no button click logged; sleep/wake and reboot are still open. Windows and Linux not run.
- [ ] S8 Tray app on each OS.
- [ ] S9 Sandbox feasibility on each OS (no admin; Windows loopback/domain filtering answer). **macOS done**: all attacks blocked, +41–45 ms ([results](spikes/S9-results.md), [ADR-0008](adr/0008-macos-script-sandbox.md)). Linux and Windows open.
- **Exit (go/no-go):** S1 and S3 give a clear default tool strategy and decision model, S2 passes, S7 passes at least without notification buttons, S9 passes on macOS and Linux. Other failures become Phase 1 risks, not blockers.
- [x] Go/no-go (2026-10-15): **conditional go** for block 1a. S7 and S9-Linux haven't run; they become carry-overs with deadlines ([PHASE0_GO_NO_GO.md](spikes/PHASE0_GO_NO_GO.md)).

## Phase 1: MVP core (v0.1, ~6 weeks)
- [x] Go skeleton: config with layered precedence + `config check` ([CONFIG.md](CONFIG.md)), SQLite schema + migrations ([DATA_MODEL.md](DATA_MODEL.md)), logging, `doctor` (backend checks join `doctor` in 1b).
- [x] Backends: `ollama`, `openai`. Model discovery + tool-strategy probe (FR-B1–B3).
- [x] Skill loader (agentskills.io-compatible), selector with embeddings, progressive disclosure (FR-S1–S3).
- [x] Tool runtimes: builtin, script (uv, sh/pwsh) (FR-S4).
- [ ] Permissions enforcement v1: built-in broker, copy-in workspace, egress proxy, per-OS sandbox as proven in S9 (FR-S6, [SECURITY.md](SECURITY.md)). Done on macOS; the Linux and Windows sandboxes wait for their S9 runs.
- [x] Agent loop with small-model mode + traces (FR-A1–A4).
- [x] `decide` primitive: `jev` adapter for Ollama `/v1/systemone` + emulated fallback (FR-B6, FR-B6a).
- [x] Orchestrator v1: rules + heuristics + JEV decision classifier (FR-O1–O2).
- [ ] Proxy per [PROXY.md](PROXY.md): OpenAI + Ollama APIs on one port, model names, progress streaming, Host/Origin/token checks, approvals in chat, client tools `merge`; tested with Open WebUI (FR-I2, FR-I3). Built except client tools `merge`; both endpoints checked live with curl, not yet with Open WebUI itself.
- [x] Full CLI per [CLI.md](CLI.md), incl. `approvals`, `schedule` and `jobs` (FR-I1).
- [x] Approvals queue + plain OS notifications (FR-A6); action buttons wait for the tray.
- [x] Daemon + scheduler + OS service install + notifications (FR-J1–J4). Built in block 1c; a real install and reboot on each OS are still open (S7).
- [x] Memory v1 (FR-L6), outcome capture v1 (FR-L1).
- [x] Built-in skills: `web-search`, `research`, `files`, `orchestrator`, `model-manager`.
- [ ] Release pipeline per [PACKAGING.md](PACKAGING.md): 6 targets, signing + notarisation, checksums + signatures + SBOM, brew tap / winget / Scoop / per-user MSI / install scripts, `noskill update` + `uninstall`.
- **Exit:** SPEC §6 acceptance criteria pass on all three OSes.

### Block 1a hand-off (2026-10-15)
Built and tested on macOS. `go vet` is clean on all six targets, and Linux and Windows cross-build.
- **Exists:**
  - config layering + `config check [--explain]`;
  - SQLite schema 1 with backups before migrating;
  - platform paths, trash, memory and the macOS sandbox;
  - backend and `decide` interfaces with the JEV and emulated deciders;
  - agent loop: repeat, step, token and time guards, context fitting, redacted JSONL traces;
  - orchestrator v1: heuristics + decision classifier, memory budget, rules, soft-swap and hold rules, `Explanation.Text()` for `route explain`;
  - permission broker: scopes, path tokens, host checks, file ops with the undo log;
  - copy-in workspace, egress proxy and script runner (curl through the proxy tested inside the macOS sandbox);
  - proxy Host/Origin/token middleware;
  - `noskill version | config check | doctor`.
- **For 1b to wire up:**
  - The `ollama` backend feeds `orchestrator.State` (from `/api/ps`) and `Profile`s.
  - `noskill secrets` gives `config.Check` and `server.NewAuth` a keychain.
  - The skill loader compiles `noskill.permissions` with `perms.Resolver` and narrows them per call with `perms.Narrow`.
  - The script runtime builds `sandbox.Call`s. The daemon starts one `sandbox.Egress`.
  - The proxy handlers sit behind `server.Auth.Wrap`.
  - `platform.Volumes` has no OS implementation yet, so `volume:` tokens fail with "not supported".
- **Known gaps, written down:**
  - The macOS memory watchdog polls `ps` every 50 ms (ADR-0008).
  - The no-replace rename has a race, and moves across drives handle files only (SECURITY.md).
  - Logprob confidence for emulated decisions isn't wired up yet (MODEL_ADAPTERS.md).
  - Linux and Windows have no script sandbox until their S9 runs (`doctor` warns; `[sandbox] mode = "warn"` runs scripts unconfined).
  - Windows path checks (UNC, `\\?\`, ADS, junctions) and the Windows trash have only been vetted, not run.

### Block 1b hand-off (2026-10-15)
Built and tested on macOS (all tests, `-race`, six-target vet, Linux/Windows cross-builds). Checked against the real Ollama 0.40.3 with `llama3.2:3b` and `qwen3:4b`: `init`, `doctor`, native tool calls through the broker, `models probe` (native kept for both: 6/10 native vs 4/10 and 6/10 constrained), `models bench` (25 tok/s, 3.0 GB at 8k), and `serve` answering OpenAI streaming, plain passthrough and Ollama-format requests.
- **Done:** `ollama` and `openai` backends; tool-strategy probe; skill loader and keyword/embedding selector; script runtime and builtin tools; the 5 built-in skills; approvals (queue, terminal, chat, notifications as plain messages); memory and outcome capture; the run engine; the proxy (`/v1`, `/api`, `/noskill/v1`); the CLI; `daemon` install per OS; `update` with ed25519 verification and rollback; `uninstall`; GoReleaser, CI workflows and install scripts.
- **Not verified:** a real `daemon install` on any OS (tested with a fake runner and generated files only); the Windows and Linux code paths (compiled and vetted, not run); an Open WebUI session; the embedding path (`nomic-embed-text` isn't installed here, so selection used keywords); web search against a live provider.
- **Release setup, deferred on purpose:** this is a personal project with no release planned soon. Apple Developer enrolment, Windows signing, the GitHub org and repository (the module path `noskill` and `update.repo` stay placeholders) and the ed25519 release key (`go run ./internal/update/cmd/sign -gen`) are left until the project is more complete. The release files (GoReleaser, workflows, install scripts, `update`) are built and tested but unused until then.
- **Known gaps:** scheduler (`schedule`, `jobs`, `FR-J1/J3`) isn't built; macOS notification buttons (S7 unproven, so plain messages); client-side tools `merge`; `response_format` on agent runs; `/ui`; logprob confidence for emulated decisions; `volume:` label spoofing protection (labels only, no UUID); PDF extraction in `fetch`; per-task benchmark quality scores (routing quality is flat until Phase 2b).

### Block 1c hand-off (2026-10-15)
Phase 1 reviewed against SPEC §6 on macOS with Ollama 0.40.3. All tests pass (also with `-race`), and `go vet` is clean on all six targets.

| § 6 | Result on macOS |
|---|---|
| 1 `init` with no manual config | Pass (fresh home; no downloads accepted) |
| 2 cited web-search answer, 4B at 8k | Pass with `qwen3:4b` and the `ddg` provider (opt-in; no keyless provider is on by default). About 3 minutes, because this `qwen3:4b` always thinks |
| 3 same through both endpoints | Pass with curl on `/v1/chat/completions` and `/api/chat` (`noskill/auto`, streaming, `X-Noskill-Skills: web-search`). Open WebUI itself not run |
| 4 `code` rule + `route explain` | Pass. `qwen2.5-coder` isn't installed, so `qwen3.5:4b` stood in; with the real name, `route explain` says the rule is skipped because the model isn't installed |
| 5 job runs with the CLI closed, survives reboot, notifies | Built ([ADR-0009](adr/0009-scheduler-through-the-database.md)). Tested end to end with a real `serve` process and a scripted model; crash recovery tested in `scheduler`. A real `daemon install` and reboot weren't run (that changes your login items) |
| 6 out-of-manifest read blocked and traced | Pass live (`qwen3:4b` + `files`) and as a test; `traces show` lists the denied call |
| 7 job waits on `confirm`, notifies, continues after approve | Pass as an end-to-end CLI test (real `serve`, scripted model): the job parks, the approval notification goes out, and after `approvals approve` the run continues from the pending call |

- **Built in 1c:**
  - the scheduler (`schedule`, `jobs`, cron and `every:`, missed and overlap policies, timeouts, retries with backoff, crash recovery, a lease so one process schedules);
  - run continuations: a parked run resumes from its pending call, used by jobs, `run --resume` and chat;
  - `BEGIN IMMEDIATE` transactions, so the daemon and the CLI don't fail each other with `SQLITE_BUSY`.
- **Fixed from the review:**
  - A weak keyword skill match no longer sets the task type. "write a python function…" picked `files` and hid the `code` rule.
  - `route explain` now prints the classifier's note and names task-type rules whose model isn't installed.
  - Thinking-only models (the template always opens `<think>`, as in this `qwen3:4b`) are detected (`think-always`). They get `think=true` and a 2048-token step, because with `think=false` Ollama returns the reasoning as content and the tool call never comes.
  - Reasoning that leaks into the content before a bare `</think>` is moved out of the answer.
  - A step that runs out of tokens while thinking is asked once more to act.
- **Known gaps:**
  - Jobs don't yield between steps (PROXY.md "Concurrency").
  - Forced-answer steps (tools off) stream as they arrive, so leaked `</think>` text can still show while streaming there; the stored answer is clean.
  - The selector's keyword fallback still adds `files` to some code questions (harmless, costs context); embeddings fix it once `nomic-embed-text` is installed.
  - `init` doesn't yet suggest a non-thinking 4B model when the installed one always thinks.

### Block 2a hand-off (2026-10-15)
Built and tested on macOS: all tests (also with `-race`), and `go vet` is clean on all six targets. Checked live against Ollama 0.40.3 with a fresh home:
- `run --model qwen3.5:4b` on three math questions picked `code-runner` and answered correctly with `run_python` in the sandbox. One of them recovered from a missing `sympy` by following the error hint.
- `learn` drafted each run into a candidate skill in 26–64 s, with 3 of 3 tests passing once the variations fix was in.
- `learn --later` and `learn queue` work.

- **Built:** trace v1 and its reader; outcome tiers incl. the LLM judge and `outcome_check` approvals; mining, the learning queue and the worker; the skill-author passes in Go; the verification gate with replay fixtures and `required` cases; the learned library ([ADR-0010](adr/0010-learned-skill-storage-and-precedence.md)), trial (`ask`/`auto`), promotion, versions, `skills versions|diff|rollback`; refinement through `.next`; the curator (stale, archive, merge); the swap rules incl. rule 6 (jobs wait while you're active), rule 7 (batch `delegate`) and rule 8 (driver hand-off); the bandit (FR-O4/L7) and escalation on hard failures (FR-O5); the `code-runner` skill with `run_python`.
- **Fixed from the live check:**
  - `code-runner` had no triggers, so without an embedding model a plain math question never selected it. It has triggers now, but not "python", which would hide the `code` routing rule.
  - A step that was still only thinking after the nudge ended as an empty "answered" with exit 0. It's now a `max_tokens` stop: the CLI says so, and escalation can take it.
  - The variations schema sent `required: null`, which Ollama refuses, and an empty `values` object, which a small model pads until it runs out.
  - Steps written from the trace no longer quote the run's program.
- **Not verified:** `run_python` contained on Linux and Windows. The script sandbox exists only on macOS (ADR-0008), and the tests skip elsewhere. The analysis venv (`skills prepare code-runner`) wasn't installed live, because it downloads packages; it's tested only for the hint. A real promotion over days of use, and the curator on a real library, are covered by tests only.
- **Known gaps:**
  - On 3–4B models, the steps pass fails for single-`run_python` runs, which then get generic steps. A learned skill for those adds little over `code-runner` itself. `learn` on such a run is the user's choice (the miner needs ≥ 5 calls), but drafting could skip them.
  - Draft pass d (new script tools) is deferred.
  - Escalation on a `looks_correct` check, and the cascade, aren't built.
  - `shadow` promotion behaves like `ask`.
  - `noskill eval` and judge statistics move to 2b.
  - `route explain` doesn't show the bandit evidence yet (2b).
  - The emulated LLM judge's confidence is a fixed 0.6.
  - This `qwen3:4b` still thinks through its whole step on some tool steps (1c gap); the run now reports it instead of answering empty.

### Block 2b hand-off (2026-10-16)
Built and tested on macOS: all tests (also with `-race`) and `go vet` on all six targets. MCP was dropped from the project at the start of the block (SPEC FR-I4/I5, docs).
Checked live against Ollama 0.40.3 with a fresh home:
- `models bench qwen3.5:4b`: speed plus the 12-task micro-eval in 25 s (chat 0.83 · code 0.83 · extract 0.83 · math 0.50 · reasoning 0.50 · summarise 0.75 · translate 0.75; the two misses are real wrong answers). `route explain` then prints quality, rated runs and expected time for each candidate.
- `eval run --type files --task code-runner-01` on `qwen3.5:4b`: 11/11 agent tasks passed (2.2 steps and 11 s a task), with real tools, files and the sandboxed Python. `qwen3:4b` (always thinks) passed a short plain run.
- The status page at `/ui` in the browser pane, with approvals, jobs, runs, models, skills, learning and eval sections.
- The tray binary starts and stays up (offline state, since no tray token was needed). The menu itself wasn't looked at.

- **Built:** the eval package (61 tasks, 10 check kinds, report, comparison); `eval list|run|report|history|pin|gate`; per-task-type quality in the profiles; `route explain` numbers; the weekly eval and the gate with auto-rollback of learned skills; the WASM runtime on wazero; `/noskill/v1/status`, job control and the `/ui` page; admin tokens; the tray (`internal/tray` plus the thin `cmd/noskill-tray`, with a drawn icon on every OS).
- **Deviations:** the tray and the page use the loopback HTTP API instead of an IPC socket ([ADR-0011](adr/0011-tray-and-status-page-over-http.md)). `eval_results` from migration 0001 was never written, so migration 0005 replaces it.
- **Not verified:** the tray on Windows and Linux (it compiles and vets for both; GNOME needs the AppIndicator extension), the tray menu on macOS (only that it starts), a weekly eval fired by the real schedule, the regression gate on a real learned skill (tested with a scripted model and a hand-made library).
- **Known gaps:**
  - `daemon install --tray` (a login item) isn't built.
  - The WASM runtime has no network and no host functions, so a wasm tool can't call out. The fixture is built with Go's wasip1 at test time (skipped without a `go` binary).
  - Models that think a lot (`qwen3:4b`) take minutes for the full suite; there's no progress persistence, so a run that a request pre-empts restarts (not retried for 6 hours).
  - Task types the micro-eval doesn't cover (research, plan, creative, files) use the mean as quality; `models bench` doesn't run the agent tasks.
  - A pinned task has no access to your files.
  - The decision model's accuracy isn't benchmarked (FR-B4 part).
  - `route explain` doesn't show the bandit's prior and evidence separately, only the posterior mean and the count.

### Block 2c hand-off (2026-10-16)
Built and tested on macOS: all tests (also with `-race`) and `go vet` on all six targets. Checked live against Ollama with fresh homes.

- **Exit check: FAIL.** `noskill eval simulate --model qwen3.5:4b --days 28 --per-day 3` (84 simulated requests, 75 rated good; about 1 h 40 min of real time, since "28 days" is simulated use). Held-out multi-step suite (12 tasks): success 92% → 100% (+9%), median model steps 5.0 → 4.5 (−10%), average steps 4.7 → 4.8. Learned skills took part in 8 of 12 tasks. Report: `spikes/results/2c-exit-check-qwen3.5-4b.txt`.
  - The success gain is one task (`multi-05`) that used no learned skill: model variance, not learning. Where a learned skill took part, steps stayed the same or rose (`multi-02` 4 → 6).
  - Why: a learned skill writes down the sequence the model already finds (list, make the folder, move with a dry run, move), so on a 4B model that already does these chores it can't save turns. Step cuts need draft pass (d) (one script tool per repeated chore), which is still deferred.
  - Other models: `qwen3:4b` thinks on every step (≈ 5.5 min per task, ≈ 12 h for the check), so it wasn't run to the end. `llama3.2:3b` passed 0 of 12 held-out and 0 of 6 workload requests, so there was nothing to learn from.
  - 11 skills learned: 5 active, 5 candidates, 1 archived (`prefix-files`, 2 failed trial runs). No refinements, merges or retirements in 4 curator passes.
- **Built:** `eval simulate` and its held-out workload (18 training requests, 6 families); 12 `multi` eval tasks (73 in all); learned skills in eval agent tasks (`--no-learned` for the baseline), recorded per result (migration 0006); a regression gate that rolls back only learned skills that took part in a task that went from pass to fail.
- **Changed for your machine ([ADR-0012](adr/0012-shared-machine-6gb-budget-with-burst.md)):** models get 6 GB on 16 GB Apple Silicon (half the machine is your other work), growing toward Metal's ⅔-of-RAM limit (10.7 GB) only while memory is free and pressure is normal. A model loaded only because of burst is held 2 minutes after its run. Under 6 GB, `qwen3:4b` no longer fits next to `tev1:0.8b`; a 3B model is the base primary.
- **Findings fixed:**
  - ADR-0006's prompt-cache cap was never built. During the trial, Ollama's MLX runner grew qwen3.5:4b from 5.0 to 12.7 GB. noskill now unloads any of its own models more than `decision_cache_gb` over the smallest size seen after a run. Models you loaded are never touched.
  - Learned steps copied the source run's file names (`read_text {path}/x.txt`) and wrote pseudo-code that the model then answered with as text. Repeated calls are now shown once with `<each item>`, and steps that quote per-item values or code fall back to steps written from the trace.
  - The retrieval check called the same chore done in another tool order "unrelated", which rejected good skills. Runs now count as related when their tool sets overlap by more than half.
- **Known gaps (for the follow-up):**
  - Draft pass (d), script tools for deterministic steps, which is what can cut steps.
  - Near-duplicates aren't merged: `organize-files-by-type` -2 and -3 and `organize-draft-files-2` were all drafted, and two of them became active. The novelty check passes same-family drafts, and the merge pass compared the wrong pair.
  - Retrieval picks the wrong learned skill for some held-out requests: `filter-error-lines` for combine-text, and nothing for the archive tasks, whose prefixes differ from the training ones.
  - `merge-text-files` stayed a candidate for 28 days. Candidates get only a few chances, so a family can end the month with nothing active.
  - The pass rule on 12 tasks moves in 8-point steps (one task), and a baseline at 92–100% leaves no room for the success half, so only a step cut can pass on a good model.
  - The burst budget's app plumbing (hold and release) is tested only in the orchestrator, not end to end. The 2 GB reserve is a guess.
  - The eval progress counter counts a task before it runs. "Steps" are model turns.

### Block 2d hand-off (2026-10-16)
Built and tested on macOS: all tests (also with `-race`) and `go vet` on all six targets. Checked live against Ollama with fresh homes.

- **Exit check: accepted as passed (2026-10-16).** By the check's own rule it was a narrow FAIL; the user accepted it because the Phase 2 exit wording ("success and/or steps") is met by steps −45%, and the one failed task used no learned skill. `noskill eval simulate --model qwen3.5:4b --days 28 --per-day 3` (run 4: 84 simulated requests, 80 rated good, about 1 h 45 min). Held-out suite (12 tasks):
  - Median model steps 5.5 → 3.0 (**−45%**); average 5.2 → 3.8.
  - Tokens per task −31%, time per task −33%.
  - Success 100% → 92% (−8%). The rule passes on steps only when success doesn't fall, so this is a FAIL.
  - Learned skills took part in 10 of 12 tasks.
  - Report: `spikes/results/2d-exit-check-qwen3.5-4b.txt`.
  - The failed task, `multi-01`, used **no** learned skill. Its skill was archived after 2 real trial failures, and the 4B model with the built-in files skill alone missed a file. That is the same one-task model variance the 2c pass came from.
  - Where a script tool ran, a 5–6 step task took 2 (`multi-05`/`06`, `multi-09`/`10`). Plain-step skills kept about the same count.
  - Whether this counts as passing is a decision about the rule, not something more code can settle. The ROADMAP's own wording ("success and/or steps") is met; the check's rule isn't. A rerun can move by one task either way.
- **Run 3 failed on a harness bug, fixed for run 4:** success stayed at 75%, steps 5 → 6.
  - Both script redrafts (`backup_files`, `rename_files_tool`) did their tasks in 2 steps, but every one of those runs was rated bad.
  - The eval's tool check didn't see a tool that only exists in a version on trial (`<name>/.next`), so both were set aside.
  - Report: `spikes/results/2d-exit-check-qwen3.5-4b-run3.txt`.
- **Built:**
  - **Draft pass (d), script tools** ([ADR-0013](adr/0013-script-file-ops-through-the-broker.md), [LEARNING.md](LEARNING.md#pass-d-script-tools)):
    - The author model writes a Python tool that prints file ops for the broker to apply: permission checks, no overwrite, OS trash, undo log, `dry_run`.
    - It's checked by replay in the macOS sandbox against the run's own file effects.
    - It replies in plain text, because inside JSON the 4B author looped until its tokens ran out.
    - Request values in the code are lifted into arguments, and code that writes files itself is refused.
  - **Near-duplicate families** (`internal/learning/family.go`):
    - A same-family draft is skipped as "covered", or upgrades a plain skill to a script version.
    - The curator keeps the script version of a pair.
    - In run 4, `invoice-folder-mover` was archived for `organize-draft-files`.
  - **Retrieval:** learned descriptions carry an example request, query paths are stripped, and the learned-skill threshold is 0.6.
  - **Eval `tools:` check:** a learned script tool that made the same kind of change counts as the required tool.
  - **Refinement:** revised steps may not write in a failed run's folder, names or prefix. In run 3 one did.
  - **Status page:** job cards from `spikes/results/jobs/*.json`.
- **Known gaps (for the follow-up):**
  - Pass d works on the 4B model about half the time. Repair often drops the tool and leaves `tools/*.py` in the skill folder, and the CHANGELOG still says "script tool written".
  - Pass d is macOS only (it needs the script sandbox).
  - `log_level` isn't wired into the CLI.
  - A plain "copy a.txt to b.txt" request still scores 0.88 against a backup skill.
  - The 12-task held-out suite moves in 8-point steps. A baseline at 100% leaves no room for the success half, and one unlucky task fails the steps half.

### Block 3 progress and Block 4 calibration (2026-10-16)
Built alongside 2d, keeping to files the learning work didn't touch. Tested on macOS: all tests, and `go vet` on all six targets.

- **Preconditions (FR-J5):** `schedule add --when volume:<label>|ac-power|unmetered|window:HH:MM-HH:MM`. A due run waits until they hold, or is recorded `unmet` when the next occurrence comes first.
- **External drives:** `internal/volumes` binds `volume:<label>` to the drive's UUID; `internal/platform` lists volumes and reads IDs on all three OSes. Run on macOS only.
- **Editable approval fields (FR-A6):** `fields` on approvals, `--set` in the CLI, a path in a chat reply, and a folder field in the tray and status page.
- **anime-tracker, run live on macOS with Transmission:**
  - Finds torrent apps installed for the user or system-wide (`platform.FindApp`); drives Transmission or qBittorrent over their web APIs.
  - Takes English-subbed releases from Nyaa's feed (`nyaa_url` for a mirror such as nyaa.media, whose magnets come from the view page) instead of qBittorrent RSS rules. The deviation is in the skill's SKILL.md.
  - Suggests only this season's or next season's new shows, and skips shows and episodes already in Videos, Downloads, Desktop, `library_dirs` and external drives (confirmed on AniList, cached).
  - Names files `<Title> S02E03 [480p].mkv`; `episode_style` = `ep` or `episode` gives `EP 3` or `Episode 3`.
  - Files through the permission broker, verifies copies, and trashes a download only when every video in it was placed.
  - The first live run sent 3 approval cards. No episode has been filed live yet.
- **Block 3, Sonnet part (2026-10-16):** tested on macOS (all tests, six-target vet).
  - **anime-tracker, model-driven:** builtins `anime_run`, `anime_status`, `anime_detect`; the skill is now embedded, so a model can start a run and read its report.
  - **Free space:** each copy needs the file's size plus 5% free on the destination drive; otherwise nothing is copied, the torrent stays, and the report says how much is needed and free.
  - **v2 replacement:** a fixed release of an episode noskill filed (same resolution) is downloaded and replaces it; the old file goes to the trash first. `replace_versions = false` turns it off.
  - **Notify-only:** `mode = "notify-only"` sends the suggestions as one notification and creates no approval cards.
  - **`watcher`** (migration 0008): `watch_add|list|check|remove`; pages (paragraph diff that ignores dates, counters and page furniture), RSS/Atom (ETag), GitHub releases, searches; each watch is a scheduler job; a decision-model relevance filter runs inside `watch_check`.
  - **`local-docs`**: `noskill docs add|list|index|remove|search` and tools `docs_index|search|read`. A separate rebuildable `docs.db` with FTS5 and, when an embedding model is installed, vectors, fused by reciprocal rank. Text, Markdown, HTML and Word files. **PDFs are not read**: no pure-Go PDF parser is a dependency yet, and that is a choice for you (see below). There is no file watcher yet; `docs index` refreshes incrementally.
  - **Skill import (FR-S8):** `noskill skills import` for Claude Code, Codex, Hermes and `.agents` folders, with a compatibility report; the folder is copied unchanged.
  - Not run live: watcher fetches against real sites, `docs` against a real embedding model, and the Hermes folder convention (`~/.hermes/skills`) is unverified.
- **Still to do in block 3:** a watcher/docs run and a real trash + `noskill undo` on Windows and Linux, PDF text for `local-docs`, and the week-long unattended run (Phase 3 exit; plan and daily checks in [WEEK_RUN.md](WEEK_RUN.md)). The review of file moves and trash is done (2026-10-18): ten fixes, decision "pass on macOS"; see [REVIEW_BLOCK3.md](REVIEW_BLOCK3.md). `noskill hook` (qBittorrent, Transmission) was built afterwards, 2026-10-18; see [CLI.md](CLI.md).
- **Block 4, Opus part: decision calibration (FR-B6c).**
  - `router.task_type` and `judge.answered` are stored in `decisions` (migration 0007) with their probability and threshold, and labelled from the run's outcome.
  - `noskill calibration` shows the reliability table, ECE and Brier score per decision and model.
  - Tuning uses Youden's J and moves only with ≥ 40 labelled decisions, ≥ 0.2 separation and ≥ 0.05 gain, by at most 0.10 a step within fixed bounds. Every change is kept in `decision_thresholds`; `calibration revert` undoes the last one.
  - `orchestrator.auto_tune` is off by default, so behaviour is unchanged until it's turned on or a tune is applied by hand. See [MODEL_ADAPTERS.md](MODEL_ADAPTERS.md#calibration).
  - Not yet tried on real outcomes: no decisions had been recorded before this.

## Phase 2: Learning + smart routing (v0.2, ~6 weeks)
- [x] Benchmarks + model profiles (FR-B4). Block 2b.
- [x] Trace format v1 + `skill-author` skill; synthesis, verification gate, ask-then-auto promotion, versioning, rollback (FR-L2–L3, FR-S9). Block 2a; storage in [ADR-0010](adr/0010-learned-skill-storage-and-precedence.md).
- [x] `code-runner` skill with per-OS sandbox. Block 2a; it uses the script sandbox, so it is contained on macOS only for now.
- [x] Refinement + curator (FR-L4–L5). Block 2a.
- [x] Memory slots + swap rules, learned routing bandit, escalation (FR-O3–O6, FR-L7). Block 2a; `route explain` shows quality and rated runs per candidate (2b).
- [x] `noskill eval`: 73 built-in checked tasks (12 multi-step added in 2c), pinned tasks, weekly run, regression gate with auto-revert. Block 2b.
- [x] WASM runtime (FR-S5). Block 2b.
- [x] Web status page (FR-I6). Block 2b.
- [x] Tray/menu-bar app v1: status, jobs, approvals, pause all (FR-I7, [ADR-0004](adr/0004-tray-app.md), [ADR-0011](adr/0011-tray-and-status-page-over-http.md)). Block 2b; not run on Windows or Linux, and no login-item install yet.
- ~~MCP client + server (FR-I4, FR-I5)~~ dropped; not planned.
- **Exit:** eval shows learned skills improve success and/or cut steps by ≥ 15% over 4 weeks of simulated use. **Met (accepted 2026-10-16).** Block 2d on `qwen3.5:4b`: median steps 5.5 → 3.0 (−45%). Held-out success fell 100% → 92% on one task, `multi-01`, which used no learned skill. The check's stricter rule (success mustn't fall when passing on steps) called that a narrow fail; the user accepted the result on this wording instead. The stricter rule stays in the harness, so later runs still flag a success drop; see the 2d hand-off.

## Phase 3: Automations + anime-tracker (v0.3, ~4 weeks)
- [x] Preconditions incl. `volume_mounted`, power, network (FR-J5). Block 3: `volume:<label>`, `ac-power`, `unmetered`, `window:`.
- [x] External drive support on all OSes. Block 3; run on macOS only.
- [ ] `anime-tracker` skill end-to-end (block 3: built and run live on macOS with Transmission and Nyaa; nothing filed live yet, and builtin tools for model-driven runs are still to do): approval per title with a folder you choose (tray, CLI, chat); qBittorrent RSS rules, 480p preferred with per-episode fallback; download to `~/Downloads` → copy, verify, rename into your folder → trash, one at a time until done.
- [x] Editable fields in approvals (folder picker in tray/status page, `--set` in CLI, path in a chat reply) (FR-A6). Block 3.
- [x] `watcher` and `local-docs` skills. Block 3; `local-docs` doesn't read PDFs yet.
- [x] Import skills from Claude Code / Hermes (FR-S8). Block 3; `noskill skills import`.
- **Exit:** a week of unattended nightly runs on each OS with correct naming and zero unexpected writes. Tools for it, built 2026-10-18: `scripts/week-audit.sh` and `noskill anime audit` (the morning check, PASS or FAIL), and `noskill doctor --files` (one real trash and undo, for Windows and Linux). `granite4:3b` ran the follow prompt correctly (30 s, dry run). Also built for it: a tabbed status page with a Downloads tab (`ask_before_download` questions, add a download by hand, finished torrents with "stop seeding and file now"), `noskill anime add|file-now`, and torrents added with a zero ratio limit so Transmission stops them the moment they finish. In source only: it needs the rebuilt binary. The macOS week starts with the 02:30 run on 2026-10-19; nothing here shortens it.

## Phase 4: Model breadth (v0.4)
- [x] MLX + vLLM adapters (FR-B5). Built 2026-10-18 ahead of block 4, tested against fake servers only; not yet run against a real `mlx_lm.server` or vLLM (installing them needs a download).
- [x] Proxy passthrough/emulation of `/v1/systemone` (FR-B6b). Built 2026-10-18 ahead of block 4.
- [x] Per-decision calibration tracking and automatic threshold tuning from outcomes (FR-B6c). Block 4, Opus part; `auto_tune` off by default.
- [ ] Optional embedding-predictive (JEPA-style) providers for video/image tagging: P2, only if there's demand.

## Phase 5: Ecosystem (v1.0)
Order chosen 2026-10-18: skill optimisation first (works now, offline), then the LoRA pipeline and the registry, both built but gated. Cloud fallback is skipped for now (no API key wanted).

| Block | Model | Work |
|---|---|---|
| 5a | **Opus** | Skill optimisation (FR-L9): GEPA-style loop in Go, scored by the checked tasks, tuned built-ins as an overlay ([ADR-0014](adr/0014-skill-optimisation.md)) |
| 5b | **Opus** | LoRA pipeline (FR-L8): `train export` and `train lora` with MLX; refuses to train until enough good traces exist |
| 5c | **Opus** | Skill registry with signatures (FR-S10), signed with a key you make locally (not the release key) |
| 5d | Sonnet | Docs site, skill authoring guide |

- [x] Skill registry with signatures (FR-S10). Block 5c: built and tested with local and git registries; none published yet (see the hand-off below).
- [x] Opt-in LoRA pipeline (FR-L8). Block 5b: built and gated; no real training run yet (see the hand-off below).
- [x] DSPy/GEPA-style optimisation (FR-L9). Block 5a: built and checked live end to end; no rewrite has been adopted yet (see the hand-off below).
- [ ] Optional cloud fallback backends, opt-in, never for scheduled jobs (FR-B7). Skipped for now (2026-10-18).
- [x] Docs site and skill authoring guide. Block 5d: built; nothing is hosted yet (see the hand-off below).
- [ ] Community skills: needs a published registry, which needs the GitHub org and a host (deferred).

### Block 5d hand-off (2026-10-18)
- **Built:**
  - `go run ./scripts/docsite` builds `site/` from the Markdown: 55 pages (the docs, the spec, every ADR, the built-in skills, the Phase 0 results) with a sidebar, light and dark themes and a phone layout. It uses `goldmark` (added to `go.mod`; it is only linked into this tool, not the noskill binary). Links between pages become links between the HTML files; a link to source code is marked as one (or goes to `-repo <url>` when you have a host); a link to a file that doesn't exist fails the build. `go test ./scripts/docsite` builds the real docs and fails on any.
  - `docs/SKILL_AUTHORING.md` brought up to date: the real list of built-in tools (the old one named tools that don't exist), what `skills test` and `skills export` are (not built), `skills optimise`, signing and registries, and a new section on trying a skill and reading its trace. README status and commands refreshed; `docs/CLI.md` is no longer titled "planned".
  - `docs-site` in `.claude/launch.json` serves `site/` on :8767.
- **Left out on purpose:** `REVIEW_BLOCK3.md` and `WEEK_RUN.md` (working notes for running the project), search, and hosting. The site has no domain: the GitHub org and domain are still undecided, so nothing is published.
- **Not done:** `noskill skills test|setup|export` are still unbuilt although the CLI doc lists them (it says so); the authoring guide says the same.

### Block 5c hand-off (2026-10-18)
- **Built** ([ADR-0016](adr/0016-signed-skill-registry.md)):
  - `internal/registry`: the manifest (`noskill-skill-v1`, name, version, sorted `sha256  path` lines), ed25519 signatures in `.noskill-sig.json` with the file list, and the signed `index.json`.
  - `noskill registry key new|show`: the seed lives in the OS keychain as `registry-signing-key`.
  - `registry publish|add|list|remove|update|search`. `add` needs `--key`; `update` refuses an index older than the last seen.
  - `skills install <registry>/<skill>`: checks the index against your key, the folder against the index's digest and its own signature, and records `registry:<name>/<skill>@<version> sha256:…` (plus the git commit).
  - Plain installs check a signature if there is one: yours or a registry's is trusted, a stranger's is shown as unknown, and a broken one is refused, naming the files that changed.
  - `skills sign <folder>` and `skills verify <name>`.
  - The git clone code is shared between `skills install` and registries (`app.GitFetch`).
- **Tested:** signing and every tamper case (an edited, added or removed file; a swapped key; a file list cut short; another name or version), links refused, index paths that leave the registry, a wrong key on `add`, a rollback, a registry in a local git repository through `file://`, and the CLI from key to install.
- **Not checked:** a registry on a real git host, and Windows and Linux keychains holding the seed (the same `internal/secrets` code as other secrets). No registry has been published.

### Block 5b hand-off (2026-10-18)
- **Built:**
  - `train export|lora|list|adopt|revert` ([ADR-0015](adr/0015-lora-pipeline.md)).
  - A dataset of good runs from real use: mlx-lm chat format with tools, one example per assistant turn, with secrets and the home folder scrubbed, plus a REVIEW.md.
  - The gate: `learning.lora.min_runs` (200), `--reviewed`, power, Apple Silicon and 8 GB free.
  - MLX through `uv run --isolated --with mlx-lm==0.26.0`, then fuse, then `ollama create <base>-ns<n>`.
  - A benchmark against the base on the checked tasks, under ADR-0014's rule (now with at least one pass needed for the fewer-steps half).
  - Trained models are left out of routing until adopted; an adopted one stands in for its base on its task type.
- **Tested:** export on hand-made traces, every gate, a whole run with uv and ollama faked, the benchmark, adopt, the routing swap, revert, and a rejected model removed. On a scratch copy of this machine's traces, export found nothing to train on: 9 runs judged at 0.60 and one at 0.75, below 0.8. With the bar raised in the copy, it wrote 3 runs and 7 examples with no home path in them. The dry run printed the commands and a 23-task benchmark.
- **Not checked:**
  - No real training has run, so the mlx-lm pin, the `fuse --dequantize` flags and Ollama 0.40 importing Qwen3/Llama safetensors are all unchecked.
  - uv would download mlx-lm and about 2.5 GB of weights.
  - The first real run needs 200 good runs. The judge's confidence on job runs (0.60) keeps them all out, so rating runs (`noskill rate`) is the way in.
  - The CUDA path (Unsloth) isn't built.

### Block 5a hand-off (2026-10-18)
Built and tested on macOS: all tests and `go vet` on all six targets.

- **Built** ([ADR-0014](adr/0014-skill-optimisation.md), [LEARNING.md](LEARNING.md#prompt-optimisation-fr-l9)):
  - `internal/learning/optimise.go`: the GEPA-style loop. Pareto parent pick, reflection on failed (or slowest) tasks, guards (protected lines, size, task values, front-matter), dedupe, train and held-out gates.
  - `internal/learning/tuned.go`: tuned built-ins under `<data>/tuned/<name>` with write-once versions, `TUNED.json`, a changelog, the stale check and an overlay over the embedded skills.
  - `internal/app/optimise.go` and `noskill skills optimise <name>|adopt|revert|show`. `skills list` shows `builtin (tuned N)`.
  - Eval runs record each built-in's tuned version; `eval gate --rollback` puts a tuned built-in back when a task naming it falls.
- **Live check, run 1** (`skills optimise files --dry-run --budget 2`, qwen3:4b writing and running): the current instructions passed 9 of 28 training tasks in 63 steps. The 10 agent tasks did well (7 passed); the workload tasks mostly failed (2 of 18 passed). Both rewrites were dropped unscored because they named `notes`, a folder from a failed request. So a dropped rewrite now gets one more try with the reason, and task values match whole words only.
- **Live check, run 2:** the retry worked (the first rewrite named `draft.txt` and was asked again), but every reply then came back as broken JSON. Asked for the whole instructions as one JSON string, qwen3:4b repeats a rule until its token limit in about half the replies (checked by hand: 2 of 4 at 2,000 tokens). Without a schema it reasons aloud instead. So the reflection now asks for at most 3 line changes, and Go applies them (ADR-0014). Replies end in about 200 tokens.
- **Live check, run 3** (same command; I stopped it after about 4¼ hours, in the middle of scoring rewrite 2, without your yes; it was a dry run, so nothing it had would have been saved): the line changes worked end to end. The current instructions passed 12 of 28 training tasks (64 steps) and the first rewrite passed 16 of 28 (83 steps), so the loop made a rewrite that passed the guards and scored better. Run 1's baseline for the same instructions was 9 of 28, so a gap of 4 tasks is no bigger than the 3-task gap between two scorings of the same text. That's why the accept rule needs at least `max(2, 10%)` more passes **and** held-out tasks that don't get worse. Whether either rewrite was accepted isn't known yet.
- **Live check, run 4** (`skills optimise files --model llama3.2:latest --tasks 8 --budget 2 --adopt`, a real run on a scratch home, 5 minutes, 28 task runs, `llama3.2` wrote the rewrites too): the whole path worked. The original passed 0 of 8 training tasks, rewrite 1 passed 2 of 8 and rewrite 2 passed 1 of 8; held-out stayed 0 of 2 (9 steps, was 3). The accept rule kept rewrite 1 (at least 2 more passes, no fewer held-out passes), saved it as tuned version 1, put it in use (`--adopt`) and wrote the report file. **The rewrite itself is weak:** it added a dangling line ("Use path tokens: ~, $DOWNLOADS, $VIDEOS, volume:<label>, and {dir}:") and a near-duplicate "never overwrite" rule. 2 more passes of 8 is within the noise seen between two scorings of the same text, and the held-out set is 2 tasks, so this shows the pipeline works, not that the rewrite helps. A real judgement needs the full task list (about 20 minutes on this model), which hasn't been run. Nothing was changed in the real home.
- **Live check, run 5** (the same on the full task list: `--model llama3.2:latest --budget 2 --adopt`, 84 task runs in about 12 minutes): the original passed 3 of 28 training tasks (61 steps), rewrite 1 passed 3 of 28 and rewrite 2 passed 4 of 28 (69 steps). Neither cleared the bar (3 more passes, or as many in 15% fewer steps), so the current instructions were kept and nothing was saved. **So run 4's accepted rewrite was noise**: 2 of 8 against 0 of 8 on a partial list. `--adopt` is no longer allowed with `--tasks`. The loop works end to end and does reject weak rewrites, but no rewrite has improved `files` on any model yet. `llama3.2` only passes 3 of 28 of these tasks, so this model is too weak here to show a gain.
- **Why it's slow:** `qwen3:4b` can't turn its reasoning off. Each model call generated a median of 1,070 tokens (up to 3,563) at about 21 tokens per second, against about 14 for `llama3.2:3b` on a tool-call step, so a task takes about 3 minutes and a full `--budget 2` run is about 111 task runs (more than 5 hours). Found afterwards: `--model` already chose the model the tasks run on (so `--model llama3.2:3b` was possible), and a `--tasks N` limit was added (`--tasks 8 --budget 2`); each task line now shows its time.
- **Not built:** running it on its own at idle; more checked tasks for skills other than `files` (they can't be optimised until they have 5).
