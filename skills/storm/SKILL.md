---
name: storm
description: Research a topic from five viewpoints (practitioner, academic, skeptic, economist, historian), one at a time, each checked against the others, then merge them into one cited report. Use for "research this thoroughly", "look at it from every angle", "a full report with sources", or a topic where one search is not enough.
license: MIT
noskill:
  version: 0.2.0
  origin: builtin
  status: active
  triggers: ["storm", "deep research", "thorough report", "every angle", "multiple perspectives", "five perspectives", "full report with sources"]
  task_types: [research]
  min_model: { context: 8192, tools: native }
  permissions:
    network: ["*"]
    filesystem: ["write:$DOCUMENTS/noskill-research"]
    exec: false
  tools:
    - builtin: storm_run     # { topic, questions? }: the whole research, run in Go; writes the report folder
---

# STORM research

## When to use
A topic that needs more than one search, where you want sources checked from several sides. It takes a long time. For a quick answer use **research**.

## Steps
1. Call `storm_run` with the topic. Set `questions` to 2 for a quick look, leave it at 3 normally, 5 for a deep one. Wait for it: it makes many searches.
2. Answer with its `summary`, then the path in `report`. If `unsupported` is more than 0, say how many questions no page could answer.
3. If it fails, say why in one line.

## What it does
Five viewpoints, one at a time, in this order:
1. practitioner: real-world use, what works in practice.
2. academic: theory, careful definitions, the big picture.
3. skeptic: weak evidence, hidden assumptions, overconfident claims. It also looks for a second source or a counter-claim for the main findings.
4. economist: incentives, costs, trade-offs, wider effects.
5. historian: how it developed over time.

Each viewpoint asks its own questions, searches, fetches pages and answers from the fetched pages only. Later viewpoints first check a doubtful claim from the earlier notes. The notes are merged into an outline and numbered sources, and the report is written section by section from the notes.

## Rules
- Don't search or write the report yourself. Only what `storm_run` wrote counts.
- Don't call `storm_run` twice for one topic.

<!--
=== Implementation spec (not shown to the model) ===
Source: STORM (Shao et al., "Assisting in Writing Wikipedia-like Articles From Scratch with Large
Language Models", NAACL 2024, Stanford): discover perspectives, simulate question-asking
conversations grounded in retrieved sources, curate an outline, write section by section with
citations. This is the user's five-viewpoint variant (practitioner, academic, skeptic, economist,
historian, run one at a time, each checking the others).
Why a tool and not steps (live check 2026-10-18, granite4:3b): given the 8-step procedure as
instructions it made six calls (plan_questions, one search, three fetches) and answered; it skipped
the viewpoints, the notes and the report. So the loop is Go (internal/app/storm.go) and the model
is given one small job at a time:
- questions: PlanQuestions per viewpoint (fixed fallbacks when the model can't); a cross-check
  question from the second viewpoint on; three check questions for the skeptic.
- research: Go does the retrieval, the model only reads. A short query job, then Go calls the
  web-search skill's tools itself (web.search, then web.fetch on up to three pages, one per site,
  encyclopedias skipped, pages under 300 characters dropped), and one job answers from the fetched
  pages with [n] page numbers. Go turns [n] into the page's address; a number that isn't a page is
  dropped; no fetched page or "no reliable source found" is recorded as unsupported. (A first
  version let the model call search and fetch in a run of its own: granite4:3b searched once and
  never fetched, so 11 of 12 questions came back unsupported.)
- notes: one lane per viewpoint in the run's folder (the `notes` tool's lane files), so the
  reference pool (builtin.NoteSources) numbers each distinct URL once.
- writing: an outline job, then one job per section with only the note lines most about it, URLs
  replaced by [n] so the model copies numbers; citations to numbers that don't exist are stripped
  and a section with none is marked as a gap; then a summary job. storm_run writes the files
  through the broker into $DOCUMENTS/noskill-research/storm-<slug>-<date>[-n]/ (a new folder).
- tool timeout 90 minutes (agent.DefaultBudget). Progress lines go to stderr.
- Learning: the trace is long and the work happens inside one tool call, so the learning loop has
  no procedure to learn; STORM is a seed skill. `skills optimise` has no checked tasks for it.
- Prior art: the storm plugin's researcher / writer agents (fetch before believing, no
  encyclopedias, untrusted page content, a global reference pool, section writers with no search).
-->
