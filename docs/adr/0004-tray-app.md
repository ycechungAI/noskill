# ADR-0004: Tray app is a separate small binary

Status: **accepted** · Date: 2026-10-15 · Amended by [ADR-0011](0011-tray-and-status-page-over-http.md): the tray uses the loopback HTTP API, not an IPC socket

## Context
The tray/menu-bar app is a top interface (Q10). Its main job is approvals: anime picks in `approve` mode, learned skills on first use, and `confirm` actions in unattended jobs. Tray libraries need native code on macOS (Cocoa) and, depending on the library, on Windows or Linux too. The core daemon must stay a CGO-free single binary (ADR-0001).

## Decision
- Ship **`noskill-tray`** as a separate binary in the same release, built with CGO per OS.
- It holds **no state and no logic**. It talks to the daemon over the same IPC as the CLI (Unix socket / named pipe) and subscribes to an event stream (job started/finished, approval requested, error).
- Library: `fyne.io/systray` (Cocoa on macOS, Win32 on Windows, D-Bus StatusNotifierItem on Linux). Details beyond the menu open the daemon's **web status page** (FR-I6) in the default browser, so there's no webview to ship.
- Started at login by the same service install as the daemon (`noskill daemon install --tray`).

## Menu (v1)
```
● noskill: running · qwen3:8b loaded
  Approvals (2) ▸   Frieren S2: approve / skip
                    Learned skill "release-notes-lookup": use / reject
  Jobs ▸            anime-tracker: next 03:00 · last ok
                    Run now / Pause
  Pause all jobs
  Open status page
  Quit tray (daemon keeps running)
```

## Consequences
- If the tray crashes, nothing else is affected; the CLI and notifications still work.
- Linux: GNOME needs the AppIndicator extension to show tray icons. Approvals still arrive as notifications and in the CLI; `noskill doctor` explains the extension.
- One more binary to sign and notarise per platform.
