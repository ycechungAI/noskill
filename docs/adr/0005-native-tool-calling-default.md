# ADR-0005: Native tool calling by default, constrained as fallback

Status: **accepted** · Date: 2026-10-15 · Evidence: [S1 results](../spikes/S1-results.md)

## Context
The plan made grammar-constrained tool calling the default for models under 7B, even ones with native tool support, on the assumption that forced JSON removes most malformed calls ([MODEL_ADAPTERS.md](../MODEL_ADAPTERS.md#tool-calling-strategies)). Phase 0 spike S1 tested this on `llama3.2:3b` and `qwen3.5:4b` (900 runs):
- Malformed calls weren't the problem: native calls were 100% valid on both models.
- Constrained cut llama3.2:3b's success from 64% to 48%. It kept calling tools until the step limit instead of answering.
- On qwen3.5:4b constrained was slightly better (95% vs 89%) and faster per step.
- llama3.2:3b called tools on 63–90% of tasks that needed none.

## Decision
- **Native** is the default whenever a model's template supports tools and it passes the `init` probe, at every size.
- **Constrained** is used (a) to retry a step after a malformed or invalid native call, (b) for models without native tools, and (c) when the per-model `init` probe (10 tasks, native vs constrained) shows constrained ≥ 10 points better.
- **Prompted** is the last resort, when there's no constrained decoding.
- **Tool gating** (revised after [S3](../spikes/S3-results.md#tool-gate-needs_tools)): tools are offered by default. S3 found no `needs_tools` threshold that keeps every tool-needing request while removing tools from a useful share of the rest, so the router's decision doesn't remove tools on its own. A request always gets tools when a deterministic check fires: a URL, "run"/"execute" with code, a file path or drive, or date/time words. A model whose `init` probe shows a high false-call rate can set `gate_tools` with a threshold calibrated by `noskill models bench` on the installed decision model. False calls are otherwise contained by the loop guards below.
- **Loop guards** in the agent loop: per-step output cap (`num_predict` 1024 by default), per-step wall-clock budget, and repeat detection (same call + same arguments twice → stop calling tools and answer).

## Consequences
- Simpler default path, and it matches what chat apps and Ollama already do.
- Strategy choice becomes empirical per model (the probe result is stored in the model profile, FR-B2) instead of a size rule.
- G4 ("works on a 3B–4B model without native tools") still relies on constrained mode. Built-in skill evals keep running in both modes.
- Only two models were tested. If a later model family shows the opposite pattern, the probe handles it without a code change.
