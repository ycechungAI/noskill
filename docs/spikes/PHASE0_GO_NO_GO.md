# Phase 0 go/no-go
2026-10-15 · decided on: S1–S5 and S9 (macOS) results; S6, S7, S8 not run · rule from [ROADMAP](../ROADMAP.md#phase-0-validate-3-weeks) and [PHASE0_TESTS](../PHASE0_TESTS.md#schedule-16-working-days-3-weeks)

## Verdict: **conditional go**
Start Phase 1 block **1a** (Go skeleton and core interfaces) now. The strict exit rule isn't met, because **S7 and S9-Linux haven't run**. Neither result changes the skeleton's shape, but each one gates a later Phase 1 piece. They become carry-over items with deadlines (below), not reasons to wait.

## Exit criteria
| Criterion | Status | Evidence |
|---|---|---|
| S1 gives a clear default tool strategy | **Met.** Native tool calling by default, constrained as fallback | [S1](S1-results.md), [ADR-0005](../adr/0005-native-tool-calling-default.md) |
| S3 gives a clear decision model | **Met.** `tev1:0.8b` asking one question on 16 GB, `tev1` above, a cache ceiling, a ≤ 300/600 ms budget | [S3](S3-results.md), [ADR-0006](../adr/0006-decision-model-on-16gb.md) |
| S2 passes | **Met.** One port for `/v1` and `/api` with Open WebUI, the OpenAI SDK and the ollama CLI; +0.1–0.6 ms | [S2](S2-results.md) |
| S7 passes, at least without notification buttons | **Not run** on any OS | — |
| S9 passes on macOS | **Met.** All 24 attacks blocked, legitimate Python and Node work runs, +41–45 ms, copy-in ≤ 92 ms | [S9](S9-results.md), [ADR-0008](../adr/0008-macos-script-sandbox.md) |
| S9 passes on Linux | **Not run** | — |

The other results: S4 changed the 16 GB defaults ([ADR-0007](../adr/0007-16gb-memory-budget-and-model-defaults.md)). S5 confirmed going standalone instead of building on Hermes ([ADR-0003](../adr/0003-standalone-not-hermes.md)). S6 (qBittorrent) and S8 (tray) are not exit criteria.

## Why go anyway
- **Block 1a builds the interfaces, not the per-OS mechanisms:** package layout, config, SQLite, the `internal/platform` interfaces, the backend and `decide` interfaces, the agent loop, orchestrator v1, the permission broker and proxy auth. S7 and S9-Linux decide what goes *behind* the `platform.Service`, `platform.Notifier` and `platform.Sandbox` interfaces, not their shape.
- **The sandbox design is proven on one OS:** the broker, the copy-in workspace, the egress proxy with per-run credentials, and the deny-by-default script sandbox. The macOS mechanism works within the budget. Linux has two well-documented mechanisms (Landlock, bubblewrap) and a fallback rule already in SECURITY.md ("refuse to run unless `[sandbox] mode = "warn"`").
- **Waiting costs more:** S7 and S8 need Windows and Linux machines that aren't set up yet.

## Carry-over items and their deadlines
| Item | Must be done before | If it fails |
|---|---|---|
| **S9 Linux:** Landlock ABI and file rules, TCP rules (≥ 6.7), blocking non-TCP sockets, bubblewrap + netns + proxy over a Unix socket, `systemd-run --user` limits, Docker, kernel 5.15 | Block 1b's script runtime ships on Linux | Require bubblewrap on kernels without Landlock net rules; scripts with `network` or user-data inputs refuse to run otherwise (SECURITY.md fallback) |
| **S9 Windows:** AppContainer + Job object, uv's Python inside it, loopback/domain-filtering answer | The script runtime ships on Windows | Ship the runtime in a noskill-owned folder with ACLs; scripts with `network` need approval per run |
| **S7 macOS:** launchd user agent, sleep/wake, missed runs, notification buttons | Block 1b's daemon/service install | v0.1 approvals use the CLI and plain notifications; buttons move to the tray (v0.2) |
| ↳ outcome (2026-10-15, [S7](S7-results.md)) | **Partly done.** Install/uninstall passes (no admin, clean). **Still open:** sleep/wake and missed runs, reboot/log out, notification buttons (a notification was shown per the user, but no Approve/Skip click is in the log). The helper `.app` and the wall-clock scheduler design are known | Same fallback |
| **S7 Windows and Linux** | Phase 1 exit (SPEC §6 on all three OSes) | Same fallback |
| **S8 tray, all OSes** | Phase 2 (tray is v0.2) | Wails or native tray code ([ADR-0004](../adr/0004-tray-app.md)) |
| **S6 qBittorrent** (needs your export) | Phase 3 (`anime-tracker`) | Documented substitutes per endpoint |
| `router-200` second labeller | Phase 2 threshold tuning | Keep the single-labeller numbers, marked as such |
| S4 on W1 and from a cold boot; S5 on W1/L1 | Phase 2 (memory slots) | Defaults stay conservative |

## Risks carried into Phase 1
1. **`sandbox-exec` is deprecated.** It still works on macOS 27.0.1. If Apple removes it: WASM only for learned code, and approval per run for other scripts.
2. **No kernel memory limit for normal users on macOS.** The watchdog overshot 512 → 869 MB at 50 ms polling. Keep headroom in the memory budget; poll with `proc_pidinfo`.
3. **Scripts allowed to fork can leave confined detached processes on macOS.** Mitigated by per-run proxy credentials, a post-run sweep, and fork off by default.
4. **Windows domain filtering may stay advisory in v1** (not yet measured).
5. **Small-model quality on 16 GB.** The default primary is now a 4B model (ADR-0007), so skills and the router must work at that level.
6. **The decision model's prompt cache grows** (S3). The ceiling is in place; Ollama's MLX runner has no cap setting of its own yet.
