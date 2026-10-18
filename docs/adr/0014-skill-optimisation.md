# ADR-0014: Skill optimisation in Go, scored by the checked tasks, with tuned built-ins as an overlay

Status: **accepted** · Date: 2026-10-18 · Implements FR-L9 ([LEARNING.md](../LEARNING.md#prompt-optimisation-fr-l9))

## Context
FR-L9 asks for "offline DSPy/GEPA-style optimisation of skill instructions against the skill's eval set". Three things had to be settled:

1. **The engine.** DSPy and GEPA are Python libraries. noskill is one Go binary with no Python needed (ADR-0001). A Python step only for this would add an install and a second runtime.
2. **The metric.** Skills don't carry their own `tests/` except learned ones. The checked tasks in `internal/eval` are the only graded runs. 22 of them name `files`, and one names `code-runner`. The workload set that `eval simulate` replays adds 18 more `files` tasks. The multi-step tasks (`multi-*`) are the learning exit check's held-out set, and the weekly gate uses them too.
3. **Where the result goes.** Learned skills already have versions and a `.next` trial (ADR-0010). Built-in skills are embedded in the binary and can't be rewritten. A rewritten copy at learned precedence would never be picked over the built-in. A copy at user precedence would pretend the user wrote it.

## Decision
**Engine: a small GEPA-style loop in Go** (`internal/learning/optimise.go`):
- It starts from the current instructions and scores them on the training tasks.
- It then makes up to `budget` candidates (default 4). For each one it picks a parent from the Pareto front (a candidate that's best on at least one task, most such tasks first). It shows the author model up to three of that parent's failed tasks: what was asked, what the check wanted, the calls made and why the check failed. The model replies with up to 3 line changes (the heading, the line it replaces or nothing, and the new line), Go applies them to the parent, and the result is scored on all the training tasks.
- No DSPy or GEPA code is used; the idea is GEPA's (reflective mutation, a Pareto pool).
- The author model is the same local model the learning loop uses (`learning.author_model`). Nothing is sent to the cloud.

**Only the instructions change.** The front-matter (permissions, tools, version, triggers) is kept byte for byte, so a rewrite can't widen what the skill may do. Each candidate must also pass these checks, or it's dropped without a score:
- **Protected lines kept.** Every line of the original that says what must never or always happen (`never`, `always`, `don't`, `do not`, `only`, `must`) is kept word for word.
- **Size.** At most 25% longer than the original, and at most 8 lines more. Learned skills keep the 60-line limit. Small models pay for every line (AGENTS.md).
- **No task values.** It may not contain a file name or other value from a training task that the original didn't have. Instructions that name a test's files score well and help nobody.
- **It loads.** The skill loader must accept it, with no errors.

A candidate that fails a check gets one more try: the author model is told why it was turned down ("it names \"notes\" from a training task") and asked again, within the same budget slot. Small models often copy a folder name from the failed requests they were shown; the first dry run on `files` lost both candidates that way. Task values are matched as whole words in any case, so `log` doesn't match `catalog`.

**Changes, not whole rewrites.** GEPA asks for the whole new text. On `qwen3:4b` that failed on `files` (about 700 tokens of instructions): asked for the whole instructions as one JSON string, half the replies repeated a rule ("Never move a file to a path that is not in the same …") until the token limit, and without a schema the model reasoned aloud instead of writing instructions. A reply of at most 3 short line changes ends in about 200 tokens. The protected lines are shown marked `(fixed)`. Small models still aim changes at them, mostly by adding a clause to a rule, so Go keeps the rule as it is and adds that clause as a line of its own after it. Any other change to a fixed line is dropped. A change Go can't place goes at the end of its section, or at the end of the instructions.

**Metric and split:**
- **Training tasks:** the agent tasks and workload tasks that name the skill. The multi-step tasks and the user's pinned tasks for the skill are **held out**. They are never shown to the author model, and each is run only twice per optimisation: once for the original, once for the finalist.
- **Accepting a finalist** needs all of these:
  - It passes at least `max(2, 10%)` more training tasks than the original, or the same number (at least one) with at least 15% fewer total steps. With nothing passed, fewer steps only means it gave up sooner. That was added in block 5b, when trained models started using the same rule (ADR-0015).
  - It passes no fewer held-out tasks than the original.
  - It passes every required test of a learned skill (`tests/cases.yaml`).
- **Too few tasks:** a skill with fewer than 5 training tasks isn't optimised. The report says how to add some (`noskill eval pin`).

**Where results go:**
- **Learned skills:** the finalist goes to `.next/` and takes the usual trial (FR-L4). Nothing new is needed.
- **Built-in skills:** the finalist is written to `<data>/tuned/<name>/.versions/<n>/SKILL.md`: the embedded front-matter plus the new instructions. A version is written once and never replaced. Next to `.versions/` are `TUNED.json` (status, the current version, and for each version the hash of the embedded `SKILL.md` it was made from and the scores) and `CHANGELOG.md`.
  - When skills load, the **active** current version replaces the embedded one, but only if its base hash matches. The skill keeps `SourceBuiltin`: its trust, precedence and every other file (scripts, references) stay those of the binary.
  - A new noskill version that changes the built-in makes the tuned copy **stale**. It's ignored, and the next optimisation starts from the new built-in.
  - A tuned built-in starts as a **candidate**, and you adopt it (`noskill skills optimise adopt <name>`). This happens whatever `learning.promotion` says, because it changes how a skill that ships with noskill behaves.
- **Reverting:** `noskill skills optimise revert <name>` sets it back to the embedded instructions. The weekly gate does the same on its own when a task that names the skill fails after the tuned version changed (`eval gate --rollback`).

**Running it:** `noskill skills optimise <name> [--budget 4] [--model m] [--dry-run] [--force]` runs it by hand, with live progress. It doesn't run on its own yet. Each candidate costs one run per training task, about 30 agent runs per candidate for `files` on a 4B model. It doesn't run on battery unless `--force` is given.

## Consequences
- Today only `files` (and learned skills that have tests) has enough tasks to optimise. `code-runner`, `research`, `web-search` and the others need checked tasks first. This is said in the report, not hidden.
- Tuned built-ins are plain folders like learned skills. They can be read, diffed against the embedded version (`noskill skills optimise show files`) and moved to the trash. Removing the folder means the built-in is used again.
- The held-out tasks guard against overfitting only as far as they reach: 12 multi-step tasks for `files`. Pinned tasks make this better over time.
- No Python dependency. A real DSPy or GEPA run stays possible outside noskill, since a skill is a plain folder.

## Added in block 5d (after three live runs on `qwen3:4b`)
- **Every search writes a report** to `<data>/optimise/<skill>-<time>.json` with all rewrites, their scores and the task results, dry runs included. The first three live runs were dry runs that printed only scores, so a winning rewrite would have been lost with the process.
- **`--dry-run` is a short trial:** 8 training tasks and 2 rewrites unless `--tasks` and `--budget` say otherwise. `qwen3:4b` can't turn its reasoning off and generated about 1,000 tokens a step (about 3 minutes a task), so a full search is more than 5 hours. Anything that long isn't a trial.
- **`--tasks N`** (at least 5) takes N training tasks spread evenly through the list, and N/3 (at least 2) held-out ones. The same tasks are chosen every time.
- **`--adopt`** puts a built-in's accepted rewrite into use at once, after every check in this ADR has passed. It's for a real run on a built-in skill; a learned skill's rewrite still goes on trial first. `optimise revert` undoes it.
- **`--adopt` isn't allowed with `--tasks`.** On `llama3.2:latest` an 8-task run accepted a rewrite (2 of 8 against 0 of 8, with the bar at 2 more passes), and the full 28 tasks then rejected the best of two rewrites (4 of 28 against 3 of 28, which needs 3 more). A partial list can still save a candidate; it can't put one into use.
