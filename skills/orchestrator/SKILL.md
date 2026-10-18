---
name: orchestrator
description: Decide which local model should handle a task or sub-task, and hand work to it. Use when a task needs a skill the current model is weak at (code, vision, long documents, hard reasoning) or when a cheaper/faster model would do.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan]
  min_model: { context: 2048, tools: any }
  permissions:
    network: ["localhost"]
    filesystem: []
    exec: false
  tools:
    - builtin: list_models      # models + capabilities + loaded state + scores per task type
    - builtin: route            # {task, task_type?} → {model, reason}
    - builtin: delegate         # {task, model? | task_type} → result text
---

# Orchestrator

Most routing happens automatically before you see the request. Use these tools when, **during** a task, a part of it fits another model better.

## When to delegate
- Writing or fixing code → task_type `code`
- Looking at an image or screenshot → `vision`
- Input too long for your context → `summarise` (a long-context model)
- Hard math or multi-step logic → `reasoning`
- Many simple items (classify, extract fields) → `extract` (small fast model)

## Steps
1. Split out the sub-task. Make it self-contained: include all needed text, since the other model can't see this chat.
2. Call `delegate` with `task_type`. Only name a `model` if the user asked for one.
3. Check the result. If it's clearly wrong, call `delegate` once more with a note on what's wrong.
4. Use the result in your answer.

## Don't
- Don't delegate simple things you can do in one step.
- Don't delegate more than 3 times in one task unless the user asked for it.

<!--
=== Implementation spec ===
Core logic lives in Go (see docs/ORCHESTRATOR.md); this skill exposes it to the model.
- route(): rules → hard constraints → scoring (quality, latency incl. swap, memory) → bandit posterior.
- delegate(): runs a nested agent loop (own budget, no further delegation by default: depth limit 2),
  records parent/child trace link so outcomes credit both models.
- list_models(): from model profiles; marks loaded models and estimated load time.
Config: [orchestrator] in config.toml (preset, rules, explore, max_loaded_models).
Learning: outcomes of delegated runs update bandit stats for (task_type, difficulty, model).
Tests: rule precedence; vision request never routed to text-only model; swap-avoidance threshold;
       route explain output stable.
-->
