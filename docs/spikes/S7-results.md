# S7 results: Daemon, scheduler and notifications (macOS part)
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1, normal user (no admin rights used) · harness [spikes/s7](../../spikes/s7/): `main.go` (daemon, `oneshot`, `install`/`uninstall`, `notify`; stdlib only), `notifyapp/` (Swift helper `.app`, built with `swiftc`, ad-hoc signed), `report.py` · raw rows: `spikes/results/s7-macos.jsonl`

**Status: partly run.** Install/uninstall and on-schedule timing were measured. Not run: sleep/wake and missed runs, and reboot / log out (the user did not choose them). The notification was reported as shown by the user, but the log has no Approve/Skip click. Windows and Linux were not run either. Steps for the open tests are at the end.

## What was built
- `s7 daemon`: a resident job runner. Every 5 minutes (slots aligned to the wall clock: :00, :05, …) it appends a JSON line (`scheduled`, `actual`, `drift_ms`, `kind`, `collapsed`). It polls the wall clock every second, because `time.Sleep`/`time.Ticker` use the monotonic clock, which stops during sleep. The last handled slot is saved in a state file so a restart also counts as a missed run. It logs a `wake` event when two polls are more than 5 s apart. Missed-run policy `-policy once` runs one catch-up job for all missed slots; `skip` runs only a slot it can still handle within 60 s and logs the rest as `skipped` (FR-J3).
- Three launchd user agents ran side by side and wrote to the same file: `dev.noskill.s7.once` (resident, policy once, plus a plain `time.Ticker` for comparison, plus the Unix-socket IPC), `dev.noskill.s7.skip` (resident, policy skip) and `dev.noskill.s7.calendar` (no resident process: launchd's own `StartCalendarInterval` starts `s7 oneshot` every 5 minutes).
- IPC: `s7 notify` → Unix socket `~/.noskill-s7/run/s7.sock` → the daemon starts the helper `.app`, and the helper reports its events back over the same socket (PLATFORMS.md: CLI ↔ daemon over a Unix socket).

## Numbers

### Install and uninstall (no admin)
| | Result |
|---|---|
| `s7 install` (copy binary and helper, write plist, `launchctl bootstrap gui/$UID`) | 0.1 s for two agents; no password, no prompt in the terminal |
| Agent is running after install | yes, within 1 s (`launchctl list` shows the PID; first log line at install time) |
| `s7 uninstall -variant all` | 0.03 s; `bootout` sends SIGTERM, the daemon logs `stop` and exits |
| Left behind after uninstall | nothing: no loaded job, no process, no plist in `~/Library/LaunchAgents`, no `~/.noskill-s7`, no helper in `~/Applications`. The plists, the app folder and the helper are in the Trash. Run twice (full set with 56 min of state and logs, then a clean install/uninstall cycle) |
| How files reached the Trash | a plain `rename` into `~/.Trash` worked every time (run from a normal shell, not from inside a launchd agent). The Finder/`osascript` fallback in `trash()` was never needed, so it is untested. A second item with the same name gets a time suffix |
| Not checked | whether macOS showed a "Background Items Added" banner for the new agents, and what `sfltool dumpbtm` lists (needs root). A user can switch such an item off in System Settings > Login Items; `doctor` should check that the agent is actually loaded |

### On-schedule timing (baseline, 14:16–15:12, 11 slots per agent, Mac awake the whole time)
| Scheduler | Runs | Median drift | Max drift |
|---|---|---|---|
| Resident daemon, wall-clock polling, 1 s tick (`once`) | 11 | 0.39 s | 0.84 s |
| Resident daemon, wall-clock polling, 1 s tick (`skip`) | 11 | 0.39 s | 0.88 s |
| launchd `StartCalendarInterval` → `s7 oneshot` | 11 | 5.03 s | 5.28 s (min 4.03 s) |
| Plain `time.Ticker` (naive) | 11 | 0.00 s | 0.00 s (awake: no difference; it only breaks over sleep, not measured) |

The 1 s polling tick is the whole drift of the daemon (0.05–0.9 s). launchd starts calendar jobs about 5 s after the slot every time (4.0–5.3 s), well inside 60 s.

### Sleep, wake, missed runs
**Not run.** The Mac never slept during the baseline (no `wake` event in 56 min), so there are no wake numbers. `pmset -g` shows `sleep 0` ("prevented by powerd, coreaudiod, Claude"), so this machine does not sleep by itself while it is in use, and waking a Mac mini needs a key press or a scheduled wake (`pmset schedule`, which needs root).

### Notifications from an unpackaged binary
| Test | Result |
|---|---|
| A bare Go/Swift binary calls `UNUserNotificationCenter.current()` | aborts: `NSInternalInconsistencyException: bundleProxyForCurrentProcess is nil`. A `.app` bundle with a bundle identifier is **required** |
| Smallest bundle that works | `Contents/Info.plist` (`CFBundleIdentifier`, `CFBundleExecutable`, `CFBundlePackageType`=APPL, `LSUIElement`=true) + one Mach-O (about 95 KB) from `swiftc` + ad-hoc signature (`codesign --sign -`). No Apple Developer account, no entitlements, no provisioning profile |
| Launch from the daemon | `exec` of `NoskillNotify.app/Contents/MacOS/noskill-notify` and `open -n -W -a` both start the helper and get the bundle identifier right, with the launchd agent as parent |
| Where the `.app` must live | **registered with LaunchServices**. A freshly copied bundle (in `~/.noskill-s7/`, in `~/Applications/`, or in the repo) made `usernoted` log `Failed to find or validate client of identifier …` and `tccd` log `kLSApplicationNotFoundErr`, and `requestAuthorization` failed within 0.1 s with `UNErrorDomain Code=1 "Notifications are not allowed for this application"` and **no prompt**. After `lsregister -f ~/Applications/NoskillNotify.app` the request reached the user. `install` now copies the helper to `~/Applications` and runs `lsregister -f`; `uninstall` runs `lsregister -u` and moves it to the Trash |
| Permission prompt | it is not a dialog. `usernoted` posts it as a notification banner from `com.apple.notificationcenter.askpermissions` that fades after a few seconds. Two requests stayed pending for 264 s with no answer; a third was auto-denied after 4.99 s. Once denied, every later request for that bundle identifier fails at once (0.08–0.16 s) with the same error. The user did not see a prompt during the attempts |
| Authorization state at the last logged run | `granted = false`, `UNErrorDomain Code=1` for bundle `dev.noskill.s7.notify4`. The settings call (`getNotificationSettings`) was never reached because the helper exits on denial. Whether a Focus mode muted the prompt was not checked |
| Button clicks reach the daemon | **no click event in the log.** After the last logged run the user reported seeing a notification, but nothing was recorded (the helper had exited and the daemon was uninstalled). The path is built and was exercised up to `auth`/`timeout`: helper → Unix socket → daemon → `s7-macos.jsonl` (`notify_auth`, `notify_timeout`, with `since_request_ms`). Not tested either: the case where the helper has exited and macOS relaunches it on a click |

## Pass/fail
| Criterion | macOS | Evidence |
|---|---|---|
| Install/uninstall without admin | **Pass** | launchd user agent, no password; 0.1 s / 0.03 s; nothing left behind except Trash items. **Not tested:** that the agent survives a reboot or log out (`RunAtLoad`/`KeepAlive` are set) |
| Job fires within 60 s of schedule after wake; missed-run policy correct | **Not run** | no sleep happened. On-schedule fires, awake: 0.84 s max (own scheduler), 5.3 s max (launchd calendar) |
| Action buttons work on macOS | **Not confirmed.** Notification shown (user-reported); click-through not confirmed in the log | the log's last helper run (15:12:28) ended with authorization denied, and there is no `notify_posted` or `notify_response` row; the user's report can't be matched to a logged post (it may have been the permission banner or a later grant). Fallback rule for v0.1 stays: CLI approvals + plain notifications, buttons in the tray (v0.2) |

Windows and Linux: **not run** (open).

## Decision
No change to SPEC or ADRs yet; nothing measured contradicts them. For Phase 1 block 1b (to put in `internal/platform/` when written):
- **Service (FR-J2):** a user LaunchAgent with `RunAtLoad` + `KeepAlive`, installed and removed with `launchctl bootstrap|bootout gui/$UID` needs no admin. `noskill daemon uninstall` must `bootout` first (SIGTERM, clean stop), then move the plist to the Trash.
- **Scheduler (FR-J3):** do not use `time.Ticker`/`time.Sleep` for schedules. Compare the wall clock on a 1 s poll (or wake events) and persist the last handled slot. This is the design the sleep test is meant to check.
- **Notifications (FR-A6):** a notification `.app` helper is mandatory on macOS (the main binary cannot post `UNUserNotificationCenter` notifications). It needs only an ad-hoc signature to start, must be registered with LaunchServices at install, and needs the user's permission. The first request must happen while the user is looking at the screen, from a step they started (`noskill daemon install` or `noskill notify --setup`), because the prompt is a banner that disappears in seconds and a missed prompt counts as a denial. `doctor` must report "notifications denied" and where to switch them on (System Settings > Notifications). Whether an ad-hoc signature is enough for button clicks is still unproven; a Developer ID signature (which the tray app needs anyway for notarisation, PLATFORMS.md) is the fallback.

## Surprises
- The permission request failed silently for an unregistered bundle (no prompt at all), which looked exactly like a denied permission. The only clue was in `log show` (`usernoted`, `tccd`).
- A permission request that is ignored is stored as a denial. Retrying needs a switch in System Settings or a new bundle identifier.
- launchd's calendar jobs start about 5 s late every time; the resident daemon is within 1 s.
- A test machine that never sleeps (a Mac mini in use, kept awake by Claude and audio) makes the wake path hard to see; it has to be forced with `pmset sleepnow`.
- `~/.Trash` could not be listed from the agent's shell (privacy protection) but could be written to; its contents were checked through Finder.

## Still open (steps)
**Rebuild and install** (no admin):
```bash
cd spikes/s7 && go build -o out/s7 . && notifyapp/build.sh
out/s7 install -variant once -helper out/NoskillNotify.app && out/s7 install -variant skip && out/s7 install -variant calendar
```
**Notification test.** First switch "noskill" on in System Settings > Notifications (alert style, sound on) if it is listed as denied; otherwise change the bundle identifier in `notifyapp/Info.plist` and `build.sh`, rebuild, reinstall, and run `out/s7 notify -launch exec` while watching the top-right of the screen (click Allow in the banner within a few seconds, or open Notification Center). Hover the notification (or use Options) and click **Approve**, then **Skip** on a second one. Read `python3 spikes/s7/report.py --after HH:MM` for `notify_response` rows. Also try `-launch open`, and let the helper time out and click afterwards to see whether macOS relaunches it.
**Sleep test.** Wait for two on-time runs, then `pmset sleepnow`. Wake after at least 30 minutes (key press) and run `python3 spikes/s7/report.py --after HH:MM`. Expected: a `wake` event with `gap_s` ≥ 1800; `once` logs one `catchup` run within about 1–2 s of the wake with `collapsed` = missed slots − 1; `skip` logs `skipped`, then runs on the next slot; `calendar` runs once after wake (`slots_since_last_run` shows the gap); `naive-ticker` drifts by about the sleep time. Pass if the next on-schedule run and the catch-up run fall within 60 s.
**Reboot and log-out test** (user's decision, kills open sessions): with the agents installed, reboot, log in, and check that the agents are running without anything started by hand (`start` events with a new `boot` value; `last_slot_saved` shows the saved slot, so the missed-run policy applies across the reboot). Repeat with log out/in. Without a login the user agent does not run at all (LaunchAgent, not LaunchDaemon); that is a limit to document for headless Macs.
**Clean up:** `out/s7 uninstall -variant all`.
