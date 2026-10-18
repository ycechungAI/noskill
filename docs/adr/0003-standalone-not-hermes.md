# ADR-0003: Build noskill standalone, not on top of Hermes Agent

Status: **accepted** · Date: 2026-10-15 · Decided by: project owner

## Context
Hermes Agent (Nous Research, MIT) already creates, refines and curates skills with Ollama. Options were: build standalone, build a thin layer on Hermes, or contribute upstream ([OPEN_QUESTIONS Q3](../OPEN_QUESTIONS.md#q3)).

## Decision
**Standalone.** noskill owns its agent loop, learning loop, orchestrator, scheduler and packaging.

## Why
- The goals that set noskill apart (one Go binary, < 150 ms start, native daemons on all three OSes, a drop-in Ollama/OpenAI proxy, small-model-first tool calling, JEV decision models in the core loop) touch the agent loop itself. They'd be hard to retrofit as a layer.
- No dependency on a Python runtime or on another project's release cadence.

## Consequences
- More to build: the learning loop (synthesis, verification, curator) is ours. Hermes stays the main reference design ([PRIOR_ART.md](../PRIOR_ART.md)).
- Interop is kept on purpose: same agentskills.io `SKILL.md` format (FR-S1, FR-S8), and noskill skills exposed over MCP (FR-I4) so Hermes users can use them.
- Phase 0 still includes a Hermes teardown, to copy what works and measure where noskill must beat it.

## Evidence (S5, 2026-10-15)
[S5](../spikes/S5-results.md) confirmed the decision. On a 16 GB Mac, Hermes needs a 64k-token context, sends 11–13k tokens of system prompt and tool schemas before the user's first word (a minute of prefill on a 4B model), starts in 1.6 s cold, keeps a 134 MB gateway resident, takes 2 GB of disk, and got 8 of 10 real tasks right with `qwen3.5:4b`. Its learning loop writes skills with no verification (the 4B model's skill referenced a script that doesn't exist). What noskill should copy is listed in the S5 results (file-mutation verifier, read-before-write, curator lifecycle, a stable prompt prefix).

