# Decisions

Append-only. Newest entry at the top. One entry per non-obvious decision — not every edit.

<!--
## [YYYY-MM-DD] [Short title]
**Decision:** [What was decided]
**Why:** [The reasoning, constraint, or tradeoff that drove it]
**Alternatives considered:** [Optional — what else was on the table and why it lost]
-->

## [2026-10-25] Agent monitor lives in sched, not as its own project
**Decision:** The "local agent monitor" (status page + self-heal for Ollama, the sched daemon,
noskill, ...) is `schedlm/monitor.py`, run by two LaunchAgents that `./start.sh` installs next to
the daemon's. The status page is a stdlib `http.server` on its own port (8790), not part of the
FastAPI web UI.
**Why:** The user asked for it in ~/dev/scheduler. Reusing start.sh gives the agents the same venv
self-repair at every login. A separate always-on server keeps the monitor independent of
`./web.sh`, which is started by hand and would clash on port 8787.
**Alternatives considered:** a standalone ~/dev/local-agent-monitor project (rejected by the user);
serving the monitor from `sched serve` (port clash with ./web.sh, heavier always-on process).

