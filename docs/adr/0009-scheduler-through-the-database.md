# ADR-0009: Scheduler coordination through the database, and run continuations

Status: **accepted** · Date: 2026-10-15

## Context
FR-J1–J4 need jobs that run while no terminal is open, survive a reboot, and stop on a `confirm` call until the user answers (FR-A6, SECURITY.md: scheduled jobs never run `confirm` actions unattended). ARCHITECTURE.md planned for the daemon to be the only executor, with the CLI talking to it over a local socket (Unix socket / Windows named pipe).

Two things made that plan costly for Phase 1:
- An IPC protocol means a second API next to `/noskill/v1`, a socket path and permissions on each OS, and a fallback for when the daemon isn't running. The CLI would still have to edit jobs then.
- A run that stopped on an approval could only be "resumed" by running the whole request again with that one call pre-approved (the block 1b chat behaviour). On a small model a second run often takes a different path, repeats work already done, or asks for a different approval.

## Decision
**Jobs live only in SQLite, and the database is the channel.**
- The CLI (`schedule`, `jobs`, `approvals`) writes rows and returns. It never runs jobs.
- Whichever process runs `serve` (the daemon does) runs the scheduler, ticking every 5 s.
- A row in `leases` (holder host/pid/id, TTL max(3 × tick, 30 s)) makes sure only one process schedules at a time, so a foreground `serve` next to the daemon doesn't run jobs twice.
- Due jobs are claimed with a compare-and-set on `next_run_at` / `retry_at`. `jobs stop` sets a run's status to `stopping`, and the scheduler cancels it on its next tick.
- When a process takes over a lease that expired (daemon crash, reboot), runs still marked `running` by the old holder become `interrupted` and are retried without using up a retry.
- All transactions are `BEGIN IMMEDIATE` (`_txlock=immediate`), so two processes wait on `busy_timeout` instead of failing with `SQLITE_BUSY`.

**A parked run keeps its state (a continuation).**
- When a run stops on an approval, the engine stores the conversation, the pending tool calls, the pinned model and skills, the tool strategy, the step count and the run's flags as JSON in `approvals.resume_token` (at most 4 MB).
- After approval, the continuation is claimed once (a compare-and-set on `details.used`). The pending call runs first without asking the model again, then the loop carries on.
- The parked run's status becomes `resumed`. The new run's `parent_id` points to it.
- The scheduler, `noskill run --resume <id>` and chat replies all use this path.

## Consequences
- No IPC socket in Phase 1. The tray app (FR-I7, Phase 2) can read and write the same tables, or use `/noskill/v1`. If it needs push events later, an IPC socket can be added then without changing the job model.
- Changes from the CLI reach a running job within one tick (≤ 5 s), not at once.
- A continuation holds the conversation so far, so it's as sensitive as a trace. It lives in `state.db` next to the traces index, and the 4 MB cap stops a long run from bloating the database.
- If a skill or the model is removed between parking and approval, the resume fails with a clear message and the job run ends as `failed`.
- Jobs wait for queued interactive requests before they start a run, but don't yield between steps yet (PROXY.md "Concurrency").
