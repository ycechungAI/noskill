# Learning and self-improvement

"The model learns new skills and improves over time" is the core promise. This doc pins down what that means, because most of the learning happens **around** the model (skills, memory, routing), not inside its weights.

## Four layers of learning

| Layer | What changes | Speed | Risk | Phase |
|---|---|---|---|---|
| 1. **Memory** | Facts and preferences ("anime goes in /Volumes/RED/ANIME2026", "I like short answers") | Instant | Low | P0 |
| 2. **Skills** | New `SKILL.md` + tools created; existing ones refined | Minutes, idle time | Medium (sandboxed + verified) | P1 |
| 3. **Routing** | Which model gets which task | Continuous | Low | P1 |
| 4. **Weights** (opt-in) | LoRA adapter trained on curated traces | Hours | Higher | P2 |

Layers 1–3 work with **any** model, including ones you can't fine-tune. That's what makes the system model-agnostic.

## The loop

```
 run ──▶ trace ──▶ outcome ──▶ mine ──▶ draft ──▶ verify ──▶ trial ──▶ promote
                      │                                         │
                      └────────── failures ──▶ refine ──────────┘
                                            curator (weekly): merge · retire · report
                                            eval (weekly): measure · auto-revert
```

## Traces

Everything starts from traces, so their format is part of the spec. One JSONL file per run (`traces/YYYY/MM/DD/<run-id>.jsonl`), indexed in SQLite. Secrets are redacted before writing.

| Event | Key fields |
|---|---|
| `run_start` | `run_id`, `parent_run_id`, `source` (cli/proxy/job/tray), `request`, `driver_model`, `skills_shown`, `no_learn` |
| `decision` | `name` (e.g. `router`, `same_series`), `model`, `answers` with probabilities, `ms`, `emulated` |
| `llm_call` | `model`, `tokens_in`, `tokens_out`, `ms`, `tool_strategy`, `thinking` |
| `tool_call` | `skill`, `tool`, `args`, `result_digest` (hash + first 500 chars), `ms`, `error`, `permission_checks` |
| `approval` | `id`, `what`, `answer`, `by` (cli/tray/notification/timeout) |
| `delegate` | `child_run_id`, `task_type`, `model` |
| `file_effect` | `op` (create/move/trash), `from`, `to`, `bytes`, `hash`: used for undo and tests |
| `outcome` | `value`, `confidence`, `source` (explicit/check/implicit/decision/llm_judge) |
| `run_end` | `status`, `steps`, `tokens_total`, `ms_total`, `answer_digest` |

## 1. Outcome (FR-L1)
Every trace gets `outcome ∈ {success, partial, fail, unknown}` and a confidence, from these sources in order:
1. **Explicit**: user good/bad (CLI `noskill rate`, `/rate` in chat, proxy feedback endpoint, tray).
2. **Checks**: skill-defined post-conditions (file exists, hash matches, HTTP 200, schema valid).
3. **Implicit**: user re-asked the same thing within 10 minutes, or aborted.
4. **Decision judge**: the resident JEV model answers `answered` (noul), `quality` (score) and `followed_skill` (noul) after the reply is sent, so its few hundred ms are off the user's wait. Its probabilities become the confidence.
5. **LLM judge**: rubric judge on the strongest local model, at idle, only when the decision judge is unsure (confidence < 0.7) or no decision model is installed.

Only `success` with confidence ≥ 0.7 feeds synthesis. Failures feed refinement.

