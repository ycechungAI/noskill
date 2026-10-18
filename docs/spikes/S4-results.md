# S4 results: Memory slots and swap cost
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1 · Ollama 0.40.3 · harness [spikes/s4](../../spikes/s4/) (`report.py` prints every table below) · models `qwen3:8b` (5.2 GB on disk), `qwen2.5-coder:7b` (4.7 GB), `qwen3:4b` (2.5 GB), `qwen2.5vl:7b` (6.0 GB), all GGUF on Ollama's llama.cpp runner, with `tev1:0.8b` (MLX runner) as the resident decision model

**Deviations from the plan:**
- **No cold-after-reboot numbers, and no W1.** A reboot and a Windows machine weren't available. "After the other models" stands in for cold: each model is loaded right after the other three, which pushes its files out of the macOS file cache. It isn't a real cold start from disk.
- **The machine wasn't idle.** The Claude desktop app, a browser pane and Finder were running, and 3 GB of swap was already in use when S4 started. That's how people actually use a 16 GB Mac, so the memory results below include it. With nothing loaded, macOS reported 78% free memory and normal pressure.
- **`q8_0` KV cache was tested on a second Ollama server** (port 11500, `OLLAMA_FLASH_ATTENTION=1`, `OLLAMA_KV_CACHE_TYPE=q8_0`), so your server's settings weren't touched. It also uses larger batches (`-b 1024` instead of 512), which belong to the flash-attention path and add to the difference.
- **S1 subset** for the quality check: `llama3.2:3b`, native tool calling, 3 seeds (150 runs), compared with the same runs in S1.
- **Ollama's MLX cache cap wasn't tested:** Ollama 0.40.3 lists no setting for it in its own server-config log.
- **A planned run was skipped:** `qwen3:8b` + `tev1:0.8b` with no cache ceiling. S3 showed `tev1:0.8b` growing to ~10 GB, which next to a 6.6 GB model is more than this machine has, and swap was already at 6 of 7 GB. It wasn't worth risking your Mac.
- **The memory guard** stops a run below 8% free memory or at critical macOS memory pressure. No run was stopped.

## Numbers

### Load times (5 runs each)
| Model | Warm (unloaded a moment ago) median / max | After the other three models median / max |
|---|---|---|
| `qwen3:4b` | 1.1 s / 1.3 s | 2.6 s / 2.6 s |
| `qwen2.5-coder:7b` | 1.3 s / 2.8 s | 4.1 s / 4.3 s |
| `qwen3:8b` | 1.6 s / 1.6 s | **6.4 s** / 7.1 s |
| `qwen2.5vl:7b` | 2.6 s / 2.6 s | **6.1 s** / 6.4 s |

### Swaps with `tev1:0.8b` resident (3 rounds through all four: unload the current model, load the next)
| To | Median | Max | Lowest free memory | Highest macOS pressure |
|---|---|---|---|---|
| `qwen3:4b` | 2.8 s | 2.8 s | 45% | normal |
| `qwen2.5-coder:7b` | 4.3 s | 6.5 s | 35% | normal |
| `qwen2.5vl:7b` | 6.3 s | 6.3 s | 14% | **warning** |
| `qwen3:8b` | **7.2 s** | 7.2 s | 27% | normal |

A real swap is a "cold" load: the model you switch to hasn't been used for a while, so its files aren't in the file cache. That's why swaps take as long as the "after the others" column, not the warm one.

### Changing `num_ctx` (does it reload?)
Yes. Asking for a different `num_ctx` than the loaded one reloads the model; the same value doesn't.

| Model | First load (4k) | Same 4k again | 4k → 8k | Same 8k again | 8k → 4k |
|---|---|---|---|---|---|
| `qwen3:4b` | 2.6 s | 9 ms | 1.4 s | 8 ms | 1.1 s |
| `qwen2.5-coder:7b` | 4.3 s | 3 ms | 1.4 s | 8 ms | 1.4 s |
| `qwen3:8b` | 5.3 s | 9 ms | 1.9 s | 3 ms | 5.5 s |
| `qwen2.5vl:7b` | 6.1 s | 7 ms | 2.7 s | 7 ms | 2.4 s |

