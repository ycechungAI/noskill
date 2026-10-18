# Orchestrator (model routing)

Goal: the agent **knows when to switch to a different local model** for a task, accounting for quality, speed and what your hardware can hold in memory. Design target: a **16 GB machine running 4B–8B models** (decision Q7), with half of it left to the user's other work: 6 GB of models, more while memory is free ([ADR-0012](adr/0012-shared-machine-6gb-budget-with-burst.md)).

The orchestrator is the built-in skill [`skills/orchestrator`](../skills/orchestrator/SKILL.md) backed by Go code in the core. It's a skill so its rules are visible, editable and improvable.

## Inputs

| Input | Source |
|---|---|
| Task type, difficulty, needs | Classifier (below) |
| Required capabilities | Request (images → vision; tools needed → tool-capable; long input → context) |
| Model profiles | `noskill models bench`: tok/s, TTFT, load time, memory at the chosen context, score per task type |
| Machine state | Loaded models + sizes (Ollama `/api/ps`), memory budget, active streams, power state |
| User rules & preset | `config.toml` |
| History | Bandit stats per (task type, difficulty, model) from trace outcomes |

## Classification (FR-O1)

Tiered, cheapest first, stop at the first confident answer:
1. **Rules/heuristics** (~0 ms): attachments, code fences, file extensions, keywords, skill already selected.
2. **Embedding kNN** (~5–20 ms): nearest past requests with a known task type.
3. **JEV decision call** (~250 ms with `tev1:0.8b` on a 16 GB M4; budget ≤ 300 ms idle, ≤ 600 ms while a model generates): one `/v1/systemone` call to the resident decision model asking `task_type` (choice, with option descriptions). `difficulty` (score) and `needs_vision` / `needs_web` / `needs_tools` (noul) are added only when the model choice or `think` depends on them (e.g. `difficulty` to decide `think` below): each extra question re-reads the request, costing ~80–100 ms and ~30 MB of prompt cache ([S3](spikes/S3-results.md), [ADR-0006](adr/0006-decision-model-on-16gb.md)). Confidence < 0.6 falls through.
4. **Emulated decision** (~2 s on a 4B model): same questions via the loaded chat model with constrained JSON, only when no decision model is installed.

Task types (extendable): `chat, code, reasoning, math, research, summarise, extract, translate, creative, vision, plan`.

