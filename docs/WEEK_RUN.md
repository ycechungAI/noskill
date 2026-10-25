# Phase 3 exit: the week-long unattended run (macOS)

Exit rule ([ROADMAP.md](ROADMAP.md)): a week of unattended nightly runs with **correct naming and zero unexpected writes**. macOS first; Windows and Linux repeat it after one real trash and one real `noskill undo` there ([REVIEW_BLOCK3.md](REVIEW_BLOCK3.md)).

Status: **set up 2026-10-18, binary frozen after the last rebuild at about 02:45; **the week starts with the 02:30 run on 2026-10-19 (day 1).** Verified 02:44: after the Full Disk Access grant the service's anime run took 13 s with no "didn't answer in time" problems (it took 3 min before). Daemon installed (runs `~/.noskill/bin/noskill serve` at login; a stable copy built from this tree, so rebuild it there after code changes) and both jobs added. Still open: Transmission wasn't running when checked (nothing on port 9091), and no anime title is approved. Day 1 starts the first night after the macOS grant below is in place on the final binary. `daemon install --tray` installs the service only; the tray app isn't installed by it yet.

## Before day 1 (needs you)

1. **Transmission:** start it and turn on Settings › Remote › "Enable remote access" (port 9091, only 127.0.0.1). noskill doesn't change the app's settings. Check with `noskill anime detect`: it should say it answered.
2. **Approve at least one title** with a folder you choose: run `noskill anime run --stage suggest`, then `noskill approvals` (or the tray). For a folder on RED use the drive picker, so it is stored as `volume:RED/…`.
3. **Install the daemon** (adds a login item, so it needs your OK): `noskill daemon install --tray`. Scheduled jobs run only while it is up.
4. Dry run: `noskill anime run --dry-run` shows what a night would do without touching anything.

## Found on the first trigger (2026-10-18, 02:10–02:30)

Triggering `anime-nightly` by hand turned up three real problems; the week isn't counted until the third is fixed on your side.

1. **Fixed:** the skill declared `read:${config.download_dir}` and `write:${approved.dest}`, which can't resolve on a fresh install, so every model-driven run failed before its first step. It now declares `$DOWNLOADS`; `TestBuiltinSkillPermissionsResolveWithNoSettings` checks every built-in skill.
2. **Fixed:** a tool call was cut off after 2 minutes, half way through the tracker. `anime_run` now gets 45 minutes and `docs_index` 30 (`Budget.ToolTimeouts`).
3. **Needs you (macOS privacy):** the background service reads `~/Downloads`, `~/Desktop` and `~/Movies`, which macOS protects. macOS asks per program and ties the answer to the program's signature; `~/.noskill/bin/noskill` is ad-hoc signed, so the answer is lost every time it is rebuilt, and unanswered requests make those folders look hung (the library scan then leaves them out and says so). Fix: System Settings › Privacy & Security › **Full Disk Access** (covers the three folders and the external drives) › **+** › press ⌘⇧G and enter `~/.noskill/bin/noskill` › switch it on. **Don't rebuild that binary during the week**; if it is rebuilt, grant it again. A real signing identity would end this, but signing is deliberately left to the release step.

The scheduled 02:30 run then worked (17 s: 3 titles offered, episodes added to Transmission), so the privacy prompt is intermittent, not constant; still grant it. 4. **Fixed after your two requests:** finished torrents are stopped from seeding and the file is renamed in the download folder before it is moved. The first live filing then found that the download folder counted as "already have it" (it would have trashed the downloads unfiled); fixed, and the six finished episodes were filed for real (`noskill undo anime-01M56VXSM3AE9WW38GHQF2QNWZ` reverts them; its `--dry-run` shows `!` notes on the rename steps because it checks before restoring anything).

