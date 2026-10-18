---
name: research
description: Do multi-step research on a topic and write a cited report. Use for comparisons, "what's the best X", deep dives, or any question that needs several sources checked against each other.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  triggers: ["research", "compare", "deep dive", "report on", "pros and cons", "best"]
  task_types: [research]
  min_model: { context: 8192, tools: any }
  depends_on: [web-search, files]   # files: to save the report
  permissions:
    network: ["*"]
    filesystem: ["write:$DOCUMENTS/noskill-research"]
    exec: false
  tools:
    - builtin: plan_questions      # structured output: list of sub-questions
    - builtin: notes               # append/read a scratch notes file for this run
    - builtin: delegate            # run a sub-question on another model via orchestrator
---

# Research

## Steps
1. Restate the goal in one line. Call `plan_questions` to list 3–6 sub-questions.
2. For each sub-question, use the **web-search** skill. Save key facts with their URLs using `notes`.
3. After all sub-questions: read your notes. Find gaps or conflicts. Search again for those (max 2 extra rounds).
4. Write the report from notes only:
   - **Summary** (3–5 lines, answer first)
   - **Findings** by sub-question
   - **Disagreements / uncertainty**
   - **Sources** (numbered, cited inline as [1], [2])
5. If the user asked to save it, use `write_text` to create `$DOCUMENTS/noskill-research/<date>-<slug>.md`. Use `make_dir` first if the folder is missing.

## Rules
- Each claim needs at least one source. Important claims need two independent sources.
- Prefer primary sources (official docs, papers, maker sites) over blogs and SEO pages.
- Note the date of time-sensitive facts.

<!--
=== Implementation spec ===
- Budget defaults: 25 tool calls, 15 fetches, 10 min wall-clock; configurable per run (--depth quick|normal|deep).
- Parallelism: sub-questions run concurrently up to N (default 2; 1 on ≤16 GB machines) using `delegate`
  so a small fast model can read/summarise pages while the driver model plans and writes.
- Notes are stored in the trace so the learning loop can mine good research patterns
  (e.g. learn "product-comparison" or "release-notes-lookup" sub-skills).
- Outcome checks: every [n] citation resolves to a fetched URL; no uncited numeric claims (heuristic).
- Prior art: gpt-researcher (planner/executor/publisher), Perplexica.
-->
