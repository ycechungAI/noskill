# ADR-0011: The tray and the status page use the loopback HTTP API, not a separate IPC

Status: **accepted** · Date: 2026-10-16 · Amends [ADR-0004](0004-tray-app.md)

## Context
ADR-0004 had the tray talk to the daemon "over the same IPC as the CLI (Unix socket / named pipe)". That IPC was never built: the CLI reads and writes `state.db` directly ([ADR-0009](0009-scheduler-through-the-database.md)) and only `serve` holds the running engine. The daemon already has a local HTTP API with Host, Origin and token checks, an event stream and approvals endpoints ([PROXY.md](../PROXY.md)). A second protocol would mean a second set of checks to get right on three OSes.

## Decision
- The tray and the status page call `/noskill/v1` on loopback. No IPC socket or named pipe is added.
- **Admin clients.** A `[[server.tokens]]` entry with `admin = true`, and the unauthenticated `local` client (when no tokens are set, so only this machine can reach the port), may:
  - see every approval, not only the ones their own requests caused, and approve ones that need `exec` (chat clients still can't);
  - read `GET /noskill/v1/status` (models, jobs, approvals, runs, skills, learning, eval) and run `POST /noskill/v1/jobs/{id}/{pause|resume|run}` and `jobs/all/{pause|resume}`.
- The tray looks for the token named `tray`. With tokens configured but none named `tray` it says so and doesn't start.
- `GET /ui` serves the status page, which is static HTML and needs no token. Its own calls carry the token, which you paste into the page and which stays in that tab's `sessionStorage`. A strict Content-Security-Policy (no external scripts, styles or frames) comes with it.
- "Pause all" remembers the jobs it paused (`app_state` key `jobs.paused_all`), so "resume all" doesn't wake a job you had paused yourself.

## Consequences
- One set of checks protects every client. A tray crash still affects nothing else.
- The tray can't reach a daemon that isn't running; it shows "noskill: not running" and keeps polling. The CLI doesn't need the daemon.
- The "tray/core version skew" note in PACKAGING.md becomes the daemon's version in `/noskill/v1/status` instead of an IPC protocol number.
- Not built: `daemon install --tray` (starting the tray at login). That changes login items, so for now the tray is started by hand or added to your login items yourself.
