# S3 results: JEV decision models on 16 GB
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1 · Ollama 0.40.3 (MLX runner for all models below) · `tev1` (4.2B, mxfp8, 4d24c6f6d61a, 4.4 GB on disk) and `tev1:0.8b` (752M, BF16, b661d5650816) through `/v1/systemone`; emulated decisions on `qwen3.5:4b` (thinking off) and `llama3.2:3b` · harness [spikes/s3](../../spikes/s3/), datasets `noul-40` and `router-200`

**Deviations from the plan:**
- `nimble` (9B) wasn't tested: it wasn't pulled, and it wouldn't fit next to a chat model on 16 GB anyway.
- Emulated decisions and the background chat load used `qwen3.5:4b` and `llama3.2:3b` (already installed) instead of `qwen3:4b` and `qwen3:8b`.
- **Labels.** Gold labels follow the [assisted labelling protocol](../PHASE0_TESTS.md#assisted-labelling):
  - Labeller a was an AI draft and b was the user.
  - Where b disagreed with a blind second AI pass (c), the user settled it: 39 requests, 26 taking the AI's label and 13 keeping their own.
  - The other 161 requests are where a and b agreed.
- **Method flaw found afterwards:**
  - The `noul` part didn't unload `tev1`, so the "idle" routing run of `tev1:0.8b` had `tev1` loaded next to it (22% memory free).
  - More importantly, Ollama's prompt cache grew during every run (see Memory), so later requests in each run were slower than earlier ones.
  - Two follow-ups measured these effects: [latency_probe.py](../../spikes/s3/latency_probe.py) and [lean.py](../../spikes/s3/lean.py).

## Numbers

### noul-40 (is `noul` P(yes)?)
| Model | Mean on yes items | Mean on no items | Lowest yes | Highest no | Right at 0.5 | Median ms |
|---|---|---|---|---|---|---|
| tev1:0.8b | 0.80 | 0.22 | 0.12 | 0.99 | 85% | 72 |
| tev1 | **0.92** | **0.06** | 0.41 | 0.32 | 98% | 400 |

### router-200: one call per request
Each call asks `task_type` (choice of 11), `difficulty` (score 1–5) and `needs_web` / `needs_vision` / `needs_tools` (noul). Emulated = the same questions answered by a chat model as constrained JSON, with first-token probabilities.

| Model | Mode | Task type right | Difficulty MAE | F1 web | F1 image | F1 tools | ECE | Median ms | p95 ms |
|---|---|---|---|---|---|---|---|---|---|
| tev1:0.8b | decision | 82.5% | 1.10 | 0.54 | 0.91 | 0.14 | 0.040 | 1445 | 2701 |
| tev1 | decision | **92.5%** | 0.64 | 0.61 | 1.00 | 0.42 | 0.045 | 3055 | 3456 |
| qwen3.5:4b | emulated | 82.5% | 0.60 | 0.81 | 1.00 | 0.60 | 0.059 | 2174 | 2647 |
| llama3.2:3b | emulated | 81.0% | 0.68 | 0.75 | 0.92 | 0.00 | 0.063 | 1304 | 1489 |
| *while `qwen3.5:4b` generates:* | | | | | | | | | |
| tev1:0.8b | decision | 82.0% | 1.10 | 0.55 | 0.91 | 0.15 | 0.040 | 1423 | 2798 |
| tev1 | decision | **93.5%** | 0.64 | 0.61 | 1.00 | 0.42 | 0.042 | 5345 | 10518 |

- F1 for the noul questions uses a 0.5 cut-off. ECE is for the task-type probabilities; lower is better calibrated.
- Common mix-ups:
  - tev1: reasoning → chat (4).
  - qwen3.5:4b: research → reasoning (7) and math → reasoning (5).
  - llama3.2:3b: math → reasoning (7) and chat → creative (6).

### Where the time goes
[latency_probe.py](../../spikes/s3/latency_probe.py), 15 requests, idle, model alone:

| Question set | Prompt tokens read | tev1:0.8b | tev1 | Same call repeated (cached) |
|---|---|---|---|---|
| All five, as in S3 | 995 | 433 ms | 3413 ms | 23 / 75 ms |
| Task type only, names without descriptions | 287 | 147 ms | 1074 ms | 11 / 22 ms |

- **Each question is read as its own "request + question" prompt.** Five questions mean the request is read five times, about 1,000 tokens per call.
- **The decision itself is nearly free.** An identical repeated call takes 6–75 ms. Reading the prompt is the cost: about 2,300 tokens/s for `tev1:0.8b` and about 290 tokens/s for `tev1` on the M4.
- **Calls slow down as the cache fills:**
  - `tev1:0.8b`'s median went from 513 ms (first 20 requests) to 1,583 ms (last 20) in the routing run.
  - Under load, `tev1` went from 4.4 s to 6.5 s.

### Memory
| Moment | Free memory | What Ollama reports |
|---|---|---|
| `tev1:0.8b` just loaded, alone | 67% | 1.68 GB |
| `tev1:0.8b` after ~70 routing calls | 28% | **10.1 GB**, then flat |
| `tev1` just loaded, `qwen3.5:4b` generating | 29% | 4.8 GB + 3.95 GB |
| `tev1` near the end of that run | 14% | **12.9 GB** + 4.7 GB |

Memory grows to about "weights + 8.5 GB" and then stops. Reports describe Ollama's MLX runner keeping a prefix cache with a fixed budget of about 8 GiB ([ollama#18131](https://github.com/ollama/ollama/issues/18131); [PR #18556](https://github.com/ollama/ollama/pull/18556) proposed making it configurable). We didn't confirm the setting on 0.40.3. The memory is held by the decision model's runner, separately from the chat model's.

### Fewer questions ([lean.py](../../spikes/s3/lean.py), `tev1:0.8b`)
Idle runs are on all 200 requests, model alone and unloaded between sets. Under-load runs are on the first 50 requests while `qwen3.5:4b` generates.

| Question set | Prompt tokens | Type right (idle) | Median / p95 ms (idle) | First 20 / last 20 median | Type right / median (under load) |
|---|---|---|---|---|---|
| All five, as in S3 | 1038 | 82.0% | 1003 / 2063 | 452 / 1595 | 80% / 1010 ms |
| Task type, names only | 296 | 72.5% | 146 / 197 | 142 / 144 | 74% / 309 ms |
| Task type names + `needs_tools` | 444 | 71.5% | 232 / 408 | 223 / 342 | 70% / 458 ms |
| **Task type with descriptions** | 430 | **81.5%** | **235 / 346** | 213 / 305 | 80% / 536 ms |

- **Asking only `task_type` (with descriptions) keeps the accuracy of the full call at about a quarter of the time.** Removing the descriptions costs about 10 points.
- **The prompt cache grows by about 30 MB for each question in each call**, whatever the prompt length, until it reaches about 8.5 GB:
  - 1 question: ~30 MB per call (6.4 GB after ~150 calls).
  - 2 questions: ~63 MB per call (10.1 GB after ~135 calls).
  - 5 questions: 10.1 GB after ~70 calls.
- Latency stays flat until the cache nears its budget, then rises. For the five-question set, the median per 25 calls went 452 → 540 → 1289 → … → 1537 ms.
- On the same 50 requests, the descriptions-only set and the full five-question call are equally accurate (80% each) both idle and under load.

### Another decision model: `laya`
`laya` (421M, Convai Innovations, ModernBERT-large with a decision head, no text decoder, 846 MB, `aaba5574a1e1`) was tested after the run above, with the same harness. It allows at most 176 tokens of options per question, so our task-type descriptions were shortened (in [lean.py](../../spikes/s3/lean.py)). `tev1:0.8b` was rerun with the same short version for a fair comparison.

| Model · question set | Type right (idle, 200) | Median / p95 ms (idle) | ECE | Type right / median (under load, 50) | Most memory seen |
|---|---|---|---|---|---|
| `laya` · names only | 60.5% | 67 / 107 | 0.32 | 54% / 152 ms | 1.4 GB |
| `laya` · short descriptions | 76.0% | **97 / 145** | 0.14 | 68% / 215 ms | 1.4 GB |
| `laya` · all five, short descriptions | 76.0% | 366 / 498 | 0.14 | 68% / 772 ms | 1.4 GB |
| `tev1:0.8b` · short descriptions | 77.5% | 245 / 511 | 0.05 | 82% / 398 ms | 10.1 GB |
| `tev1:0.8b` · full descriptions (above) | **81.5%** | 235 / 346 | 0.05 | 80% / 536 ms | 6.6 GB after 200 calls |

- **noul-40:** `laya` was 77% right at 0.5 (yes items 0.61 on average, no items 0.18, 40 ms). Five yes items scored ≤ 0.17, which fits the model card's warning that `noul` can stick to "no".
- **`laya`'s memory doesn't grow.** It has no prompt cache: repeated calls take the same time, and Ollama reported 0.8–1.4 GB throughout.
- `laya`'s commonest mistake is chat → creative (6 of 200).
- Answers are identical idle and under load; the under-load rows differ only because the first 50 requests are harder for `laya` (mostly chat).
- Shorter descriptions cost `tev1:0.8b` 4 points (81.5% → 77.5%) and saved nothing, since its time is set by the prompt read, not the options.
- During `tev1:0.8b`'s idle "all five, short descriptions" run, `llama3.2:3b` was loaded by another app, so that run's latency (1566 ms median) isn't comparable.

### Parallel calls ([parallel.py](../../spikes/s3/parallel.py), `tev1:0.8b`, all five questions, idle, 24 requests per way)
| Way | Median ms per routing decision | Decisions per second |
|---|---|---|
| One call (as in S3) | 580 | 1.7 |
| The five questions as five calls at once | 518 | 1.9 |
| Four different requests at once | 1784 each | 2.2 |

Ollama's parallel slots share one GPU, and reading the prompt already keeps the GPU busy. Splitting a decision saves about 10%, and running requests side by side adds about 30% throughput. Asking fewer questions saves far more.

### Reloading to empty the cache ([reload.py](../../spikes/s3/reload.py), `tev1:0.8b`, task type only, 8 reloads)
| Step | Median ms | Max ms |
|---|---|---|
| Unload request | 7 | 68 |
| First decision after unloading (load + read prompt) | 978 | 2310 |
| Next decision | 231 | 268 |

At ~30 MB per call, a 1 GB cache ceiling means one reload about every 30 calls. Done after a reply instead of before the next one, the reload stays off the user's wait.

### Tool gate (`needs_tools`)
Can the router safely send a step without tools? 34 of the 200 requests need a tool (web, files, clock or running code); 166 don't. "Kept" = tool-needing requests that still get tools; "gated" = no-tool requests that would be sent without tools.

| Gate | tev1 | tev1:0.8b |
|---|---|---|
| tools or web noul ≥ 0.1 | kept 34/34, gated 12/166 | kept 34/34, gated 1/166 |
| tools or web noul ≥ 0.2 | kept 33/34, gated 45/166 | kept 34/34, gated 7/166 |
| tools or web noul ≥ 0.3 | kept 30/34, gated 100/166 | kept 32/34, gated 40/166 |
| type is research/math/plan, or noul ≥ 0.4 | kept 30/34, gated 112/166 | kept 32/34, gated 73/166 |

- No setting keeps every tool-needing request while gating a useful share.
- Two of tev1's misses are unambiguous: "summarise https://…" (web 0.12) and "run this and tell me the output: print(…)" (tools 0.27).
- Several others are borderline labels, for example a sci-fi book recommendation or a meal plan labelled as needing the web.

## Pass/fail
| Rule | Result |
|---|---|
| `noul` mean ≥ 0.9 on yes, ≤ 0.1 on no → confirm "noul = P(yes)" | **Pass for `tev1`** (0.92 / 0.06). `tev1:0.8b` fails (0.80 / 0.22): it's a usable score, not a probability |
| `tev1` task type ≥ 85% and median ≤ 150 ms under load → `tev1` default on 16 GB | **Fail.** 93.5% right, but a 5.3 s median; and memory grows to ~13 GB |
| `tev1` < 85% but `nimble` ≥ 85% and fits | Not tested |
| Emulated `qwen3:4b` within 5 pts of `tev1` → decision model optional | Emulated `qwen3.5:4b` is 10 pts behind `tev1`, but level with `tev1:0.8b` (82.5% each) |
| Memory pressure goes red with `tev1` + chat model → `tev1:0.8b` on 16 GB | **Yes:** 14% free with `tev1` + `qwen3.5:4b` (4B, not 8B) |

The 150 ms target was set from the ~91 ms quoted for `nimble` on an M5 Max. On the M4 the prompt read sets the time, so the target is replaced with the measured budget below.

## Surprises
- **Every question re-reads the request.** Five questions cost five prompt reads, so the number of questions matters more than the model.
- **Ollama's MLX prompt cache isn't capped by the model's size.** It adds ~30 MB for every question in every call, up to ~8.5 GB, and calls slow down as it fills. A resident decision model on 16 GB has to be reloaded now and then (or capped on the server, if Ollama lets you).
- **Option descriptions are worth ~10 points** on `tev1:0.8b` (72.5% → 81.5%), at ~90 ms.
- **`tev1:0.8b` with one question matches emulated `qwen3.5:4b`** (81.5% vs 82.5%) at a tenth of the time, without occupying the chat model.
- **Parallel calls don't help.** The GPU is already busy reading the prompt.
- **`laya` is 2.5× faster and its memory doesn't grow**, but it's 5.5 points less accurate than `tev1:0.8b` at its best, worse calibrated, and limited to short option lists.

## Decisions
1. **16 GB default: `tev1:0.8b`**, resident, asking **only `task_type` with descriptions** for routing (81.5% idle, 235 ms median; 80%, 536 ms while a 4B model generates).
   - `difficulty` and the `needs_*` flags are asked only when a decision depends on them. Each extra question adds ~80–100 ms and ~30 MB of cache.
   - Tools stay on by default with deterministic triggers ([ADR-0005](../adr/0005-native-tool-calling-default.md)).
2. **`tev1` (4B) only on machines with more than 16 GB.** It's the most accurate (92.5%) and its `noul` is a probability, but it takes ~3 s per five-question call and grows to ~13 GB. `noskill models bench` decides whether it's fast enough on a given machine.
3. **Cache ceiling.** noskill reloads the decision model when Ollama reports it more than `decision_cache_gb` (default 1 GB) above its weights. The reload happens after a reply, not before the next request. It costs ~1 s and is needed about every 30 calls. noskill never changes Ollama's settings. `doctor` suggests a server-side cache cap where Ollama supports one.
4. **Latency budget** for a routing decision on 16 GB is ≤ 300 ms median idle and ≤ 600 ms while generating, replacing 150 ms. Rules and kNN (tiers 1–2) still come first, so most requests never reach the decision model.
5. **`noul` meaning.** For `tev1` it's P(yes). For `tev1:0.8b` it's a score, so each decision gets its own threshold from `noskill models bench`, not a fixed 0.5.
6. **One call per decision, no splitting**, and no parallel calls for speed.
7. **No decision model** remains supported: decisions are emulated by the chat model. It's equally accurate but takes ~2 s and occupies the chat model, so `doctor` recommends `tev1:0.8b`.
8. **`laya` isn't a default.** It's a candidate where memory matters most (8 GB machines, no cache to manage) and for short decisions with few options where ~50 ms matters. `noskill models bench` can pick it per decision once 8 GB is tested.

See [ADR-0006](../adr/0006-decision-model-on-16gb.md).