Also seen: AniList took ~8 s to answer from this Mac, close to noskill's request timeout; if `can't reach AniList` repeats at night, that is the network, not noskill.

## The jobs

```bash
noskill schedule add anime-tracker --name anime-nightly --prompt "Run the anime tracker: all stages." --cron "30 2 * * *" --missed once --timeout 2h --retries 1 --notify os
```

```bash
noskill schedule add local-docs --name docs-refresh --prompt "refresh the document index" --cron "0 3 * * *" --missed skip --notify none
```

Add watches (`watcher` skill) only if you have pages or feeds you actually want; each creates its own job. `--missed once` makes a night the Mac slept through run once on wake.

## Followed donghua (added 2026-10-18)

Renegade Immortal, Battle Through the Heavens and A Mortal's Journey are followed from config (newest episode each week, smallest file, filed flat into `volume:RED/@DONG+`). Renegade Immortal comes out Sundays around 1 pm Eastern, so one more job runs the download and file stages on Saturday and Sunday afternoons and evenings (Battle Through the Heavens and A Mortal's Journey drop Saturday afternoon, Renegade Immortal Sunday about 1 pm Eastern):

```bash
noskill schedule add anime-tracker --name anime-follow --prompt "Run the anime tracker with stage download, then again with stage file." --cron "0 14,16,18,20 * * 6,0" --missed once --timeout 1h --notify os
```

## Each morning (one command)

```bash
scripts/week-audit.sh            # PASS or FAIL for the last 24 hours; logs to ~/.noskill/logs/week-audit.jsonl
```

The installed binary doesn't have `noskill anime audit` yet (it is built from source on purpose: rebuilding the installed one loses the Full Disk Access grant). The script copies `state.db` and `config.toml` to a scratch folder, runs the audit from this source tree against the copy with `go run`, and appends the result to the real log. It changes nothing else. After the week, with the rebuilt binary, `noskill anime audit` does the same. A ✗ line means follow the stop rules below; the ! line about undo is only a note (a dry run can't see steps that depend on earlier ones, and old runs' files are long gone).

The table below is what the audit checks, for doing it by hand:

## Each morning, by hand

| Check | Command | Pass |
|---|---|---|
| The job ran | `noskill schedule history anime-nightly` | one run per night, status ok |
| What it did | `noskill anime status` and the run's report | titles and episode counts as expected |
| Names | `ls` your anime folder | `Title/Title S02E03 [480p].mkv`, no `.part` files, no duplicates |
| Nothing unexpected written | `find ~/Downloads ~/Movies/Anime /Volumes/RED -newermt "-24 hours" -type f \| head -50` | only the episodes in the report |
| Undo works | `noskill undo anime-<id> --dry-run` (the id ends the report when files were filed) | lists the same files |
| Trash | Finder's Trash | only finished downloads and replaced releases |
| Waiting on you | `noskill approvals` | nothing stuck; cards expire |
| Daemon alive | `noskill daemon status`, `noskill daemon logs` | running, no repeated errors |

## Stop and fail rules

- Any file outside the approved folders, the download folder or the Trash changed by noskill: **stop** (`noskill schedule pause anime-nightly`), run `noskill undo` for that run, and record it below.
- Downloads folder, a title folder or a file you didn't expect in the Trash: stop, same steps.
- A night skipped for a reason other than the Mac being off: note it; two in a row is a fail.
- Wrong names on any file: note it; fix the naming and restart the week.

## Log

| Date | Ran | Filed | Notes |
|---|---|---|---|
| 2026-10-18 | 02:10, 02:11, 02:15 (by hand: three failures, all fixed or explained above); 02:30 scheduled: ok | 0 | Not counted as a clean night. Day 1 is the first night with no problems. |
| 2026-10-18 ~04:00 | none | 0 | **Binary rebuilt on purpose** (adds `noskill hook`, which files one finished torrent at once; `~/.noskill/bin/noskill` replaced, ad-hoc re-signed, daemon restarted, no job was running). The Full Disk Access grant is lost with the new signature: **re-grant it before the 02:30 run on 2026-10-19**, then check `noskill anime run --dry-run` for "didn't answer in time". Day 1 is unchanged. The hook isn't set up in Transmission (optional; see CLI.md `noskill hook`); until it is, episodes are filed by the scheduled runs. Old binary kept at the session scratchpad as `noskill-old`. |
| 2026-10-18 ~04:30 | none | 0 | Transmission's "call script when download completes" turned on by noskill's session (`defaults write org.m0k.transmission DoneScriptEnabled/DoneScriptPath` → `~/.noskill/bin/transmission-done.sh`). Results in `~/.noskill/logs/hook.log`. To undo: untick it in Transmission › Settings › Transfers › Management. |

## Config change before day 1 (2026-10-18, 12:30)
Two edits in `~/.noskill/config.toml` (backup: `config.toml.before-main-model`), made at your request, with the installed binary unchanged:
- `orchestrator.memory_burst_gb = 8`: the budget grows from 6 GB to at most 8 GB (it was growing to 11.1 GB).
- A rule `needs tools → granite4:3b`, plus the built-in code rule (a rules list in the file replaces the defaults). Every model-driven run that offers tools, `anime-nightly` included, now uses `granite4:3b` (2.9 GB), where it used `llama3.2:latest`. On the checked `files` tasks `granite4:3b` passed 9 of 10 at 9 s each and `llama3.2` passed 3 of 10. It hasn't run the anime job itself yet, so the first nights show how it does: look at `noskill traces list` and the file moves. To go back, restore the backup file.

## Models changed again (2026-10-18, 12:40)
- `llama3.2:latest` and `llama3.2:3b` are removed (they shared one file). It passed 3 of 10 checked `files` tasks.
- `gemma3:1b` (806 MB, no tool support) is the backup model and takes summarise and extract requests (two rules in the config, before the tools rule). It scored 10 of 11 on those checked tasks, the same as `llama3.2`, at 0.6 s a task. A request that needs tools skips those rules, so tool runs, `anime-nightly` included, use `granite4:3b`.
- Backup before this edit: `~/.noskill/config.toml.before-gemma`.

## The new status page this week
`./ask.sh` (repo root) runs the page with the Approvals, Downloads and History tabs from source on port 11436, next to the daemon, with `serve --no-jobs` so it runs no jobs. The installed binary and its page on :11435 are unchanged. Answers you give there are picked up by the daemon. The installed daemon's filing run doesn't know about downloads added by hand on the page: use "Stop seeding and file now" for those.

## What gets asked (decided 2026-10-18)
Followed donghua (Renegade Immortal, Battle Through the Heavens, A Mortal's Journey) download without asking: you are getting them for sure. New shows are asked about, as before. For episodes of shows you approved, the new `ask_before_download = "unsure"` (the default in the source; the installed binary doesn't have it yet) asks only when a release's name isn't exactly the show's. `true` asks about every episode of approved shows; `false` never does.

## Torrents not stopped after finishing (2026-10-18, 14:50)
Two finished torrents were still seeding at 14:50: nothing stops a download when it finishes except the next filing run (16:00, 18:00, 20:00), and Transmission's "call script when download completes" never fired (`hook.log` has only my test line), most likely because Transmission was already running when the setting was written, so **quit and reopen Transmission once** to turn the hook on. I filed both by hand at 14:55 (`noskill anime run --stage file`). The source now adds each torrent with a zero ratio limit, so Transmission stops it by itself the instant it finishes; that needs the rebuilt binary. Until then: Transmission › Settings › Transfers › Seeding › "Stop seeding at ratio 0.0" does the same for every torrent.

The 14:00 follow run also showed `granite4:3b` making the two right calls and then carrying on with the files tools: it made an empty folder `~/Downloads/2026-10` and tried to move a line of the report as if it were a file. Nothing was lost (the move failed). The folder is empty; move it to the Trash if you don't want it. This is why running the jobs without the model (a direct tool call) is the safer setup for the week.

## Granite trial and a gotcha (2026-10-18, 12:40)
- **Trial:** `noskill run --skill anime-tracker --model granite4:3b "Run the anime tracker with stage download, then again with stage file. Set dry_run to true so nothing changes."` took 30 seconds. It made the two `anime_run` calls it should (`{"dry_run":true,"stage":"download"}`, then `"file"`) and described the result correctly. So the model part of the nightly and follow jobs works.
- **Gotcha, fixed in source (not in the installed binary):** run from your home folder, `~/.noskill/config.toml` was also read as a project's `.noskill/config.toml`, whose per-skill settings are ignored, so the run used `nyaa.si` (blocked on this network) and not your `nyaa.media` mirror. The scheduled jobs run from elsewhere and are not affected. **Until the rebuild, don't run `noskill anime …` from your home folder (`cd /tmp` first).**

## Windows
`ask.ps1` is the Windows twin of `ask.sh` (builds the source, serves the page, opens it); `scripts/transmission-done.cmd` is the Transmission done-script for Windows (`script-torrent-done-filename` in its `settings.json`); `start.cmd` is the menu ([START.md](START.md)). `scripts/week-audit.sh` needs the `sqlite3` tool, so on Windows run `noskill anime audit` once the installed noskill has it. These PowerShell and batch files have not been run on a real Windows machine yet; `noskill ui` is Go and is covered by the tests.
