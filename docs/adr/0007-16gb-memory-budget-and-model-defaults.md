# ADR-0007: 8 GB model budget and a 4B default primary on 16 GB

Status: **superseded in part** by [ADR-0012](0012-shared-machine-6gb-budget-with-burst.md) (budget is now 6 GB plus burst; the measurements below still stand). Was: accepted · Date: 2026-10-15 · Evidence: [S4 results](../spikes/S4-results.md)

## Context
The plan gave models ~10.5 GB on a 16 GB Mac (Metal's ⅔-of-RAM working-set limit) and put `qwen3:8b` as the default primary next to a resident decision model, with swaps costing ~3 s ([ORCHESTRATOR.md](../ORCHESTRATOR.md)). Phase 0 spike S4 measured this on a 16 GB Mac mini M4 with normal desktop apps running (3 GB of swap already in use):
- `tev1:0.8b` (with the ADR-0006 ceiling, ≤ 2.7 GB) next to `qwen3:8b` (6.6 GB at 8k) put macOS at its memory-pressure warning in 12 of 21 samples over 200 requests, down to 14% free, and grew swap by 3 GB. Next to `qwen2.5vl:7b` (7.3 GB): 8 of 21 samples, 10% free, swap +1.8 GB. Next to `qwen3:4b` (3.9 GB) or `qwen2.5-coder:7b` (5.1 GB): 3 and 2 of 21 samples, 23–24% free at the lowest.
- Swapping to a model that wasn't used recently took 2.8 s (4B), 4.3 s (7B coder), 6.3 s (7B vision) and 7.2 s (8B), because the file cache has been used by other models. Warm reloads were 1.1–2.6 s.
- Changing `num_ctx` reloads the model (1.1–5.5 s).
- GGUF models keep a constant size over 200 requests; only MLX models grow.
- `q8_0` KV cache saved 0.2–0.6 GB at 8k and cost 2.7 points of S1 success (noise-level, over the 2-point limit).

## Decision
- **Memory budget on 16 GB Apple Silicon: 8 GB** (resident + primary), not 10.5 GB. Other Apple Silicon keeps ⅔ of RAM until measured; discrete GPUs keep VRAM − 0.5 GB.
- **Default primary on 16 GB: `qwen3:4b`**; `qwen2.5-coder:7b` for code (it fits). `qwen3:8b` and `qwen2.5vl:7b` don't fit next to the resident model in 8 GB. They run when the user raises `memory_budget_gb`, or (vision, a hard need) after noskill unloads the resident decision model and emulates decisions on the loaded model.
- **Swap cost is measured** per model at `init`/`models bench`; defaults 3 s (4B), 5 s (7B), 7 s (8B). `min_swap_gain` 0.10 → 0.15 and `hold_seconds` 60 → 120.
- **One `num_ctx` per model**, as planned.
- **`q8_0` KV cache stays optional.** `doctor` may offer it when memory is tight, after the `init` probe shows no quality loss for that model.
- **The prompt-cache ceiling (ADR-0006) applies to MLX models only.**

## Consequences
- The default chat quality on 16 GB drops from an 8B to a 4B model. The router and skills must work at that level (AGENTS.md already says design for small models first).
- Vision needs a ~6 s swap and a temporary loss of the resident decision model on 16 GB, unless a smaller vision model is installed (not tested).
- Learned routing (bandit) rarely has an 8B to choose on 16 GB, so most of its gain comes from choosing between the 4B, the coder and the vision model.
- Results are from one machine with one app mix. S4 should be repeated on another 16 GB Mac and on a 32 GB Mac before the other-sizes rule (⅔ of RAM) is trusted.
