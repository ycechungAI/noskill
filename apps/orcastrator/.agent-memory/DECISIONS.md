# Decisions

Append-only. Newest entry at the top. One entry per non-obvious decision — not every edit.

<!--
## [YYYY-MM-DD] [Short title]
**Decision:** [What was decided]
**Why:** [The reasoning, constraint, or tradeoff that drove it]
**Alternatives considered:** [Optional — what else was on the table and why it lost]
-->

## 2026-10-27 — start.sh never uses Homebrew Python
Homebrew Pythons live under a versioned Cellar path that `brew upgrade` removes (this orphaned the
old venv: 3.15.0 → 3.15.2), and on this machine the Homebrew prefix is owned by another user and
hangs. `start.sh` probes non-Homebrew Pythons (PATH, python.org, pyenv) with a timeout, falls back
to installing uv into `~/.local/bin` and fetching a standalone Python 3.12, and rebuilds any venv
that is broken or Homebrew-based.
