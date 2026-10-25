# Decisions

## 2026-10-25 Housekeeping skills: Go plans, model-free jobs, gated apply tools
**Decision:** Five built-in skills (downloads-tidy, duplicate-finder, disk-watch, media-renamer, daily-brief) keep their
logic in `internal/chores`. Scheduled work that needs no judgement runs as model-free job kinds (`anime`, `remind`, `brief`,
`diskwatch`). The skills that change files expose a read-only `*_plan` tool and one confirm-gated `*_apply` tool that plans
again at apply time; they don't depend on the `files` skill.
**Why:** A model-driven anime-follow run took 13 minutes and wandered where the code takes 10 seconds. In the first live
run of downloads-tidy, granite4:3b ignored the gated `tidy_trash` and used the files skill's ungated `trash`; with only an
apply tool offered, the approval can't be bypassed and the model has one step fewer to get wrong.
**Alternatives considered:** Letting the model pass a plan to `move_many` (more calls, the plan can be altered); a reminder
parser backed by tev1 (deterministic parsing covers the common phrasings and refuses the rest, so no model is needed yet).

Append-only. Newest entry at the top. One entry per non-obvious decision — not every edit.

## 2026-10-24 Adopted spare-project-memory for cross-tool continuity
**Decision:** Added `.agent-memory/` (this directory) as the shared, tool-agnostic memory store
for this project, with a pointer block appended to `AGENTS.md` and a new minimal `CLAUDE.md` that
points here and at `AGENTS.md`.
**Why:** The user switches between Claude Code, Codex CLI, and possibly other agentic tools on
this repo. Each tool's own session memory is private to that tool, so context (what's in
progress, open threads, past decisions) was getting lost on every switch. Plain markdown
committed to the repo is the one format every tool can read without any special support.
**Alternatives considered:** A database or MCP-server-backed memory was ruled out for now — the
whole point is zero-setup compatibility with tools that have no concept of this skill.