### Memory by context size and KV cache type (Ollama's reported size, GB)
| Model | f16 8k | f16 16k | q8_0 8k | q8_0 16k |
|---|---|---|---|---|
| `qwen3:4b` | 3.86 | 5.11 | 3.31 | 3.97 |
| `qwen2.5-coder:7b` | 5.12 | 5.61 | 4.91 | 5.18 |
| `qwen3:8b` | 6.63 | 7.76 | 6.08 | 6.74 |
| `qwen2.5vl:7b` | 7.30 | 7.79 | 7.09 | 7.36 |

`q8_0` saves 0.2–0.6 GB at 8k and 0.4–1.1 GB at 16k. Doubling the context from 8k to 16k costs 0.5–1.3 GB with f16.

### 200 varied requests, each model alone (8k context)
| Model | Size at start → end | Median ms, first 10 / last 10 requests | Lowest free memory |
|---|---|---|---|
| `qwen3:4b` | 3.86 → 3.86 GB | 406 / 401 | 47% |
| `qwen2.5-coder:7b` | 5.12 → 5.12 GB | 753 / 685 | 31% |
| `qwen3:8b` | 6.63 → 6.63 GB | 709 / 742 | 23% |
| `qwen2.5vl:7b` | 7.16 → 7.16 GB | 678 / 670 | 15% |

**GGUF models don't grow.** Their memory and speed stay flat over 200 varied requests. The prompt-cache growth found in [S3](S3-results.md#memory) is specific to the MLX runner (`tev1`, `tev1:0.8b`, `qwen3.5:4b`).

### `tev1:0.8b` resident + one primary, 200 varied requests
The resident model is reloaded after a reply when it's more than 1 GB over its weights ([ADR-0006](../adr/0006-decision-model-on-16gb.md)). Each request makes one decision call, then a 24-token reply from the primary.

| Primary | Lowest / median free memory | Samples at macOS "warning" (of 21) | Swap in use, start → end | Resident reloads | Decision median | Primary reply median |
|---|---|---|---|---|---|---|
| `qwen3:4b` | 23% / 33% | 3 | 5.4 → 5.3 GB | 7 | 230 ms | 1.0 s |
| `qwen2.5-coder:7b` | 24% / 28% | 2 | 5.7 → 5.4 GB | 7 | 216 ms | 1.8 s |
| `qwen3:8b` | **14%** / 20% | **12** | 3.0 → **6.0 GB** | 7 | 246 ms | 2.0 s |
| `qwen2.5vl:7b` | **10%** / 14% | **8** | 5.3 → **7.1 GB** | 7 | 264 ms | 1.9 s |

- With the ceiling, `tev1:0.8b` stays between 1.6 and 2.7 GB (2.5 GB at the end of each run) and is reloaded **7 times in 200 requests** (about every 28 calls). The decision stays at ~220–260 ms.
- Sizes at the end: primary + `tev1:0.8b` = 6.4 GB (`qwen3:4b`), 7.6 GB (coder), 9.1 GB (`qwen3:8b`), 9.8 GB (`qwen2.5vl:7b`).

### `q8_0` KV cache and tool-calling quality (S1 subset, `llama3.2:3b`, native, 150 runs)
| KV cache | Success | By seed | Single-call | Two-step | No-tool | Median ms/run |
|---|---|---|---|---|---|---|
| f16 (S1) | 64.0% | 33 / 32 / 31 of 50 | 50/60 | 16/60 | 30/30 | 2264 |
| `q8_0` | **61.3%** | 30 / 32 / 30 of 50 | 48/60 | 14/60 | 30/30 | 2418 |

