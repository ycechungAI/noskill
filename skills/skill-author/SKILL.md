---
name: skill-author
description: Turn a successful run into a reusable skill, or fix an existing learned skill from failed runs. Use when the learning loop queues a run, or when the user says "remember how to do this", "make this a skill" or "fix that skill".
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan, code]
  min_model: { context: 8192, tools: any }
  permissions:
    network: []
    filesystem: ["read:traces", "write:skills/learned"]
    exec: false
  tools:
    - builtin: read_trace        # { run_id } → compressed action list + request + outcome
    - builtin: write_skill_draft # { name, files: {path: content} } → skills/learned/<name> as draft
    - builtin: run_skill_tests   # { name } → sandbox test results
    - builtin: retrieval_check   # { name } → does the source request retrieve it; do unrelated ones not
    - builtin: delegate          # script writing goes to the code model
  decisions:
    is_parameter:
      noul: "Should this value change between uses of this task (an input), rather than stay fixed?"
      threshold: 0.6
    deterministic:
      noul: "Can these steps run in order without any judgement between them?"
      threshold: 0.8
---

# Skill author

Work in passes. Finish each pass before starting the next.

## New skill
1. `read_trace`. Write the action list: one line per tool call, `tool(args) → result`.
2. For each value in the actions (paths, names, URLs, numbers): if it's in the user's request, it's an input. Otherwise use `decide("is_parameter")`. Inputs get the trace value as default.
3. Write the "Steps" section: short, numbered, one action per step, using input names in `{braces}`.
4. Use `decide("deterministic")`. If yes, `delegate` (task_type `code`) a script tool that does the steps from the inputs.
5. Write `tests/cases.yaml`: the original request with its end state, plus 1–2 variations with different inputs.
6. Write `name`, `description` (when to use it, one sentence), `triggers`, and `permissions`: only what the trace used.
7. `write_skill_draft`, then `run_skill_tests`. If a test fails, fix using the test output (max 2 tries).
8. `retrieval_check`. If it fails, rewrite the description (max 2 tries).

## Fix a skill
1. `read_trace` for each failed run, and read the current skill.
2. Find the step that went wrong. Change as little as possible.
3. Add the failed case to the tests. Keep all old tests.
4. `write_skill_draft` as a new version, then `run_skill_tests`.

## Rules
- Never add permissions the trace didn't use.
- Never put secrets, personal data or one-off values in the skill unless they're inputs.
- Keep `SKILL.md` under 60 lines. Put details in `references/`.

<!--
=== Implementation spec ===
Driven by the learning loop (docs/LEARNING.md §3–6); also callable via `noskill learn`.
Model: learning.author_model (default: best local general model); scripts via delegate → code model.
Each pass uses constrained output (JSON schema per pass) so 7–8B models stay on track.
Static checks after drafting (in Go, not by the model): permission subset, secret scan, script lint,
  SKILL.md size, front-matter schema.
Outputs land as status=draft; verification/promotion are core logic, not this skill's job.
Tests: 10 recorded traces → expected drafts pass their own tests ≥ 8/10; permission-escalation
  attempt is always rejected; retrieval check catches a deliberately vague description.
-->
