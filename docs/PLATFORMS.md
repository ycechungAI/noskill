# Platforms: macOS, Windows, Linux

Goal G1/G2: identical features everywhere, under 2 minutes to first run.

## Install

| OS | Primary | Also |
|---|---|---|
| macOS | `brew install noskill/tap/noskill` | Signed + notarized `.pkg`, `curl -fsSL https://…/install.sh \| sh` |
| Windows | `winget install noskill` | Scoop, signed per-user `.msi`, `irm https://…/install.ps1 \| iex` |
| Linux | `curl -fsSL https://…/install.sh \| sh` | `.deb`, `.rpm`, AUR (`noskill-bin`), static tarball; Nix (community) |

All installers drop a single binary (plus the optional tray app), per-user, without admin rights. Nothing else is required until a skill needs it. Signing, update and uninstall details: [PACKAGING.md](PACKAGING.md).

## First run: `noskill init`
1. Detect backends: Ollama `:11434`, LM Studio `:1234`, llama.cpp `:8080`, MLX, plus `OLLAMA_HOST` etc.
2. If none: offer to install Ollama (opens the official installer, doesn't bundle it).
3. List models; recommend 2–3 pulls if missing for a good default set by RAM:
   - ≤ 8 GB: `qwen3:4b` + `nomic-embed-text`, decisions emulated on `qwen3:4b` (`tev1:0.8b` next to a 4B model is 6.6 GB, more than an 8 GB machine's budget; untested)
   - **16 GB (design target):** `qwen3:4b` + `qwen2.5-coder:7b` + embed + `tev1:0.8b` (decisions, resident), 8 GB budget. Only one primary is loaded at a time. `qwen3:8b` and a 7B vision model are optional: they don't fit next to the resident model in the default budget ([ADR-0007](adr/0007-16gb-memory-budget-and-model-defaults.md)). `tev1` (4B) is too big next to an 8B model here ([ADR-0006](adr/0006-decision-model-on-16gb.md)).
   - 24 GB: as 16 GB, with `tev1` for decisions if `noskill models bench` shows it fast enough.
   - ≥ 32 GB: add a 14B–32B general/coder + a vision model; `nimble` for decisions
   - Requires Ollama ≥ 0.35 for decision models; older versions fall back to emulated decisions and `init` suggests upgrading.
4. Probe tool strategy per model; quick benchmark (≤ 60 s total, skippable).
5. Offer `noskill daemon install` (needed for schedules).

## Daemon / background service (FR-J2)

| OS | Mechanism | Location |
|---|---|---|
| macOS | launchd LaunchAgent (per user) | `~/Library/LaunchAgents/dev.noskill.daemon.plist` |
| Linux | systemd `--user` unit (+ `loginctl enable-linger` prompt for headless boxes); fallback: XDG autostart | `~/.config/systemd/user/noskill.service` |
| Windows | Task Scheduler "at logon" task (no admin), or Windows Service when installed via the per-machine MSI (admin) | `\noskill\daemon` |

- CLI ↔ daemon IPC: Unix domain socket (`~/.noskill/run/noskill.sock`) / Windows named pipe (`\\.\pipe\noskill`).
- Sleep/wake: daemon listens for power events (IOKit / logind D-Bus / `WM_POWERBROADCAST`) to apply missed-run policy and re-check backends.

## Paths

| | macOS | Linux | Windows |
|---|---|---|---|
| Config | `~/.noskill/` (override `NOSKILL_HOME`) | `$XDG_CONFIG_HOME/noskill` | `%APPDATA%\noskill` |
| Data | `~/.noskill/` | `$XDG_DATA_HOME/noskill` | `%LOCALAPPDATA%\noskill` |

Skills use **portable path tokens** so the same skill works everywhere:
`~` (home), `$DOWNLOADS`, `$DOCUMENTS`, `$DESKTOP`, `$PICTURES`, `$VIDEOS`, `$MUSIC`, `$TEMP`, and `volume:<label>`.

## External drives (`volume:<label>`)
Needed by anime-tracker and other storage skills.

| OS | Resolve label → mount | Change events |
|---|---|---|
| macOS | `/Volumes/<label>`; confirm via `diskutil info -plist` (volume UUID) | DiskArbitration / FSEvents on `/Volumes` |
| Linux | `/run/media/$USER/<label>`, `/media/...`, or `/mnt/...` via `lsblk -J -o LABEL,UUID,MOUNTPOINT` | udev / polling |
| Windows | Drive letter whose volume label matches (`GetVolumeInformationW`) | `WM_DEVICECHANGE` / polling |

Rules:
- Bind skills to a **volume UUID** after first use (labels can collide).
- Check free space before writes; refuse below a reserve (default 5%).
- Filename sanitisation per target filesystem (exFAT/NTFS forbid `:*?"<>|`; FAT32 4 GB file limit warning).
- Precondition `--when volume:RED` lets jobs wait or skip cleanly (FR-J5). Its siblings: `ac-power`, `unmetered` and `window:HH:MM-HH:MM`.
- Built in block 3 (`internal/volumes`): the UUID comes from `diskutil info -plist` (macOS), `/dev/disk/by-uuid` (Linux) or `GetVolumeNameForVolumeMountPoint` (Windows). The label is bound on first use: by `schedule add --when volume:<label>`, or by the first run that resolves `volume:<label>/…`. A different drive with the same label is refused until `noskill volumes forget <label>`. Two unbound drives with one label are refused, not guessed. Mount watching is polling: the scheduler rechecks every few seconds, with results cached for 30 s.

**Metered network** (`--when unmetered`): Linux reads NetworkManager's `Metered` property over D-Bus, and Windows reads the WinRT connection cost (Fixed or Variable = metered). macOS reports it only through Network.framework, which needs CGO, so there it is unknown. Unknown counts as unmetered, as unknown power counts as on AC.
- A folder the user picks on an external drive (e.g. `/Volumes/RED/ANIME2026`) is stored as volume UUID + label + relative path (`volume:RED/ANIME2026`), so it still resolves if the mount point changes (`/Volumes/RED 1`, a different drive letter).

## Tray / menu-bar app
`noskill-tray` ([ADR-0004](adr/0004-tray-app.md)) installs with `noskill daemon install --tray` and starts at login.

| OS | Shows as | Notes |
|---|---|---|
| macOS | Menu-bar icon | Signed + notarised with the main binary |
| Windows | Notification-area icon | Toast notifications with Approve/Skip buttons |
| Linux | StatusNotifierItem (KDE, XFCE, Cinnamon, most others) | GNOME needs the AppIndicator extension; `doctor` explains. Approvals still work via notifications + CLI |

## Notifications
macOS `UNUserNotificationCenter` (falls back to `osascript`), Windows toast, Linux `notify-send`/D-Bus. Remote: ntfy, Telegram, Discord/Slack webhook, email (SMTP).

## GPU / acceleration
noskill itself doesn't need a GPU; backends handle it. The orchestrator reads memory info:
- macOS: unified memory via `sysctl hw.memsize` + `vm_stat`; Metal via backend.
- NVIDIA: `nvidia-smi --query-gpu` (or NVML), AMD: `rocm-smi`, Intel/Windows: DXGI adapter memory.
- Falls back to Ollama `/api/ps` sizes.

## CI matrix
GitHub Actions: `macos-14` (arm64), `macos-13` (x64), `windows-latest` (x64), `windows-11-arm`, `ubuntu-latest` (x64), `ubuntu-24.04-arm`. Integration tests run against a real Ollama with a tiny model (e.g. `qwen3:0.6b`) on at least Linux + macOS.