−2.7 points. Of the 150 task-and-seed pairs, 6 came out differently: 5 favour f16, 1 favours `q8_0`. That's within seed noise (S1 seeds ranged 62–66%), but it's above the 2-point limit.

## Findings
1. **A 16 GB Mac can't give models the 10.5 GB the plan assumed.** `tev1:0.8b` + `qwen3:8b` is 9.1 GB on paper and still put macOS at its warning level in 12 of 21 samples, down to 14% free, and grew swap by 3 GB. `qwen3:8b` alone reached the warning level too (23% free). `tev1:0.8b` + `qwen2.5-coder:7b` or a 4B model is comfortable.
2. **Swaps are slow for 7–8B models:** 4–7 s, not the ~3 s assumed in [ORCHESTRATOR.md](../ORCHESTRATOR.md). Only the 4B model swaps in under 4 s.
3. **Changing `num_ctx` reloads the model** (1.1–5.5 s), so one fixed `num_ctx` per model is right.
4. **GGUF models have no cache growth; MLX models do.** The ceiling-and-reload rule from ADR-0006 is only for MLX decision models. Where noskill talks to a GGUF decision model (e.g. `nimble` if it ships as GGUF) it isn't needed.
5. **`q8_0` KV is a small saving** (0.2–0.6 GB at 8k) with a small, probably noise-level quality cost. It isn't worth recommending by default.
6. **Cold-start numbers matter more than warm.** The "warm swap ≤ 4 s" rule is met by warm reloads (1.1–2.6 s), but real swaps are cold.

## Pass/fail
| Rule | Result |
|---|---|
| Resident + any one 7–8B primary at 8k fits the budget with no memory-pressure warning after 200 varied requests | **Fail.** `qwen3:8b` (12 of 21 samples at warning, 14% lowest free) and `qwen2.5vl:7b` (8 of 21, 10%) fail clearly. `qwen2.5-coder:7b` (2 of 21) and `qwen3:4b` (3 of 21) pass in practice but still touched the warning level once or twice, on a machine that already had 3 GB of swap in use |
| Warm swap ≤ 4 s on M1 (otherwise raise `min_swap_gain` and `hold_seconds`) | **Warm reload passes (1.1–2.6 s). Real, cold swaps fail** for the 7–8B models (4.3–7.2 s). So raise `min_swap_gain` and `hold_seconds` |
| `q8_0` KV costs ≤ 2 pts task success on the S1 subset | **Marginal fail:** −2.7 pts (noise-level, but over the limit), so don't recommend it by default |

## Decision
Details in [ADR-0007](../adr/0007-16gb-memory-budget-and-model-defaults.md).
- **Memory budget on 16 GB Apple Silicon: 8 GB** (was ~10.5 GB). Resident (`tev1:0.8b` ≈ 2.5 GB with the ceiling) leaves ~5.5 GB for the primary.
- **Default primary on 16 GB: a 4B-class model** (`qwen3:4b`, 3.9 GB at 8k) or `qwen2.5-coder:7b` for code (5.1 GB). 8B models and vision 7B models need more room than the default budget gives: they run only when the user raises `memory_budget_gb` or closes other apps, or when noskill unloads the resident decision model (emulating decisions on the loaded model).
- **Swap costs are measured per model at `init`/`models bench`**, not assumed. Defaults until measured: ~3 s for a 4B, ~5 s for a 7B, ~7 s for an 8B. `min_swap_gain` 0.10 → **0.15**, `hold_seconds` 60 → **120**.
- **One `num_ctx` per model**, confirmed.
- **`q8_0` KV cache** isn't recommended by default. `doctor` can offer it as an option when memory is tight, and the `init` probe checks tool-calling quality before it's switched on.
- **The prompt-cache ceiling applies to MLX models only.**
- **Not tested here:** cold-from-reboot loads, a Windows/NVIDIA machine (W1), an MLX cache cap, and decision calls made while the primary is generating.
