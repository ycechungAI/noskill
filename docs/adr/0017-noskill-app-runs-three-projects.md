# ADR-0017: The noskill app runs noskill, orcastrator and scheduler as they are

Status: **accepted** · Date: 2026-11-01 · Implements FR-I8

## Context
Three local projects all run on Ollama and overlap:
- **noskill** (Go): skills, routing, the proxy, its own scheduler (FR-J1) and a `storm` skill.
- **orcastrator** (Python, `~/dev/orcastrator`): STORM research reports with a web page on :8400.
- **scheduler** (`sched`, Python, `~/dev/scheduler`): Markdown task files on a cron schedule, with
  a web page on :8787 and login agents.

Each has its own start script, port and page, so using all three means three terminals and three
browser tabs. The user wants one app with one interface that starts and runs them.

Two ways to get there:
1. **One app over the three.** A small app starts, stops and watches the three as they are and
   shows their pages in one window.
2. **Merge the code into noskill.** Port orcastrator's report pipeline and sched's task files into
   Go, so there is one binary.

## Decision
**Option 1, as a native macOS app** in `apps/macos/` (SwiftUI, built with SwiftPM and only the
Command Line Tools). `noskill-tray` stays the cross-platform tray.
- The app holds no state of its own beyond `app.json` (project folders and ports). Each project
  keeps its own data, config and tests.
- It finds a part by its port: a port that answers counts as running, whoever started it. It starts
  only what isn't running and stops only what it started.
- The pages are each project's own, shown in a web view. The Overview, Logs and menu-bar icon are
  native.

## Consequences
- Works today with no change to the three projects, and each can still run on its own.
- It's macOS only. That breaks the "every feature on macOS, Windows and Linux" rule in AGENTS.md
  for this one app, on purpose: it's a prototype, and `noskill-tray` covers the other systems.
- Scheduling exists twice (noskill jobs and sched tasks). Merging them, and moving the STORM report
  into noskill's `deepresearch` engine, is still possible later (option 2); this app is the step
  before it, and its window wouldn't change.
- The app needs the two Python projects' `.venv` folders, made by their own start scripts.

**2026-11-03 update:** the two projects moved from `~/dev/orcastrator` / `~/dev/scheduler` into this
repo (`apps/orcastrator/`, `apps/scheduler/`, `git subtree --squash`). The app's defaults and the
moving parts (app.json, venvs, docs) point at the repo paths now; nothing above changes otherwise.
