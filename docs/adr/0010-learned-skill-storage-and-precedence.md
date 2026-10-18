# ADR-0010: Learned skills as plain folders, with status in the front-matter and the lowest precedence

Status: **accepted** · Date: 2026-10-15

## Context
The learning loop (LEARNING.md, FR-L2–L5) writes skills on its own. It also changes their status (candidate, active, stale, archived), keeps every version, and tries refinements next to the version in use. DATA_MODEL.md planned `skill_versions` rows and a `skills.status` column as the source of truth. That splits one skill between the database and the disk:
- A user who edits or copies a learned skill by hand would have a folder that disagrees with its rows.
- The plain-`SKILL.md` rule (AGENTS.md) means a learned skill must also load anywhere without noskill's database.

There's a second question: which skill wins when a learned skill has the same name as a built-in or user skill.

## Decision
**The folder is the source of truth.** `<data>/learned/<name>/` is an ordinary agentskills.io skill:
- `noskill.status` and `noskill.version` in its front-matter say where it is in its lifecycle. Status changes rewrite only those keys and keep the rest of the front-matter as it was.
- Every published version is copied to `.versions/<version>/`. `CHANGELOG.md` records why each version or status change happened. Rollback copies an old version back; the newer one stays in `.versions/`.
- A refinement or merge on trial waits in `.next/`, beside the version in use. Adopting it is a copy plus a changelog entry. Rejecting it moves it to `.rejected/`.
- Drafts are built in `.drafts/`. Rejected drafts go to `.rejected/<name>-<stamp>/` with `REJECTED.txt`. Nothing is hard-deleted: replaced files go to the OS trash.
- The database keeps only what's derived or per-run: `run_skills` (which version a run used), `skill_stats` (recomputed from runs), `skill_versions` as an index of the `.versions/` folders, the trial state in `app_state`, and the queue.

**Learned skills have the lowest precedence after project skills:** user 4 > built-in 3 > extra roots 2 > learned 1 > project 0. A learned skill can never shadow a skill a person wrote or one that ships with noskill. Project skills (from a folder you happen to run in) still need approval and rank last.

## Consequences
- `noskill skills versions|diff|rollback` work on folders alone, and a learned skill can be copied out and used elsewhere as is.
- `skills.status` in the database is a cache refreshed when skills load, not the authority. A `skill_versions` row that disagrees with the folder loses.
- The learned skill's name has to differ from a built-in or user skill's name to be selected. The author picks class-level names (LEARNING.md "Lessons from Hermes"), so this hasn't come up.
- Two noskill processes could write the same skill folder at once. Each queue item is claimed atomically (`ClaimLearning`), so two workers never handle the same item. They could still handle two items for the same skill at once; the queue's per-skill de-duplication makes that rare, and this isn't guarded further.