**Judges need an independent check.** A model judging its own kind of output mostly agrees with itself: in Phase 0, a same-model relabel matched its draft 200/200 ([protocol](PHASE0_TESTS.md#assisted-labelling)). So:
- The LLM judge uses a different model family from the one that ran the task, when one is installed.
- When the decision judge and the LLM judge disagree, the run goes to the approvals queue as a quick "was this right?" question. At most 3 a day, shown in the tray and `noskill approvals`, never as a blocking prompt.
- About 1 in 50 judged runs is also offered for your rating at random, even when the judges were sure. That's how judge accuracy is measured (most real disagreements are on items the AI was sure about), and it's shown in `noskill stats`.

## 2. Mine (FR-L2)
Triggers, checked after each run and again by the nightly learning job:
- success with ≥ 5 tool calls and no skill covering it (best skill-description similarity < 0.75), or
- the same tool-call pattern (sequence of tool names, ignoring args) seen ≥ 3 times in 30 days, or
- the user says "remember how to do this", `/learn`, or `noskill learn <run-id>`.

Mined runs go into a queue. Nothing is drafted while you're using the machine (see [Resources](#resources-on-a-16-gb-machine)).

## 3. Draft: turning a trace into a skill
The [skill-author](../skills/skill-author/SKILL.md) skill does this in **small passes**, each with constrained output. That works much better on 4B–8B models than one big "write a skill" prompt.

| Pass | Input | Output |
|---|---|---|
| a. Compress | Trace | Ordered action list: `tool(args) → result summary` |
| b. Parameterise | Action list + original request | Each constant (path, name, URL, number) marked **input** or **fixed**. Constants that appear in the request are inputs; the rest go to `decide("is_parameter")`, and low-confidence ones become inputs with the trace value as default |
| c. Steps | Parameterised actions | `SKILL.md` "Steps" section: short, numbered, imperative |
| d. Script? | Trace + request | If the run changed files, a Python script tool that does the whole chore in one call; the steps then say "call it once". See [Pass d](#pass-d-script-tools) |
| e. Tests | Trace + inputs | `tests/cases.yaml`: replay of the original request (same tool sequence and end state) + 1–2 variations with different inputs, using recorded fixtures |
| f. Describe | All of the above | `name`, `description`, `triggers`, `permissions` = exactly those used in the trace |

**Retrieval check:** the original request must retrieve the new skill in the top 3, and 5 unrelated past requests must not. Otherwise the description is rewritten (max 2 tries). The check uses the same selector, and so the same learned-skill floor, as a real run (see [Retrieval](#retrieval-of-learned-skills)).

## 4. Verify (FR-L3)
- Run tests in the sandbox (WASM, or a subprocess under the permission jail; see [SECURITY.md](SECURITY.md)). File tests run in a temp copy of the relevant folders, never the real ones.
- Static checks: permissions requested ⊆ permissions used in the source trace; no new domains or paths; no hard-coded secrets; scripts pass a lint for the language.
- **Permission escalation always needs a human**, even if tests pass.
- Pass → `candidate`. Fail → up to 2 self-repair attempts with the test output → else discarded to `learned/.rejected/` for inspection.

## 5. Trial and promote
Decided default (Q6): ask first, then automatic.
- The first time a candidate matches a request, noskill asks through the [approvals queue](ARCHITECTURE.md#approvals): "noskill learned a way to do this: *Organise Downloads folder*. Use it?" (CLI, tray or notification).
- Approve → used for this run. After **3 successful uses with no failures** it becomes `active` and is used automatically.
- Skip → the request runs the normal way; after 2 skips the candidate is archived.
- Alternative modes (config): `shadow` (run both when cheap, compare) and `auto` (use right away, roll back on failure).

## 6. Refine (FR-L4)
When an active skill's rolling success rate (last 10 runs) drops below 70%, or on 2 consecutive failures:
- collect failing traces → skill-author proposes a **diff** (instructions and/or script) → verify on **old passing tests + new failing case** → new version as candidate → trial as above (refinements to a skill you already approved don't ask again unless permissions change).
- Every version is kept in `.versions/` with a changelog; `noskill skills diff` / `rollback`.
- Revised steps may not write in a value from a failed run's request (a path, a file name, a prefix) that the current steps don't already have; the revision is refused instead. In block 2d a refinement named one run's folder in its step, which would send the next run there.

## 7. Curator (FR-L5)
Weekly, idle-only:
- **Merge** near-duplicate learned skills (same family, or `decide("duplicate")` ≥ 0.85, with overlapping tools). Merged skill keeps both sets of tests.
- **Retire**: unused 30 days → `stale` (hidden from the selector), 90 days → `archived`.
- **Report** (notification + status page): "This week: 2 skills learned, 1 refined (success 61% → 88%), 3 retired, eval +4 pts."

## As built in block 2a
What the code does today, where it's more specific than the sections above.

**Traces (v1).** The first line is `run_start` with `v: 1`, the full request, `source`, `parent`, the driver model, the task type and the selected skills. Then `route`, `select`, `step`, `tool_call` / `tool_result` / `tool_denied` (each with the owning `skill`), `progress`, `budget`, `retry`, `approval`, `answer`, `file_effect` (`op`, `from`, `to`, from the undo log) and `end`. `outcome` lines are appended later; the last one wins. Older traces without `run_start` read as v0, with the request from the `route` event (cut at 400 characters). Streaming deltas aren't written. `llm_call` and `decision` events aren't written yet.

**Outcome.** Tiers 1–4 as above. The LLM judge (tier 5) runs in the learning worker at idle on runs that finished more than 10 minutes ago (so you can rate first), within the last 3 days, with no outcome or only a decision-judge outcome under 0.7. It judges at most 3 per pass, on the largest chat model that fits the memory budget, preferring another family than the run's model (`qwen3:4b` and `qwen2.5-coder:7b` are both `qwen`). An emulated judge gives one label without probabilities, so its verdict alone has confidence 0.6. That's enough for routing (≥ 0.6) but not for synthesis (≥ 0.7). When it agrees with the decision judge the confidence becomes max(both, 0.75), and the run is mined again. When they disagree, or for about 1 in 50 judged runs, a "Was this answer right?" approval (kind `outcome_check`, 7-day timeout, at most 3 a day) is queued. Approve means it was right (rated good); skip means it wasn't (rated bad); expiry changes nothing.

**Queue.** `learning_queue` items have a kind: `draft` (from the miner or `noskill learn`), `refine` (an active learned skill that keeps failing, with up to 2 failing run ids) or `merge` (from the curator, with `skill` = the keeper and `pattern` = the duplicate). Caps per 24 h: `max_drafts_per_night`, `max_refines_per_night`. Merges are uncapped (the curator queues at most 3 a week). A refine for the same skill isn't queued again within 3 days. `noskill learn queue` lists the items and says whether the worker can run now.

**Library.** Learned skills live in `<data>/learned`, loaded with precedence user 4 > built-in 3 > extra 2 > learned 1 > project 0:
- `<name>/`: the current version; status (`candidate`, `active`, `stale`, `archived`) is in its front-matter.
- `<name>/.versions/<version>/`: every published version.
- `<name>/CHANGELOG.md`: one entry per publish, status change, adoption or rollback.
- `<name>/.next/`: a refinement or merge on trial, with a `.note`.
- `.drafts/<name>/`: work in progress.
- `.rejected/<name>-<stamp>/`: rejected drafts, with `REJECTED.txt` saying why. A rejected `.next` is named `<name>-next-<stamp>`.

**Drafting on small models.** The passes are constrained JSON calls with thinking off. When the steps pass doesn't name the tools the run used, the steps are written from the trace instead. Multi-line arguments, such as `run_python`'s program, become "(a short program written for the request …)" there, so a step never carries the source run's data. On 3–4B models, a run that is one `run_python` call almost always takes this path: the models describe the inside of the program rather than the assistant's steps (seen with `qwen3.5:4b`, `qwen3:4b` and `llama3.2:3b`). When the request has no changing values, the variations pass asks only for reworded requests. A `values` object with no properties lets a small model pad it with whitespace until its output runs out.

**Tests.** `tests/cases.yaml` as in [SKILL_FORMAT.md](SKILL_FORMAT.md#tests-testscasesyaml), plus `required: true` on a case. The gate passes when the first case (the replay of the source run) passes, every required case passes, and at least half of the others pass. Small models vary from run to run, and the trial catches the rest. Replays answer tool calls from the recorded fixtures (matched by tool name, in order), so no test touches real files or the network.

**Trial (§5).** A candidate joins skill selection. In `ask` mode, the first request it matches asks:
- At the terminal, when someone is there to answer.
- Otherwise through the approvals queue (kind `learned_skill`, 7-day timeout). That run carries on without the skill.

`shadow` mode behaves like `ask` for now. `auto` mode uses the candidate right away. Trial state is kept in `app_state` as `learning.trial.<name>`.
- A candidate becomes `active` after `promote_after` (3) good runs in a row: success with confidence ≥ 0.7.
- It's archived after 2 skips or 2 failures (1 failure in `auto` mode). A single failure in `ask` mode means it asks again next time.
- Naming the skill with `--skill` counts as approval.

**Refine (§6).** Only learned skills are refined. The triggers are 2 failures in a row, or under 70% success over the last 10 rated runs once at least 5 runs are rated. Steps:
1. The current version is copied to a draft.
2. Each failing run becomes a required case, `failed-<run id>`, with fixtures from its trace.
3. The author model rewrites the Steps section (at most 2 repairs).
4. File checks (load, size, secrets) and an access check run: new access is reported, not refused.
5. The tests replay. A pass is published to `.next`.

While a `.next` exists, it's used instead of the current version. It asks first only if it needs more access than the current version, and 2 skips reject it. It's adopted after `promote_after` good runs in a row and rejected after 2 failures.

**Curator (§7).** The curator runs weekly in the learning worker (`learning.curated_at`):
- Learned skills unused for 30 days become `stale`, and after 90 days `archived`. `noskill skills enable <name>` brings one back.
- Pairs whose replay fixtures use overlapping tools (Jaccard ≥ 0.5) are queued as merges, closest source requests first (at most 3 a week). The keeper is the one with a script tool, then more successes, then an active one. A pair of the same [family](#near-duplicates) merges without asking; any other pair needs `decide("duplicate")` ≥ 0.85. The merge then copies the duplicate's tests into the keeper (the duplicate's first case becomes required, and its fixtures are prefixed with its name) and adds the duplicate's triggers. Fixtures and expected tools that name the duplicate's own script tool are renamed to the keeper's. Once verified, the merge is published as the keeper's `.next`, and the duplicate is archived.
- The report goes to your notification channels: "This week: N skill(s) learned, N refined, N merged, N retired."

<a id="pass-d-script-tools"></a>**Pass d: script tools (block 2d, ADR-0013).** A file chore done the long way takes about 5 model turns (list, make the folder, dry run, apply, answer). Pass d turns it into a Python tool that does the chore in one call, so the learned skill takes 2 turns. It runs only where the script sandbox enforces file access (macOS, ADR-0008); elsewhere the draft keeps plain steps with a note.
1. The run's folder is rebuilt from its `list_dir` and `read_text` results in noskill's own temp folder, and the run's file effects (moves, copies, writes, trashes) become the expected ops.
2. The author model writes the tool: a name, up to 4 string arguments with their values in this request, and code that reads `{"folder": …, <args>}` on stdin and prints `{"ops": [...]}` ([SKILL_FORMAT.md](SKILL_FORMAT.md#file-ops-from-a-script)). This is the one pass in plain text, not JSON: `tool_name:`, `description:` and `arg <name>: <value> | <what it is>` lines, then the code in a ```` ```python ```` block. Under a JSON schema, `qwen3.5:4b` wrote code inside the `"code"` string until its tokens ran out, every time. Its repair replies often run the header keys together on one line, so the parser finds them anywhere.
3. Checks:
   - An argument whose value isn't in the request (a result such as a count) isn't an argument, and one the code never reads is dropped. The code may read an undeclared argument only with `args.get(name, default)`.
   - The code may not change files itself (`open(…, "w")`, `shutil`, `os.remove`, …): the folder is read-only, so it asks with ops.
   - It holds none of this run's file names or request values (names with dots, digits or a trailing dash, quoted words). A default in `args.get(name, default)` for a declared argument doesn't count, since the call always passes a value. Small models keep writing request values in after being told not to, so a quoted literal that is exactly a request value is rewritten to `args["…"]` in Go, adding the argument (`ext`, `prefix`, `name`) when the reply had none.
   - Run in the sandbox on the rebuilt folder, it prints exactly the expected ops (texts compared line by line).
   
   A failure goes back to the model with the problem, at most 4 tries.
4. On success the steps become "call `<tool>` once … answer from its result", the replay case expects that one call, and the tool's result on the rebuilt folder is its fixture. The tool asks first (`confirm:`) when a tool the run used asked first (`move_many`).
5. If the replay fails, the first repair adds "call it first, no listing, no dry run"; the second drops the tool and goes back to the plain steps, with a note giving the failed test's reason and calls. A redraft that was only there to add a script tool (see [Near-duplicates](#near-duplicates)) is then rejected as "covered".

The static check allows a learned skill's own tool only when it's one pass-d tool: `runtime: python`, no packages. It runs like any script tool, with no network, and its ops go through the broker: permission checks, no overwrites, the OS trash, the undo log.

<a id="near-duplicates"></a>**Near-duplicates.** Two learned skills are one family when their replay fixtures share tools (Jaccard ≥ 0.5) and their source requests share task words (≥ 0.45). Task words are the request's words without values (paths, names with dots or digits, `prefix-` words, quoted words, the word after "named"/"called"), stemmed. The score is the mean of the Jaccard of the words and of the word pairs in order, scaled down below 4 words. On the workload's six families, every pair inside a family scores ≥ 0.45 and every pair across families ≤ 0.35 (`TestFamiliesOfTheWorkload`). Before drafting, a run whose request belongs to an active or candidate learned skill's family is skipped as "covered by X", except when X has plain steps and pass d can run. Then the new draft must come with a script tool; at most 2 such redrafts per skill (`learning.upgrade.<name>`):
- An active X gets the draft as its `.next`, on trial in its place.
- A candidate X is archived, with a changelog line pointing at the new skill.

<a id="retrieval-of-learned-skills"></a>**Retrieval of learned skills.** There's often no embedding model, so learned skills are found by keyword. In block 2c that went wrong both ways: "combine every .txt into all.txt" picked filter-error-lines at 0.21, and a new archive prefix found nothing. Now:
- The description ends with ` Example request: "…"`, the source request with its values replaced by "…".
- The request's paths are dropped before scoring. A temp folder's segments weighed as much as the task words.
- A skill with an example request scores the geometric mean of two shares: the request's words found in the skill, and the example's words found in the request. A short request ("How many files are in X?") no longer matches every skill that mentions files.
- Learned skills need 0.6 by keyword (`Selector.MinLearned`, a code default), or a trigger phrase hit. On the 2c skills, true matches scored 0.73–1.0 and other tasks ≤ 0.48. The exception is "copy a.txt to b.txt and keep the original" against a backup skill, at 0.88.

**Not built yet:**
- Per-skill accuracy stats for the judges.
- Pass d for runs that only read (answers computed from files) and for other OSes' sandboxes.

## As built: `noskill eval` (block 2b)
**Tasks.** 73 built-in tasks in `internal/eval/tasks/*.yaml`, each with a deterministic check, so no model grades another:
- 50 plain tasks (one model call, no tools): chat 6, reasoning 6, math 8, code 8, summarise 5, extract 6, translate 5, creative 3, plan 3. Twelve of them are marked `bench` and are what `models bench` runs.
- 11 agent tasks (the whole loop with real tools against a temporary folder): 10 for `files` (read, list, write, move, copy, make a folder, filter lines) and one for `code-runner`. They check the answer, that one of the expected tools was called, and the files left behind.
- 12 multi-step tasks (type `multi`, block 2c), two in each of six chores a user repeats: sort by type, pull errors out of a log, archive by prefix, rename with a prefix, back up by extension, combine text files. Each has a `family`; `internal/eval/workload/` holds three training requests per family for `eval simulate`, with other names and contents.

Agent tasks name the skills they need, and the best-matching active learned skill joins them, as in normal use (block 2c; before that the suite never used learned skills, so it couldn't measure them). Confirm actions such as `move_many` are approved, since everything happens in the throwaway folder. `--no-learned` leaves learned skills out. Each result records the learned skills it used.

Checks: `contains`, `any`, `not`, `regex`, `not_regex`, `number` (the last number in the answer), `json` (an object in the answer with these fields; strings match as substrings, arrays need every element), `max_words`, `lines`, `answered`. Web tasks aren't in the suite (no network, nothing to record).

**Runs.** Each task goes through the normal run path on a pinned model with no trace, memory, bandit or outcome signal (`source: eval`, not counted as you using noskill). Results are stored per run (`eval_runs`, `eval_results`), together with the version of every learned skill at the time. `eval run` compares with the last run of the same model and suite.

**Pinned tasks.** `eval pin <run-id>` saves the request, the skills it used and the tools it called, with the check "the answer isn't empty" plus any `--contains` text, in `<data>/eval/pinned.yaml`. A pinned task runs with no access to your files, so it only makes sense for requests that don't need any (answers, `code-runner`). Editing the file is how you tighten a check.

**Regression gate.** Per task type (3 or more paired tasks), a fall of more than 25 points, or two flipped tasks, is a regression. So is a fall overall of `regression_success_drop` points (5). The weekly run (`eval_schedule`, run by the learning worker at idle on the model you used most in the last 30 days) and `eval gate --rollback` do the same thing on a regression: each learned skill whose version differs from the last run *and* that took part (before or now) in a task that went from pass to fail goes back to that version (`skills rollback`, so the newer one is kept), and a notification says what fell and what went back. Skills that didn't exist at the last run stay and are reported. A fall in tasks no learned skill touched, such as plain questions, rolls nothing back (block 2c; before that, any fall rolled back every changed skill).

Not built: median steps rising 20% as a regression signal, the routing-priors snapshot in the revert, routing regret, the 1-eval-a-night cap, research and multi-step tasks, and the weekly report on the status page beyond the list of runs.

## Lessons from Hermes Agent (S5)
Measured in [S5](spikes/S5-results.md); what we copy and what we do differently:
- **Copy:** the review prompt's order of preference (update a loaded skill → an umbrella → a support file → only then a new class-level skill), class-level names instead of session artifacts, **read-before-write** enforced by the tool, a review that runs after the answer is delivered, and a curator that marks stale and archives but never deletes.
- **Do differently:** Hermes saves a skill straight from one conversation, and its "be ACTIVE" prompt pushes toward writing. Its review replays the whole conversation (~30k tokens by its own estimate), which is minutes on a 4B model. In S5 a 4B model's skill referenced a script that doesn't exist. noskill keeps the verification gate and trial period below, and mines the short trace, not the full transcript.
- **Add:** a **file-effect check** on every answer: compare what the model says it changed with the recorded file effects and warn on a mismatch (Hermes's "file-mutation verifier" caught two failed writes this way).

## Worked example: learning "organise Downloads"

1. **Run.** In `noskill chat` you ask: *"Sort my Downloads: PDFs to Documents/PDFs, images to Pictures/From Downloads, installers older than 30 days to the trash."* The driver model (`qwen3:8b`) uses the **files** skill: `list_dir` → builds a plan → `move_many` dry run → you confirm → `move_many` → `trash` ×4. That's 9 tool calls.
2. **Outcome.** You type `/rate good`. Checks pass (every `file_effect` target exists). Outcome: success, confidence 1.0.
3. **Mine.** 9 tool calls, and the closest skill (`files`, 0.62) is below 0.75 → queued.
4. **Draft** (that night, idle, on AC):
   - Compress: `list_dir($DOWNLOADS)`, `move_many([...pdf → $DOCUMENTS/PDFs, ...])`, `trash([...dmg, ...pkg])`.
   - Parameterise: `$DOWNLOADS`, target folders, `30 days` and the extension groups appear in the request → inputs with defaults. On Windows the installer group becomes `.exe/.msi`, on Linux `.deb/.rpm/.AppImage`, because the draft uses the per-OS extension table from the files skill.
   - Script: every step is deterministic → `organise.py` tool (runs in under a second, no model needed).
   - Tests: replay on a temp copy of a fixture folder with 20 files → expected end state; variation with `older_than = 7 days`.
   - Describe: `name: organise-downloads`, permissions `read:$DOWNLOADS`, `write:$DOCUMENTS/PDFs`, `write:$PICTURES/From Downloads`, `write:$DOWNLOADS` (trash only).
5. **Verify.** Tests pass in the sandbox; permissions equal the trace's → `candidate`.
6. **Trial.** A week later: *"clean up my downloads again"* → the candidate is retrieved → approvals: "Use learned skill *organise-downloads*?" → approve → done in 1 tool call instead of 9, ~2 s instead of ~40 s.
7. **Promote.** After 3 good runs it's `active`. You can also schedule it: `noskill schedule add organise-downloads --every 7d`.
8. **Refine.** A `.zip` the rules don't cover gets left behind twice → you rate it bad → refine adds an "archives" group with a test → new version, same permissions, so no new approval.

## Routing learning (FR-L7)
See [ORCHESTRATOR.md](ORCHESTRATOR.md#learned-routing). Each finished trace updates the bandit for (task type, difficulty, model).

## Measuring "improves over time"

`noskill eval` runs a held-out suite weekly (idle, ~20–40 min on a 16 GB machine) and on demand.

**Suite**
- **Built-in tasks** (~60): 10 each of research, files, extract, code, chat/reasoning, multi-step automation. Each has an automatic checker (expected files/state, required citations, exact answers, unit tests for code). Web tasks replay recorded fixtures, so results are comparable week to week.
- **Pinned tasks**: your own runs, added with `noskill eval pin <run-id>` (the end state or answer becomes the expectation).
- Learned skills' own tests are **not** part of the suite, to avoid grading the work against itself.
- A task that names the tools it needs (`tools:`, e.g. `copy_verify`) also passes when a learned script tool made the same kind of change through its ops (`copy`, `move`, `create`, `mkdir`, `trash` in the undo log). The broker applies those ops with the same checks, and the task's end-state check still inspects the folder. Before block 2d, a run that used a learned backup tool failed with "didn't call copy_verify", and the simulated user rated it bad. The learned tools include a version on trial in `<name>/.next`: a script redraft brings a tool the current version lacks, and before that fix every script redraft in the 2d exit check was rated bad and set aside after 2 runs that had done the task.

**Metrics (per week, per task type)**

| Metric | Definition |
|---|---|
| Success rate | Checker passed / tasks |
| Steps | Median tool calls per successful task |
| Tokens | Median total tokens per successful task |
| Latency | Median wall-clock per task |
| Swaps | Model loads per task (from orchestrator) |
| Learned share | % of tasks that used a learned skill |
| Routing regret | Success lost vs. the best model in hindsight on the same task |

**Regression gate:** if success drops ≥ 5 points, or median steps rise ≥ 20%, versus last week, noskill reverts everything learned since the last good eval (learned-skill versions + routing priors snapshot) and reports what it reverted.

## Resources on a 16 GB machine
- Learning work (draft, verify, refine, curator, eval, LLM judge) runs only when: no request for 10 minutes, on AC power, and no job running. Pre-empted immediately when you send a request (the current LLM call is cancelled and resumed later).
- Uses the model that's already loaded when it's good enough; at most one model swap per learning session.
- Caps per night (configurable): 3 drafts, 3 refinements, 1 eval.

```toml
[learning]
enabled = true
promote_mode = "ask"          # ask | shadow | auto
promote_after = 3
min_tool_calls = 5
novelty_threshold = 0.75
idle_minutes = 10
require_ac = true
max_drafts_per_night = 3
max_refines_per_night = 3
author_model = "auto"         # auto = best local general model; scripts use the code model
```

## Weight learning (FR-L8, opt-in)
Decided in [ADR-0015](adr/0015-lora-pipeline.md).

### As built (block 5b)
- **Export:** `noskill train export [--task t] [--since 30d] [--base m]` writes `<data>/train/datasets/<id>/`: `train.jsonl`, `valid.jsonl` (about 1 run in 10, by run ID), `REVIEW.md` and `MANIFEST.json`.
  - **Kept:** runs from real use (`cli`, `proxy`, `tray`, `job`, `delegate`) that succeeded. The success must be your rating, a check, or a judge with confidence ≥ 0.8.
  - **Left out:**
    - eval, learning and optimise runs (the benchmark uses those tasks);
    - runs with a failed or refused call, a stop at a budget, or no answer;
    - repeats: the same task words and the same calls.
  - **Format:** mlx-lm's chat format with `tools`, one example per assistant turn, so that `--mask-prompt` trains every call and the answer. The system message is rebuilt from the base prompt and today's skills. Memory and the file-access note are left out. Secrets and the home folder (`~`) are replaced.
  - **Review:** `REVIEW.md` shows each run as you'd read it. Delete a run's lines from the `.jsonl` files to leave it out.
- **Train:** `noskill train lora --data <id> --base <model> --reviewed [--iters n] [--dry-run]` runs `mlx_lm lora` (4-bit base: QLoRA), then `mlx_lm fuse --dequantize`, then `ollama create <base>-ns<n> -q q4_K_M` with the base's own Modelfile. The steps go through `uv run --isolated --with mlx-lm==<learning.lora.mlx_lm>`.
- **Refused when:**
  - the dataset has fewer than `learning.lora.min_runs` runs (default 200);
  - `--reviewed` is missing;
  - it's on battery, or not Apple Silicon (CUDA isn't built);
  - under 8 GB is free;
  - the base has no MLX build in `learning.lora.mlx_models`.
- **Benchmark:** the new model and its base run the dataset's task type's checked agent tasks (all of them when it has none). It must be clearly better under ADR-0014's rule. If it is, it waits as a `candidate`. If not, it's `rejected` and removed from Ollama; the adapter stays.
- **Routing:** a model `train lora` made is never routed to unless it's adopted. `noskill train adopt <n>` makes it stand in for its base on its task type, and the route explanation says so. `train revert <n>` stops it, and `train list` shows the datasets and runs.
- **The first check on this machine's traces (2026-10-18):** none qualified. Ten job runs were judged successes at only 0.60–0.75 confidence. With the bar lowered in a scratch copy, one of the three runs left was an answer the model made up ("List of files in ~/Downloads: …") that the judge had passed. So the confidence bar and the review stay.

## Prompt optimisation (FR-L9)
Offline GEPA-style search over a skill's instructions, scored by the checked tasks that name it. Decided in [ADR-0014](adr/0014-skill-optimisation.md).

### As built (block 5a)
`noskill skills optimise <name> [--budget 4] [--model m] [--dry-run] [--force]` runs it by hand, with a ✓/✗ line per task. It doesn't run on its own.

- **Which skills:** built-in and learned ones. User and installed skills are yours, so they're left alone.
- **Tasks:** training tasks are the agent and workload tasks that name exactly this skill. Held out are the multi-step tasks and your pinned tasks. A skill needs 5 training tasks; today only `files` has enough (about 29 training, 12 held out).
- **Loop** (`internal/learning/optimise.go`):
  1. Score the current instructions.
  2. Up to `--budget` times: pick a parent from the Pareto front. The author model (`learning.author_model`, local) sees up to three of its failed tasks, or its slowest ones when none failed, and replies with up to 3 line changes to the instructions (heading, line replaced or none, new line). Go applies them; the protected lines are shown marked `(fixed)` and never replaced. A clause added to one becomes its own line. Whole rewrites in one JSON string looped on qwen3:4b (ADR-0014).
  3. Drop a rewrite that breaks a guard: a `never`/`always`/`don't`/`only`/`must` line changed, more than 25% or 8 lines longer, a task's file name or value added (whole words), front-matter touched, or it doesn't load. Changes that break a guard first get one more try with the reason; 4B models often copy a folder name from the failed requests. Also drop one already tried.
  4. Score every survivor on the training tasks.
- **Accepting:** the best rewrite needs `max(2, 10%)` more training passes, or the same passes with 15% fewer steps. On the held-out tasks it must pass no fewer than the original. A learned skill's rewrite must also pass its `tests/`.
- **Where it goes:**
  - A learned skill's rewrite goes to `.next/` and takes the usual trial (FR-L4).
  - A built-in's goes to `<data>/tuned/<name>/.versions/<n>/SKILL.md` as a **candidate**. `skills optimise adopt <name>` puts it in use. `skills optimise revert <name> [--to N]` goes back to the embedded instructions, or to version N. `skills optimise show <name>` lists the versions and the diff against the embedded file.
  - An adopted version keeps the built-in's front-matter, files and trust. It is ignored once a noskill update changes the embedded `SKILL.md` (stale). `skills list` shows it as `builtin (tuned N)`.
- **Gate:** each eval run records which tuned version every built-in used. `eval gate --rollback` puts a tuned built-in back to the good run's version when a task that names it passed then and fails now.
- **Power:** it doesn't run on battery unless `--force` is given.

## Guardrails
- Learned skills never get more permissions than the trace they came from without a human.
- No learning from runs marked `--no-learn` / incognito.
- Learning never runs on battery or while you're using the model.
- Everything learned is plain files you can read, edit or delete.

## Seed skill: STORM research (block 5e)
The `storm` built-in ([skills/storm](../skills/storm/SKILL.md)) is a research skill the learning loop starts from, not one it learns. It is the user's five-viewpoint variant of Stanford's STORM (Shao et al., NAACL 2024): practitioner, academic, skeptic, economist and historian run one at a time; each asks its own questions, searches, fetches pages and answers from the fetched pages only; later viewpoints first check a doubtful claim from the earlier notes; the skeptic looks for second sources and counter-claims; the notes are merged into an outline and numbered sources, and a report is written section by section from the notes only.
- **The loop is Go, not instructions.** The first version gave the steps to the model. On `granite4:3b` it made six calls (a plan, one search, three fetches) and wrote one answer: no viewpoints, notes or report. A second version let the model call search and fetch in a run of its own: it searched once, never fetched, and 11 of 12 questions came back unsupported. So `storm_run` (internal/app/storm.go) drives everything and the model gets one small job at a time: plan questions, write a search query, answer from three pages Go fetched, an outline, one section, a summary.
- **Sources are only pages that were fetched.** Go searches (the web-search skill's own tools and settings), reads up to three pages, one per site, skips encyclopedias, and the model's `[n]` page numbers become the pages' addresses; a number that isn't a page is dropped. A question no page supports is recorded as "no reliable source found", and the report says how many.
- **Results from a live check** (`granite4:3b`, `questions` 1, the real web): all five lanes, 7 sections, 14 sources (papers, vendor and news pages), a report folder with the notes of every viewpoint. Searches failed in bursts (the DuckDuckGo provider times out and is then skipped for a minute), which cost questions; a failed search is now tried again after 65 seconds (the provider's cool-down), searches are paced a second apart, near-identical questions from different viewpoints are skipped, and when 4 questions in a row lose their search the research stops: with nothing answered that is an error that says to try later or set `BRAVE_API_KEY` or `searxng_url`; with something answered the report is written from it. A third live run (7 minutes) hit a DuckDuckGo outage and answered only a few questions, so it had 4 sources; the second, with search working, had 14.
- **Why it isn't learned:** the work happens inside one tool call, so a trace has no procedure to learn, and a draft is limited to 60 lines. It stays built in, like the others.
- **What the loop still gets:** every run's trace, outcome and rating, so the routing prior for `research` tasks and the model choice improve with use.
- **Checked tasks:** none name it yet, so `skills optimise storm` has nothing to score on. Pin a good run (`eval pin`) to start one.
- **Cost:** about 12 to 30 searches and as many reading jobs, 10 to 25 minutes on a 3B model with `questions` at 1 to 3. The tool's time limit is 90 minutes.
