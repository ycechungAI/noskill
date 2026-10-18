# ADR-0006: `tev1:0.8b` with one question as the 16 GB decision model, with a cache ceiling

Status: **accepted** · Date: 2026-10-15 · Evidence: [S3 results](../spikes/S3-results.md)

## Context
The plan made `tev1` (4B) the resident decision model on 16 GB, answering five router questions in one ~100 ms call ([ORCHESTRATOR.md](../ORCHESTRATOR.md)). Phase 0 spike S3 measured this on a 16 GB Mac mini M4:
- `tev1` was the most accurate (92.5% task type) but took ~3 s per call idle and 5.3 s while a 4B chat model generated.
- Ollama's MLX runner keeps a prompt cache of up to ~8.5 GB per model. `tev1` grew to 12.9 GB, leaving 14% of memory free next to a 4B chat model.
- Each question is read with its own copy of the request, so latency grows with the number of questions. An identical repeated call takes 6–75 ms: reading the prompt is the cost.
- `tev1:0.8b` asking only `task_type` (with option descriptions) matched its own five-question accuracy (81.5% vs 82%) at a quarter of the time (235 ms idle, 536 ms under load). The cache still grows ~30 MB per question per call.
- Emulated decisions on `qwen3.5:4b` were as accurate (82.5%) but took ~2.2 s and occupy the chat model.
- `laya` (421M ModernBERT) was 2.5× faster (97 ms) and kept no prompt cache (≤ 1.4 GB), but it was less accurate (76% with short option descriptions, 68% under load), less well calibrated (ECE 0.14 vs 0.05) and limited to 176 tokens of options.

## Decision
- **16 GB and below:** `tev1:0.8b` is the recommended resident decision model. The router asks only `task_type` (with descriptions). Other questions are asked only when a decision depends on them.
- **Above 16 GB:** `tev1` when `noskill models bench` shows it within the latency budget on that machine. Otherwise `tev1:0.8b`.
- **Cache ceiling:** when `/api/ps` reports the decision model more than `decision_cache_gb` (default 1 GB) above its weights, noskill unloads it after the current reply and lets the next call reload it (~1 s, about every 30 calls). noskill never changes Ollama's server settings. `doctor` recommends a server-side cache cap where Ollama supports one.
- **Latency budget** for a routing decision on 16 GB: ≤ 300 ms median idle, ≤ 600 ms while generating. This replaces the 150 ms target.
- **`noul` thresholds** are calibrated per decision and model by `noskill models bench`. Only `tev1`'s `noul` behaves as P(yes).
- Emulation by the chat model stays the fallback when no decision model is installed.

## Consequences
- Rules and kNN (router tiers 1–2) matter more: they avoid the decision call for most requests.
- The router loses `difficulty` and `needs_*` from the single call. Model choice falls back to the skill's `min_model` and the deterministic tool triggers ([ADR-0005](0005-native-tool-calling-default.md)) unless a second question is asked.
- The cache ceiling adds a reload path to the backend (FR-B6) and a `decision_cache_gb` setting.
- These results come from one M4 machine and Ollama 0.40.3. S4 re-measures memory after 200 varied requests with a 7–8B primary, and the cache behaviour may change between Ollama versions.
