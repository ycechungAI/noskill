# S1 results: Tool calling on small models
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1 · Ollama 0.40.3 · `llama3.2:3b` (Q4_K_M, a80c4f17acd5), `qwen3.5:4b` (nvfp4, 7f3251fa6878, thinking off) · harness [spikes/s1](../../spikes/s1/), dataset `tools-50` (20 single-call, 20 two-step, 10 no-tool tasks)

**Deviation from the plan:** 2 models instead of 4. `gemma3:4b`, `qwen3:4b` and `qwen3:8b` weren't pulled (only the decision models were approved for download). `qwen3.5:4b` stands in for the Qwen models. The decision below is made for "small models in general" from two very different models; the per-model probe at `init` (see Decision) covers models we didn't test.

## Numbers
900 runs: 2 models × 3 strategies × 3 seeds × 50 tasks, temperature 0.2, `num_ctx` 8192, max 8 steps.

| Model | Strategy | Valid calls | Right tool | Right args | **Success** | Single | Two-step | No-tool | False calls | Median ms/run | Median ms/step | Success by seed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| llama3.2:3b | native | 100% | 86% | 86% | **64%** | 83% | 27% | 100% | 63% | 2264 | 1202 | 66 / 64 / 62 |
| llama3.2:3b | constrained | 100% | 88% | 88% | **48%** | 60% | 30% | 60% | 90% | 2657 | 877 | 48 / 46 / 50 |
| llama3.2:3b | prompted | 69% | 77% | 77% | **43%** | 68% | 17% | 43% | 90% | 3106 | 856 | 38 / 46 / 44 |
| qwen3.5:4b | native | 100% | 100% | 100% | **89%** | 95% | 83% | 90% | 0% | 5248 | 2167 | 90 / 90 / 88 |
| qwen3.5:4b | constrained | 100% | 100% | 98% | **95%** | 95% | 93% | 100% | 0% | 3992 | 1656 | 96 / 94 / 96 |
| qwen3.5:4b | prompted | 98% | 97% | 94% | **89%** | 95% | 82% | 90% | 0% | 3466 | 1492 | 90 / 88 / 88 |

- "False calls" = share of the 10 no-tool tasks where the model called a tool anyway. "No-tool" success can still be high, because a needless tool call doesn't make the answer wrong.
- Two llama3.2:3b runs (native seed 3 t07, prompted seed 2 t43) generated text until the 300 s request timeout, on every retry. They count as failures.
- t37 was rerun (18 runs) after a mock-web fix (see Surprises); the table includes the rerun.

Why runs failed:

| Model · strategy | Failed runs | Used all 8 steps | Wrong answer or end state | Other |
|---|---|---|---|---|
| llama3.2:3b · native | 54 | 0 | 50 | 3 answered without tools, 1 timed out |
| llama3.2:3b · constrained | 78 | 46 | 32 | |
| llama3.2:3b · prompted | 86 | 52 | 33 | 1 timed out |
| qwen3.5:4b · native | 16 | 7 | 9 | |
| qwen3.5:4b · constrained | 7 | 1 | 6 | |
| qwen3.5:4b · prompted | 17 | 1 | 15 | 1 invalid call |

## Pass/fail
S1 has decision rules rather than pass/fail criteria.

| Rule | llama3.2:3b | qwen3.5:4b |
|---|---|---|
| Constrained ≥ native + 10 pts → constrained default for < 7B | No: −16 pts | No: +6 pts |
| Within ±10 pts → native default, constrained as fallback | – | **Yes** |
| Native ≥ constrained + 10 pts → native everywhere | **Yes** | – |
| Any strategy with false-call rate > 10% → add a "tool needed?" decision | **Yes**: 63–90% | No: 0% |
| Constrained latency overhead ≤ 15% | Fail per run (+17%, because of more looping steps); pass per step (−27%) | Pass (−24% per run, −24% per step) |

## Decision
Recorded in [ADR-0005](../adr/0005-native-tool-calling-default.md), [MODEL_ADAPTERS.md](../MODEL_ADAPTERS.md#tool-calling-strategies) and FR-A1/FR-B3:
1. **Native tool calling is the default** for every model whose template supports tools and passes the `init` probe, including models under 7B. This replaces the plan's "constrained by default for < 7B".
2. **Constrained is the fallback**: after a malformed or invalid native call (retry that step constrained), and for models without native tools. Prompted is the last resort.
3. **The `init` probe picks the strategy per model**: 10 `tools-50`-style tasks, native vs constrained. Constrained is used only if it wins by ≥ 10 points. This covers models we didn't test.
4. **Gate tools with the router's `needs_tools`**: when the decision says no tools are needed (below a threshold that S3 will calibrate), the step is sent without `tools`. This targets llama3.2:3b's 63% false-call rate. *Revised after S3: no safe threshold was found, so gating is off by default and optional per model ([S3](S3-results.md#tool-gate-needs_tools), [ADR-0005](../adr/0005-native-tool-calling-default.md)).*
5. **Loop guards in the agent loop**: a per-step output cap (`num_predict`, default 1024), a wall-clock budget per step, and repeat detection (the same call with the same arguments twice in a row → stop and answer from what you have).

## Surprises
- **Constrained made llama3.2:3b worse, not better.** With every turn forced into the action schema, it kept calling tools until the 8-step limit (46 of its 78 failures) instead of answering. Native lets it stop.
- **Constrained is faster per step for both models.** Native tool templates add a lot of prompt tokens. For qwen3.5:4b, constrained is also the most accurate (95%). The probe in Decision point 3 can still pick constrained for models like this; the rule only says native is the safe default.
- **llama3.2:3b calls tools for nothing:** `search` for "capital of Australia", `run_python` for `12 * 12`, and even `run_python` with the sentence it was asked to summarise as its code. qwen3.5:4b never did.
- **Time arithmetic fails everywhere (t04, 0%).** Every run called `get_time` correctly (09:30), then got "hours until midnight" wrong: 6.5, 8.5, 12.5 and 2.5 hours. Exact date and time maths should go to a deterministic tool (a `time` helper or `run_python`), never the model's head.
- **Models don't know where external drives are (t30, 11%).** They tried `~/AnimeDrive/Anime` and `/Users/me/AnimeDrive/Anime` before `/Volumes/...`. The `files` skill's `volumes` tool, plus a one-line list of mounted drives in context, is needed for storage skills like anime-tracker.
- **Runaway generation:** llama3.2:3b sometimes generates until the request times out (2 runs, repeatable with the same seed). Hence the per-step output cap.
- **Two harness bugs found and fixed:**
  - t48 ("summarise in five words or fewer") required the word "Thursday". It now accepts any fair summary of ≤ 5 words, and runs were rescored from their saved answers. qwen3.5:4b native/prompted still fail, honestly: "Meeting rescheduled due to broken projector." is six words.
  - t37: the mock web only served the exact repo URL, so fetching `…/blob/main/LICENSE` returned 404 and models looped. Sub-pages and raw/API GitHub URLs now resolve to the repo page. After the rerun (done after S3, so S3's timings weren't disturbed), qwen3.5:4b native and constrained and llama3.2:3b constrained solve it 3 of 3; llama3.2:3b native searches once and then guesses the licence ("MIT"); prompted mode fails on both models (llama3.2:3b guesses "MIT", qwen3.5:4b "GPL-3.0") without fetching the page.
- **Seeds barely matter** at temperature 0.2 (± 2–4 points), so 3 seeds is enough for future tool-calling evals.
