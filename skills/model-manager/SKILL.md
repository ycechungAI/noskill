---
name: model-manager
description: Manage local models. List, suggest, pull, update, benchmark and remove models, fitted to this machine's memory. Use when the user asks which model to use or get, wants to free disk space, or when routing finds a missing capability (no vision model, no decision model).
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan]
  min_model: { context: 4096, tools: any }
  permissions:
    network: ["localhost", "ollama.com", "registry.ollama.ai"]
    filesystem: []
    exec: false
  confirm: [pull_model, remove_model]
  tools:
    - builtin: list_models      # installed models: size, capabilities, scores, last used
    - builtin: machine_info     # RAM/VRAM, memory budget, free disk
    - builtin: suggest_models   # { need: task_type|capability } → candidates that fit the budget
    - builtin: pull_model       # { name } → progress; then auto-bench
    - builtin: remove_model     # { name }
    - builtin: bench_model      # { name }
---

# Model manager

## Suggest or get a model
1. `machine_info` and `list_models`.
2. `suggest_models` for the need. Only suggest models that fit the memory budget with the resident models loaded.
3. Explain the pick in one line each: size, what it's good at, how it compares to what's installed.
4. If the user agrees, `pull_model`. It's benchmarked automatically after download.

## Free up space
1. `list_models`. Mark models not used in 30 days, and duplicates (same family, different quantisation).
2. Never suggest removing the decision model, the embed model, or a model a rule or schedule uses.
3. Show the list with sizes. Remove only what the user approves.

<!--
=== Implementation spec ===
- suggest_models uses a curated catalog (shipped JSON, updated with releases): family, sizes, quants,
  capabilities, typical scores, memory at 8k ctx. Filtered by the orchestrator's memory budget.
- Triggered automatically (as a suggestion in the approvals queue, never a silent pull) when:
  routing hits a hard need with no capable model; no decision model installed; Ollama < 0.35.
- Updates: weekly check of `ollama` registry digests for installed tags → "update available" in tray.
- After pull/remove: routing table re-derived; `route explain` reflects changes.
Tests: budget filter (16 GB never suggests > 8B primary); protected models never offered for removal.
-->
