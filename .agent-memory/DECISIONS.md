# Decisions

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