How the tiers behave (as built in block 1a, `internal/orchestrator`):
- Heuristics decide only on strong signals: an image (→ `vision`, hard need), a skill selected strongly (its task type: forced with `--skill`, matched by a trigger phrase, or an embedding match ≥ 0.7; a plain keyword match isn't enough), a code fence, or a source-file name together with a code verb ("fix the nil check in server.go"). A URL or words like "latest" and "today" add the `web` need but don't set the task type.
- `vision` is never offered as a choice to the decision model: text alone can't need vision.
- **Below the threshold** the top choice is kept but marked *uncertain*. An uncertain task type doesn't fire task-type rules or soft swaps, so noskill stays on the loaded model rather than swapping on a guess. Hard needs still apply.
- Emulated decisions (tier 4) give no confidence until logprobs are wired up, so they're accepted as confident: otherwise `code → qwen2.5-coder:7b` would never fire without a decision model.
- If the decision call fails, the type is `chat`, uncertain.
- Tier 2 (embedding kNN) needs past outcomes and arrives with learning.
- `think` is turned on when `difficulty` is Moderate or harder (asked only when needed).

## Memory budget

Everything below is about one number: **how much memory models may use**.

| Machine | Default budget | Why |
|---|---|---|
| Apple Silicon, 16 GB unified | **6 GB**, more while free (burst) | Half the machine is the user's other work ([ADR-0012](adr/0012-shared-machine-6gb-budget-with-burst.md)). [S4](spikes/S4-results.md) had already shown that 9 GB of models with normal apps open kept macOS at its pressure warning half the time; the earlier 8 GB is in [ADR-0007](adr/0007-16gb-memory-budget-and-model-defaults.md) |
| Apple Silicon, other sizes (including 8 GB) | ⅔ of RAM | Metal's working-set limit. Not measured yet; `models bench` and `doctor` report the pressure level and can lower it |
| PC with discrete GPU | VRAM − 0.5 GB | Models that spill into system RAM run several times slower, so the budget is VRAM, not RAM |
| CPU only | 50% of RAM | Leaves room for the OS; slow but works |

Override with `memory_budget_gb`.

**Burst** (`memory_burst_gb`, default `auto`, [ADR-0012](adr/0012-shared-machine-6gb-budget-with-burst.md)): each routing decision may use `free memory + what noskill's models hold − 2 GB`, between the default budget and a ceiling: ⅔ of RAM on Apple Silicon (Metal's limit, 10.7 GB on 16 GB), ¾ of RAM CPU-only, no growth on a discrete GPU. Only while the OS reports normal pressure. A model picked only because of burst is held for 2 minutes after its run, not the usual time, so the memory goes back soon.

The 16 GB figures were measured on one Mac mini M4 in Phase 0 ([S3](spikes/S3-results.md), [S4](spikes/S4-results.md)); the other rows are estimates.

### Slots
The budget is split into **slots** instead of letting Ollama evict models at random:

| Slot | Holds | Policy |
|---|---|---|
| **Resident** | Decision model (`tev1:0.8b` on ≤ 16 GB, `tev1` above), embedding model (`nomic-embed-text`) | Pinned (`keep_alive: -1`). Used on almost every request. The decision model is reloaded after a reply when its prompt cache passes `decision_cache_gb` ([ADR-0006](adr/0006-decision-model-on-16gb.md)) |
| **Primary** | One general, code or vision model | Swapped by the rules below |
| **Secondary** | A second chat model | Only if the budget fits two (≥ 24 GB machines) |

### Measured fit on a 16 GB Mac (8k context, f16 KV cache, Ollama's reported size)

| Model | Size loaded | At 16k context | With q8_0 KV (8k) | Time to load, warm / after other models |
|---|---|---|---|---|
| `tev1:0.8b` (decisions) | 1.7 GB, up to ~2.7 GB with its prompt cache (reloaded past 1 GB over) | n/a | n/a | under 1 s |
| `nomic-embed-text` | ~0.3 GB (not measured) | n/a | n/a | n/a |
| `qwen3:4b` | 3.9 GB | 5.1 GB | 3.3 GB | 1.1 s / 2.6 s |
| `qwen2.5-coder:7b` | 5.1 GB | 5.6 GB | 4.9 GB | 1.3 s / 4.1 s |
| `qwen3:8b` | 6.6 GB | 7.8 GB | 6.1 GB | 1.6 s / 6.4 s |
| `qwen2.5vl:7b` | 7.3 GB | 7.8 GB | 7.1 GB | 2.6 s / 6.1 s |

Resident (`tev1:0.8b`, ~2.7 GB at most) + one primary must fit the **6 GB** budget, which leaves ~3.3 GB for the primary. So on 16 GB at base: **a 3B model (`llama3.2:3b`) fits; `qwen3:4b` (3.9 GB), the coder 7B and the 8B models need burst** (ADR-0012). With burst up to 10.7 GB all of them fit, the vision 7B with only 0.7 GB to spare. S4 measured them next to the resident model for 200 requests (with other apps open): the resident model next to them for 200 requests: `qwen3:4b` and the coder rarely reached the macOS warning level, `qwen3:8b` reached it in 12 of 21 samples (14% free, swap +3 GB) and `qwen2.5vl:7b` in 8 of 21 (10% free).

A model bigger than the budget is allowed only when:
- free memory lets the budget grow (burst), or the user raises `memory_budget_gb`, or
- the request needs it (a hard need such as an image) and noskill unloads the resident decision model for the time being, so decisions are emulated by the loaded model until it swaps back.

GGUF models (the ones above) keep a fixed size however many requests they serve. Only MLX-runner models grow ([S4](spikes/S4-results.md#findings)): in the block 2c simulation `qwen3.5:4b` (MLX) went from 5.0 to 12.7 GB at the same context. After each run noskill unloads any of its own models (decision, embedding, the run's driver) that is more than `decision_cache_gb` over its fresh size, so its next use loads it fresh ([ADR-0012](adr/0012-shared-machine-6gb-budget-with-burst.md)). The fresh size is the smallest size seen after a run (sizes taken right after loading are too small, since Ollama allocates its working buffers on the first request); before one is seen it assumes 1.5 × the model file. Models you loaded yourself are never touched.

### Ollama settings noskill recommends
noskill doesn't change your Ollama server settings itself. `noskill doctor` checks them and shows how to set them for your OS:
- Optional, only when memory is tight: `OLLAMA_FLASH_ATTENTION=1` and `OLLAMA_KV_CACHE_TYPE=q8_0`. It saves 0.2–0.6 GB at 8k context (about 0.4–1.1 GB at 16k), not half of the model's memory. In S4 it cost 2.7 points of tool-calling success on `llama3.2:3b` (within seed noise but over the 2-point limit), so it isn't on by default; the `init` probe checks quality before `doctor` suggests it.
- `OLLAMA_MAX_LOADED_MODELS` ≥ 3, so resident + primary can coexist.
- A cap on the MLX runner's prompt cache, if your Ollama version has one. S3 measured up to ~8.5 GB per model ([S3](spikes/S3-results.md#memory)). Without it, noskill keeps the decision model small by reloading it.
- **Fixed context per model**: Ollama reloads a model when `num_ctx` changes, so noskill picks one context size per model at `init` (8k default, 16k if it fits) and always sends that. Long inputs are summarised or chunked rather than raising `num_ctx` mid-session.

## Selection (FR-O2, FR-O3)

```
candidates = models with required capabilities, context ≥ needed, and that fit the primary slot
if a user rule matches → pick it (unless a hard need rules it out)
score(m) = w_q·quality(m, task, difficulty)        # bench score blended with bandit posterior
         − w_l·expected_latency(m, input_len)       # includes load time if not loaded
pick argmax
```

Preset weights: `fast` (w_l high), `balanced` (default), `best` (w_q high). Block 1a uses w_q = 1 and w_l = 0.05 / 0.02 / 0.005 per second for fast / balanced / best, with latency = load time (if not loaded) + 300 output tokens at the model's measured tok/s (30 until benched). Unbenched models score 0.5 on every task type. These are starting values for `models bench` and the bandit to replace.

Fit: a model must fit in the budget next to the resident models. The only exception is a hard `vision` need, where the pick may unload the resident models for the time being (`UnloadResident`). Context is a soft filter: if no model's context holds the input, routing ignores context and the input is summarised or chunked.

### When to swap the primary model
1. **Hard need** (image input, tool support, context too small) → swap.
2. **User rule** (e.g. `code → qwen2.5-coder:7b`) → swap.
3. **Soft gain** → swap only if the quality gain ≥ **0.15** (on the 0–1 task score) **and** the gain is worth the load time under the preset. Swap time is measured per model at `init`/`models bench`. On a 16 GB M4 (S4) it was 2.8 s for a 4B, 4.3 s for a 7B and 7 s for an 8B, because the model you switch to is rarely in the file cache.
4. **Hysteresis**: a model that was just loaded stays for at least **120 s** or the rest of the current turn. There's no swapping back for small gains, which prevents ping-pong.
5. **Never evict a model that's streaming** an answer to a client.
6. **Background jobs** use the loaded primary if it meets the skill's `min_model`. Otherwise they wait until you've been idle for 10 minutes before swapping.
7. **Batch delegations**: when a plan has several sub-tasks for different models, they're ordered to group by model where dependencies allow (one swap instead of three).
8. **Driver handoff**: if a hard need forces a swap mid-task and little work remains, the newly loaded model finishes the task instead of swapping back.

### Default routing table (16 GB, with burst)
Set by `init` from what you have installed and from benchmark scores. These are the starting values:

| Task type | Model | Notes |
|---|---|---|
| chat, research, reasoning, math, plan, translate, creative | `qwen3:4b` when burst fits it, else `llama3.2:3b` (`qwen3:8b` if the budget fits it) | `think` on when difficulty ≥ 3 |
| code | `qwen2.5-coder:7b` | Rule; needs burst (5.1 GB); the general model otherwise |
| vision | `qwen2.5vl:7b` | Optional pull; hard need. Needs the resident decision model unloaded while it's loaded (7.3 GB) |
| summarise, extract | **whatever is loaded** | Every model here is good enough, so it never swaps |
| decisions (routing, judging, matching) | `tev1:0.8b` (`tev1` above 16 GB) | Resident |

## Worked examples (16 GB Mac, resident: `tev1:0.8b` + embed)

**A. Research question, no swap.**
*"What changed in Ollama 0.35?"* → decision 240 ms: `research` (0.91), which implies the web → `qwen3:4b` already loaded → web-search + research skills → cited answer.

**B. Code mid-chat, one swap.**
*"Write a Go function that parses this RSS feed."* → `code` → rule → `qwen2.5-coder:7b`. Swap: unload `qwen3:4b`, load coder (~4 s, measured at bench time). The next message, *"thanks, explain it simply"* → `chat`: `qwen3:4b` scores 0.06 higher, below 0.15 → **stay** on the coder. No swap back.

**C. Screenshot of an error, hard need.**
User pastes a screenshot → needs_vision 0.99 → swap to `qwen2.5vl:7b` (~6 s; the resident decision model is unloaded meanwhile, so decisions are emulated by the vision model). It reads the error and the remaining work is short ("explain the fix") → driver handoff: the vision model answers. One swap instead of two.

**D. 03:00 anime-tracker job.**
You're idle, `qwen3:4b` is loaded and meets the skill's `min_model` → runs on it, no swap. RSS-item matching uses `tev1:0.8b` decisions, which are resident. If you'd left the coder loaded, it also meets `min_model` (8k context, tools), so it runs on that.

**E. Research with a code sub-task.**
Plan: 4 web sub-questions + 1 "write a script to compare these numbers". Batched: all 4 research sub-tasks on `qwen3:4b` first, then one swap to the coder for the script, and the coder writes the final answer (handoff). One swap total.

## Escalation and delegation (FR-O5, FR-A5)
- **Escalate**: output fails a check (schema invalid, tests fail, or `decide("looks_correct")` < 0.5) → retry once on the next-best model → record for learning. On 16 GB "next-best" is usually the coder 7B (or an 8B if the budget allows it), since a 14B model doesn't fit.
- **Delegate**: the driver calls `delegate(task_type, prompt)` to run a sub-task on another model (subject to the swap rules above).
- **Cascade** (optional, `fast` preset): answer with the loaded model; if `looks_correct` is low, re-answer with the best-scoring model.

**As built (block 2a).**
- **Escalation** covers hard failures only:
  - the run errored (not cancelled or waiting);
  - it used up its token budget;
  - it answered with nothing.

  The request then runs once more with the failed model excluded, so the router picks the next-best one. A progress line in the first run's trace says why, and the result lists the failed run in `EscalatedFrom`.

  It never escalates:
  - pinned runs, runs it already escalated, trial or verification runs, or resumed runs;
  - once answer text has reached the client;
  - when the first run left a side effect: an undo-log entry, or a successful call from a skill that writes files, runs programs or asks before acting.

  A run that hit the step limit already wrote a best-effort answer, so it isn't escalated. The `looks_correct` check and the cascade aren't built.
- **Batch delegations (rule 7).** `delegate` takes a `tasks` list of up to 6 independent sub-tasks. Each is routed first. The sub-tasks the loaded model can take run first, then the rest grouped by model in order of first appearance. Results come back in the order given. A batch counts as one of the run's 3 delegations.
- **Driver handoff (rule 8).** After a delegation runs on another model, noskill checks before the driver's next step whether the driver is still loaded. If it isn't, the delegated model takes over when it can drive the run: chat, tool calling when tools are on, at least the driver's context. It must also be no more than `min_swap_gain` worse on the task (bandit posterior means). The trace gets a "Handing off" line. The run's `driver_model` becomes the model that finished, which is the one the bandit credits.
- **Background jobs (rule 6).** A job uses the loaded primary when the primary can serve it. A job that needs a swap waits while you've used noskill in the last 10 minutes; it releases the generation slot and checks again each minute. "Used" means a run that wasn't started by a job or by learning, or an interactive request waiting now.

<a id="learned-routing"></a>
## Benchmark quality (block 2b)
`noskill models bench <model>` also runs the 12 `bench` tasks of the eval suite on the model and stores a quality score per task type, `(passed + 0.5) / (tasks + 1)`, in `model_bench`. The router reads these as `Profile.Quality`, with `*` (the mean) for the task types the micro-eval doesn't cover, and `4 ×` that value is the bandit's prior. A model that was never benched stays flat at 0.5. `route explain` prints `score = quality … , N rated runs, ~T s` for every candidate.

## Learned routing (FR-O4)
- Contextual bandit: Thompson sampling, Beta per (task type, difficulty bucket, model).
- Reward = outcome success, minus a latency penalty from the preset (including load time).
- Exploration ≤ 5% of interactive, non-pinned requests, and only among models that need **no swap**. Never for jobs with side effects.
- Cold start uses benchmark scores as priors.

**As built (block 2a).**
- **Arms.** One per (task type, difficulty bucket, model name). Buckets: 0 for trivial or easy (difficulty < 1.5), 1 for moderate or not asked, 2 for hard or very hard.
- **Prior and posterior.** The prior is the model's benchmark quality for the task type (0.5 until benched), worth 4 runs. The `bandit` table holds evidence only: α = the sum of rewards, β = the sum of their complements. So a new benchmark changes the prior without touching what was learned. Routing uses the posterior mean, Beta(4q + α, 4(1−q) + β).
- **Reward.** 1 for success, 0.5 partial, 0 fail, minus min(0.5, w_l × the run's seconds), where w_l is the preset weight above. An outcome with confidence under 0.6 doesn't count. Rewards are written when an outcome is recorded. A changed outcome replaces the run's earlier reward (`runs.reward`) instead of adding to it. Pinned and `--no-learn` runs teach nothing.
- **Explore.** With probability `explore`, an interactive (cli, proxy, tray) non-pinned request samples the loaded model's quality from its posterior instead of using the mean. So exploration only ever keeps the loaded model; it never causes a swap. It's never used for jobs, delegations or learning.

## Config

```toml
[orchestrator]
preset = "balanced"           # fast | balanced | best
memory_budget_gb = "auto"     # auto | number
decision_model = "tev1:0.8b"  # "" = emulate with the chat model; "tev1" recommended above 16 GB
decision_cache_gb = 1         # reload the decision model when its prompt cache passes this
embed_model = "nomic-embed-text"
min_swap_gain = 0.15
hold_seconds = 120
explore = 0.05

[[orchestrator.rules]]
when = { task_type = "code" }
use  = "qwen2.5-coder:7b"

[[orchestrator.rules]]
when = { needs = ["vision"] }
use  = "qwen2.5vl:7b"

# `preset` limits a rule to one preset, so one model is the fast default and another the quality choice
# (measured on this project's machine, 2026-10-18: granite4:3b 9/10 checked files tasks at 9 s, 2.9 GB; qwen3.5:4b 10/10 at 14 s, 4.2 GB):
[[orchestrator.rules]]
when = { needs = ["tools"], preset = "best" }
use  = "qwen3.5:4b"

[[orchestrator.rules]]
when = { needs = ["tools"] }
use  = "granite4:3b"
```

## Explainability (FR-O6)

```
$ noskill route explain "refactor this function to use generics"
task_type=code (decision 0.93)
budget 9.4 GB (grown from 6.0 GB while 8.7 GB is free) · resident 2.7 GB (tev1:0.8b, nomic-embed-text) · primary: qwen3:4b (3.9 GB)
candidates: qwen2.5-coder:7b (rule 'code') ✓, qwen3:4b (loaded, score −0.12)
picked: qwen2.5-coder:7b: user rule; swap qwen3:4b → coder, est. load 4.3 s, fits (7.8/9.4 GB), held 2 min after (burst)
```

## Prior art
RouteLLM (trained routers, cost/quality), LiteLLM (proxy/fallbacks), paularlott/llmrouter (Go gateway with scriptable "auto" model and an Ollama-compatible API). See [PRIOR_ART.md](PRIOR_ART.md). noskill's difference: **memory-slot planning for one local machine, routing learned from your own outcomes, decision-model classification, and routing exposed as a skill.**
