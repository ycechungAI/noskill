# ADR-0012: Half a 16 GB machine: 6 GB of models, more only while memory is free

Status: accepted (block 2c). Supersedes the 8 GB budget in [ADR-0007](0007-16gb-memory-budget-and-model-defaults.md); its measurements and model-size table still stand.

## Context
ADR-0007 gave models 8 GB of a 16 GB Mac, assuming noskill and a normal set of apps share the machine. The user's machine is actually split: about 8 GB goes to other work that must not be pushed into swap, and noskill gets the other 8 GB, including the OS share it causes, its own process and the decision model. When the other work isn't running, the whole machine is available, up to about 16 GB.

Two constraints limit "up to 16 GB":
- Metal lets the GPU wire at most about ⅔ of RAM on Apple Silicon (10.7 GB on 16 GB). A model larger than that runs partly on the CPU or doesn't load.
- How busy the machine is changes during the day. A fixed bigger number would hit the other work whenever it starts again.

## Decision
- **Base budget on 16 GB Apple Silicon: 6 GB** (resident + primary). That leaves ~2 GB of the user's 8 GB half for noskill itself, its scripts and the OS cost of loading models.
- **Burst** (`orchestrator.memory_burst_gb`, default `auto`): for each routing decision, the budget is
  `clamp(free memory + memory noskill's models already hold − 2 GB reserve, base, ceiling)`,
  and only when free memory is known and the OS reports normal pressure. Free memory: macOS `kern.memorystatus_level` × RAM, Linux `MemAvailable`, Windows `ullAvailPhys`.
- **Ceiling** with `auto`: ⅔ of RAM on Apple Silicon (the Metal limit, 10.7 GB on 16 GB, not 16), ¾ of RAM on CPU-only machines, and no growth on a discrete GPU (VRAM is the limit). A number sets it; `0` turns burst off.
- **Short hold**: a model picked only because of burst gets `keep_alive` 2 minutes after its run (same `num_ctx`, so no reload), instead of the usual hold. It leaves memory soon after the work, since the other work may come back.
- **Cache cap for every model noskill uses**, not only the decision model: ADR-0006's reload was never built, and Ollama's MLX runner grew `qwen3.5:4b` from 5.0 to 12.7 GB during the block 2c simulation, more than the whole share. After each run, noskill unloads its own models (decision, embedding, the run's driver) that are more than `decision_cache_gb` over their fresh size (the smallest size seen after a run; 1.5 × the file before that). Models the user loaded are left alone.
- `noskill route explain` and `noskill doctor` show the base, the grown budget and why.

## Consequences
- With `tev1:0.8b` resident (~2.7 GB at most), the primary slot is ~3.3 GB at base. **`qwen3:4b` at 8k (3.9 GB) no longer fits at base**; a 3B model (`llama3.2:3b`) is the base primary, and `qwen3:4b`, `qwen2.5-coder:7b` and the 8B models load when burst allows them. Routing already picks the best model that fits, so this needs no new rule, only lower quality at base.
- Under pressure (as on a busy machine), noskill stays at 6 GB and may run a weaker model than on an idle one. Results of the same request can differ by time of day; traces record the budget and its note.
- `qwen3:4b` with a q8_0 KV cache (3.3 GB) or at 4k context would fit at base; noskill doesn't change Ollama settings itself, so this stays a `doctor` suggestion.
- Not yet measured: a burst load followed by the other work starting. The 2 GB reserve and 2-minute hold are first guesses; the S4-style pressure sampler should check them on the user's machine.
- Other machine sizes keep the ADR-0007 rows (⅔ of RAM, VRAM − 0.5 GB, 50% CPU-only) and gain burst on top.
