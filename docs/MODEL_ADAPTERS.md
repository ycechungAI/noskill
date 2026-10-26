# Model adapters

noskill must work with three kinds of models:

1. **Tool-native LLMs**: models with native tool calling in Ollama/llama.cpp (Qwen 3, Llama 3.1+, Mistral, Gemma 3 tool variants, gpt-oss, etc.).
2. **Plain instruct LLMs**: no native tool calling, or unreliable at it.
3. **JEV decision models**: models that don't chat but answer a set of typed questions (choice / yes-no / score) about an input in one fast call, served by Ollama's `/v1/systemone` endpoint (e.g. `nimble`, `tev1`).

## Backends

| Adapter | Covers | Notes |
|---|---|---|
| `ollama` | Ollama | Native `/api/chat` with `tools`, `think`, `format` (JSON schema), `/api/ps` for loaded models, `keep_alive` |
| `openai` | LM Studio, llama.cpp `llama-server`, Jan, LocalAI, KoboldCpp | `/v1/chat/completions`; uses `response_format`/`json_schema` or llama.cpp `grammar` when available |
| `mlx` | `mlx_lm.server` on Apple Silicon | OpenAI-compatible, default `http://127.0.0.1:8080/v1`. No `response_format` or logprobs: a schema goes in the prompt and the reply is read as JSON (a ```json fence is stripped). Lists `/v1/models`; skips embedding and vision models; no embeddings (501); loading is `mlx_lm.server`'s own |
| `vllm` | vLLM on Linux | OpenAI-compatible, default `http://127.0.0.1:8000/v1`. `response_format` (guided decoding) and logprobs; context from `max_model_len`; every served model counts as loaded; version from `/version` |
| `jev` | JEV decision models: Ollama ≥ 0.35 `/v1/systemone`, or any TypeSafe Jev-compatible endpoint | See [below](#jev-decision-models). Serves the `decide` capability only |
| `cloud` (P2) | Anthropic, OpenAI, OpenRouter | Opt-in fallback only |

## Tool calling strategies

Chosen per model from its profile (auto-detected, then confirmed by a probe during `init`). Decided by Phase 0 S1 ([results](spikes/S1-results.md), [ADR-0005](adr/0005-native-tool-calling-default.md)):

| Strategy | When | How |
|---|---|---|
| **native** (default) | Model template supports tools and probe passes, at any size | Pass `tools`; parse `tool_calls` |
| **constrained** (fallback) | No native tools; or retrying a step after a malformed native call; or the probe shows it ≥ 10 pts better for this model. Backend must support JSON schema/grammar | Force output to `{"thought": str, "action": {"tool": enum, "args": {...}}} \| {"final": str}` via schema; the tool enum + args schema are built per step from the shortlisted skills |
| **prompted** | No constrained decoding available | Few-shot ReAct text format + tolerant parser + one repair retry |

~~Constrained by default for models < 7B~~: dropped after S1. Native calls were 100% valid on `llama3.2:3b` and `qwen3.5:4b`, and constrained cut llama3.2:3b's success from 64% to 48% because it kept calling tools instead of answering. The `init` probe runs 10 short tasks in native and constrained mode and stores the winner in the model profile (constrained only if ≥ 10 pts better). Constrained mode is still how models without native tools reach G4.

Other small-model tactics:
- One tool call per step (no parallel calls).
- **Tool gating:** off by default (S3 found no safe `needs_tools` threshold). Deterministic checks (URL, "run this" + code, file paths, date/time words) always keep tools on. Per model, `gate_tools` can be enabled with a benchmarked threshold when the `init` probe shows many false calls (S1: llama3.2:3b called tools on 63% of tasks that needed none).
- **Loop guards:** output cap per step (`num_predict` 1024), wall-clock budget per step, and stop calling tools when the same call with the same arguments repeats.
- **Deterministic helpers for exact work:** date/time differences, arithmetic and hashing go to tools (`time`, `run_python`), never the model's own maths (S1: 0% on "hours until midnight").
- **Mounted drives in context** when the `files` skill is active (one line from `volumes`), because models guess `~/<Drive>` instead of `/Volumes/<Drive>`.
- Tool output summarised to ≤ 1–2k tokens before re-entering context.
- Skill bodies trimmed to the "Steps" section in small-model mode.
- Thinking (`think`) enabled only when the router marks difficulty ≥ 3 (latency).
- **Thinking-only models** (capability `think-always`: the Ollama template always opens `<think>` and never reads `$.Think`, as in Qwen3 "Thinking-2507" builds): every step sends `think=true` and gets twice the step output (2048), because with `think=false` Ollama returns the reasoning as content and the step runs out before the tool call. A step that still runs out while thinking is asked once more to act. Reasoning that leaks into content before a bare `</think>` is moved out of the answer.
- **Graded thinking for a hybrid model whose template mishandles full off** (`ChatRequest.ThinkLevel`, `"low"`/`"medium"`/`"high"`, sent as Ollama's `think` string instead of the bool): checked live on `granite4.2:3b` (2026-10-26) — with `think=false` it skipped tool calls outright (docs-search, tidy/rename apply) and once leaked a bare `</think>` into a plain-chat answer, while `think="low"` called tools correctly and was faster than the `false` run's rambling (2-3 s against 15 s+). `granite4:3b` wasn't affected (scored 84/96 at `think=false`), so this is scoped by model name (`toolThinkLevel`, `internal/app/run.go`) to the 4.2 line rather than applied to the whole `granite` family or every thinking-capable model — raising it generally risks the same latency problem `think=false` exists to avoid (PROXY.md "Thinking fields").

## JEV decision models

Source: Ollama's announcement, [Ollama now supports JEV-style decision models](https://ollama.com/blog/ollama-now-supports-jev-style-decision-models). Details below are from that post; re-check the API before building FR-B6.

### What they are
Decision models implement TypeSafe's Jev API. You send an input (`state`) and a set of named **questions**, and get typed answers to all of them in **one request**. The post reports ~91 ms per decision for `nimble` (9B) on an M5 Max. They don't do free-form chat, and the post doesn't mention tool calling, so they **can't drive the agent loop**. They're a fast, calibrated *judgement* engine that sits beside the chat model.

| Model | Size | From |
|---|---|---|
| `nimble` | 9B | Bespoke Labs |
| `tev1` | 4B | Together AI (experimental) |
| `tev1:0.8b` | 0.8B | Together AI (experimental) |
| `laya` | 421M (ModernBERT, no decoder) | Convai Innovations. Options limited to 176 tokens per question |

### API (as served by Ollama ≥ 0.35)
`POST {ollama}/v1/systemone`

```json
{
  "model": "tev1",
  "state": { "request": "refactor this function to use generics" },
  "questions": {
    "task_type": { "type": "choice", "instructions": "What kind of task is this?",
                   "criteria": { "code": "Write or change code", "research": "Needs web sources", "chat": "Simple conversation" } },
    "needs_web": { "type": "noul", "instructions": "Does answering need current information from the internet?" },
    "difficulty": { "type": "score", "instructions": "How hard is this task?",
                    "criteria": ["Trivial", "Easy", "Moderate", "Hard", "Very hard"] }
  }
}
```

Answers come back keyed by question name:
- `choice` → `choice`, `probabilities`, `confidence`
- `noul` → `noul`: a number. The post doesn't define it, but its example (0.997 for "does the customer ask for a refund?") reads as P(yes). **Treat it as P(yes) until confirmed.**
- `score` → `score`, `legend` (number → label), `probabilities`, `confidence`
- plus `usage.input_tokens` / `usage.output_tokens`

### The `decide` primitive
noskill makes this a first-class capability that the core **and** skills can use:

```go
type Question struct {
    Type         string            // "choice" | "noul" | "score"
    Instructions string
    Criteria     any               // map[string]string for choice, []string for score
}
type Decider interface {
    Decide(ctx context.Context, state map[string]any, qs map[string]Question) (Answers, error)
}
```

Skills get it as the builtin tool `decide`. A skill can also declare named question sets in its front-matter (`noskill.decisions`, see [SKILL_FORMAT.md](SKILL_FORMAT.md)) so they're versioned and testable like any other part of the skill.

### Where noskill uses decisions

| Use | Questions | Replaces |
|---|---|---|
| **Router classification** (FR-O1) | `task_type` (choice, with descriptions); `difficulty` (score) and `needs_vision` / `needs_web` / `needs_tools` (noul) only when the choice depends on them | The tiny-LLM classifier tier. One ~250 ms call (`tev1:0.8b`, M4) instead of a ~2 s generated JSON blob. Each question re-reads the input, so calls ask as few questions as they need |
| **Skill selection rerank** (FR-S3) | `skill` (choice over the embedding shortlist + "none") | Asking the chat model to pick |
| **Outcome judging** (FR-L1) | `answered` (noul), `quality` (score), `followed_skill` (noul) | Slow LLM-as-judge. The probabilities give the outcome confidence directly |
| **Escalation** (FR-O5) | `looks_correct` (noul) on a small model's answer | Self-reported confidence |
| **Injection guard** (SECURITY) | `has_instructions_for_ai` (noul) on fetched web text | Heuristics only |
| **Action safety** (FR-A6) | `destructive` (noul), `matches_user_intent` (noul) before `confirm` tools in headless jobs | Fixed per-tool policy only |
| **Curator** (FR-L5) | `duplicate` (noul) for two skill descriptions | Similarity threshold only |
| **Anime tracker** | `same_series` (noul) for RSS item vs. series; `would_enjoy` (score) vs. taste profile | Fuzzy string match; hand-tuned weights |

Every decision is logged in the trace with its probabilities, so the learning loop can measure calibration and adjust thresholds per decision.

### Calibration
FR-B6c (Phase 4). Two thresholded decisions are stored in `decisions` with their probability, per model:

| Decision | Held to | Right when | Labels come from |
|---|---|---|---|
| `router.task_type` | confidence ≥ threshold (default 0.6) → task-type rules fire | the routed run succeeded (partial is left out) | any outcome with confidence ≥ 0.7 |
| `judge.answered` | P(yes) ≥ threshold (default 0.5) → not a refusal | the run didn't fail | your rating, a check or an implicit signal only (never the judges themselves) |

`noskill calibration` shows, over the last 90 days (up to 500 per decision and model), the reliability table, expected calibration error and Brier score. Tuning picks the threshold with the best separation (Youden's J: share of right decisions at or above it minus share of wrong ones), searched in steps of 0.01 within fixed bounds (router 0.3–0.9, judge 0.2–0.8). It only moves when there are ≥ 40 labelled decisions with ≥ 8 right and ≥ 8 wrong, the best threshold separates by ≥ 0.2 and gains ≥ 0.05 over the current one, and then by at most 0.10 per step.

`orchestrator.auto_tune` is **off by default**: nothing moves until you run `noskill calibration tune … --apply` or turn it on. When on, each (decision, model) is considered at most once a day per process, when the threshold is next read. Every change is a row in `decision_thresholds`; `noskill calibration revert` undoes the last one. Turning `auto_tune` off stops further changes but keeps the thresholds in force.

### Fallback: decisions on any model
Not everyone will have a JEV model, and skills must stay universal. When no decision model is available, `decide` is **emulated** with the chat model:
- constrained output with a JSON schema built from the questions: one property per question, `enum` of the labels for choice and score, a plain boolean for noul (read as `noul` = 1 or 0);
- if the backend returns token logprobs, they're turned into `probabilities`/`confidence`; otherwise confidence is reported as `null` and thresholds fall back to conservative defaults. Block 1a ships the `null` path; the logprob path comes with the backends in 1b.

The emulation is slower (hundreds of ms to seconds) and less calibrated, but the skill code is identical. `noskill doctor` recommends pulling a decision model when emulated decisions are frequent.

The proxy also **passes `/v1/systemone` through** to Ollama, and serves it via emulation when the backend lacks it, so other apps using the TypeSafe SDK can point at noskill too.

### Choosing a decision model (16 GB target)
Measured in [S3](spikes/S3-results.md); decided in [ADR-0006](adr/0006-decision-model-on-16gb.md).
- The decision model stays **resident** (pinned with `keep_alive`), since it's called on almost every request. It doesn't count toward `max_loaded_models`.
- ≤ 16 GB: `tev1:0.8b`. On an M4, the router's `task_type` question takes ~235 ms idle and is 81.5% right, the same as emulating on `qwen3.5:4b` at a tenth of the time.
- Above 16 GB: `tev1` (4B, 92.5% right) when `noskill models bench` shows it within the latency budget on that machine. It takes ~3 s for five questions on an M4 and grows to ~13 GB with its prompt cache.
- `nimble` (9B): ≥ 32 GB, untested.
- `laya` (421M) answers in 40–100 ms and keeps no prompt cache, but was 76% right on `task_type` (short descriptions) and less well calibrated in S3. It isn't a default; it's a candidate for 8 GB machines and short decisions.
- **Prompt cache.** Ollama's MLX runner caches prompts per model, up to ~8.5 GB (~30 MB per question per call). noskill reloads the decision model after a reply when `/api/ps` shows it more than `decision_cache_gb` (default 1 GB) above its weights. A reload costs ~1 s, about every 30 calls. noskill never changes Ollama's settings; `doctor` suggests a server-side cap where one exists.
- **`noul` thresholds** come from `noskill models bench` per decision and model. `tev1`'s `noul` is P(yes) (0.92 / 0.06 on yes / no items); `tev1:0.8b`'s is a score (0.80 / 0.22).
- `noskill models bench` measures decision latency and accuracy on a built-in labelled set (router + judge questions), then picks the default.

### Other non-chat models (later, P2)
Embedding-predictive models such as V-JEPA 2 / VL-JEPA could add video/image classification and retrieval (e.g. tagging anime episodes). They'd plug in as extra `embed`/`classify` providers via a Python sidecar. Not planned before v1.

## Model profile (stored in `state.db`)

```json
{
  "id": "ollama/qwen3:8b",
  "family": "qwen3", "params_b": 8.2, "quant": "Q4_K_M",
  "context": 40960, "capabilities": ["chat","tools","thinking"],
  "tool_strategy": "native",
  "bench": { "tok_s": 48.2, "ttft_ms": 210, "mem_gb": 6.1,
             "scores": { "code": 0.61, "research": 0.72, "extract": 0.80 } },
  "bandit": { "research:2": [14, 3] }
}
```

Decision model profile:

```json
{
  "id": "ollama/tev1:0.8b",
  "params_b": 0.8, "capabilities": ["decide"], "resident": true,
  "bench": { "decide_ms_p50": 235, "decide_ms_p50_loaded": 536, "mem_gb": 1.7, "cache_gb_per_call": 0.03,
             "accuracy": { "router.task_type": 0.815, "judge.answered": 0.85 },
             "calibration_ece": 0.05, "noul_thresholds": { "needs_tools": 0.2 } }
}
```
