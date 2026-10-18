# ADR-0015: LoRA from good traces with MLX through uv, gated, benchmarked, and swapped in per task type

Status: **accepted** · Date: 2026-10-18 · Implements FR-L8 ([LEARNING.md](../LEARNING.md#weight-learning-fr-l8-opt-in))

## Context
FR-L8 asks for this: export curated traces as a fine-tuning dataset, run LoRA (MLX on Mac, Unsloth or axolotl on CUDA), re-import the result as an Ollama model, and benchmark it before routing to it. Five things had to be settled:

1. **What a training example is.** A trace records the request, the tool calls with their results, and the answer. It doesn't record the system prompt or the tool list the model saw. Those come from the skills and tools at run time.
2. **When it's worth training.** On 2026-10-18 this machine had 12 traces, all job runs on `llama3.2:3b` (10 judged success, 2 partial). A LoRA on a few dozen examples mostly teaches the model those requests' wording.
3. **The trainer.** MLX and Unsloth are Python. ADR-0001 says that ML pieces run as on-demand Python sidecars through managed `uv`. A 16 GB Mac with a 6 GB model budget (ADR-0012) can train a 4-bit 3–4B model (QLoRA), but not a bf16 one.
4. **Getting it back into Ollama.** `mlx_lm.fuse --export-gguf` covers only Llama-family models. Ollama's `ADAPTER` line doesn't cover Qwen3 adapters from MLX.
5. **When the router may use it.** The new model is the same size as its base. If it were simply installed, the router would treat it as one more model and could send it any request.

## Decision
**Export** (`noskill train export [--task t] [--since 30d] [--base m]`): runs everywhere and writes `<data>/train/datasets/<id>/`.
- **Kept:** traces from real use only (sources `cli`, `proxy`, `tray`, `job`, `delegate`), whose outcome is `success`. The outcome must come from you or a check, or from the judge or an implicit signal with confidence ≥ 0.8.
- **Left out:**
  - Eval, learning, optimise and pinned-task runs. The benchmark below runs those tasks, so training on them would grade the model on its own homework.
  - A run with a refused or failed tool call, a run that hit a budget, and a run with no answer.
  - Duplicate requests: the same words with values removed (as for near-duplicates) and the same tool pattern.
- **Each example** is `{"messages": [...], "tools": [...]}`, mlx-lm's chat format with tools:
  - The system message is rebuilt from noskill's base prompt and the run's skills as they are now, in the small-model form for a small base.
  - The tool definitions are those skills' tools.
  - Memory and the file-access note are left out: both are personal and change from run to run.
  - Assistant tool calls carry their arguments as objects, and each is followed by its tool result. The answer is the last message.
  - A run becomes one example per assistant turn: the conversation up to that turn, ending with it. With `--mask-prompt`, mlx-lm trains only an example's last message, so this way every tool call and the answer are trained.
- **Scrubbing:** secret values are replaced again, as when the trace was written, and your home folder becomes `~`.
- **Split:** about 10% of examples go to `valid.jsonl`, chosen by a hash of the run ID so that the split is stable.
- **Review:** the export writes `REVIEW.md`, every example as plain text, and `MANIFEST.json`, which lists the counts and the runs used. You can delete lines from `train.jsonl` by hand.

**Gate:** `train lora` refuses when:
- the dataset comes from fewer than `learning.lora.min_runs` runs (default 200; its examples are their turns);
- `--reviewed` isn't given. That flag says you read REVIEW.md, and it's recorded with the run.
- it's on battery (unless `--force`);
- the machine isn't Apple Silicon. The Unsloth (CUDA) path isn't built: that part of FR-L8 is deferred until someone with an NVIDIA machine needs it;
- it has less than 8 GB of memory available right now.

**Train** (`noskill train lora --data <id> --base qwen3:4b --reviewed`):
- The base's MLX repository comes from `learning.lora.mlx_models`. The defaults are 4-bit `mlx-community` builds, so this is QLoRA.
- It runs `uv run --isolated --with mlx-lm==<pin> python -m mlx_lm lora`:
  - `--train --mask-prompt --grad-checkpoint --batch-size 1 --num-layers 8 --max-seq-length 4096`
  - iterations: 2 epochs, between 100 and 1000.
- It then runs `mlx_lm fuse --dequantize` and `ollama create <base>-ns<n> -q q4_K_M`. The Modelfile is the base's own (`ollama show --modelfile`) with `FROM` pointing at the fused folder, so the base's chat template and parameters carry over.
- Every step's output goes to `<data>/train/runs/<n>/train.log`, with live progress. `--dry-run` prints the commands without running them.
- uv downloads mlx-lm and the base weights (about 2.5 GB for a 4B model) the first time. `train lora` says so before it starts.

**Benchmark:**
- The new model and its base run the same checked tasks: those of the dataset's task type, or all agent tasks when it has none.
- The new model is accepted under ADR-0014's rule: at least `max(2, 10%)` more tasks passed, or as many passed with at least 15% fewer steps.
- Its RUN.json records base, tag, dataset, task type, both scores and status (`candidate`, `active`, `reverted`, `rejected`).
- A rejected model is removed from Ollama (`ollama rm`). It's a model noskill made itself, not a user file. The adapter and logs stay.

**Routing:**
- Every model in `<data>/train/runs` is left out of routing unless it's `active`.
- `noskill train adopt <n>` makes it active. After routing, a request of its task type (or any type, when it has none) whose pick is the base runs on the tuned model instead. The route explanation says so.
- `noskill train revert <n>` stops that.
- `noskill train list` shows the runs.
- Like tuned skills, it's never adopted on its own.

## Consequences
- **Gated today.** With 12 good traces, `train lora` refuses, and that is the intended behaviour. The pipeline is tested with a fake runner. No real training has run yet, and the mlx-lm pin, the fuse flags and Qwen3 safetensors import into Ollama 0.40 are unchecked until the first real run. ROADMAP records that.
- **The system prompt is rebuilt from today's skills**, not the run's. A skill rewritten since the run trains the model on a prompt it never saw. That only matters within one dataset's window (`--since`, default 30 days).
- **One example per turn** repeats a run's early messages in each of its examples. A 5-call run costs about 3 times the tokens of a single example. That's a small cost at a few hundred runs.
- **No Python in the core.** uv is needed only for `train lora`, and `noskill doctor` doesn't require it.
