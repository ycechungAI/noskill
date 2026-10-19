# Block 3 review: file moves and trash

For the **Opus 5.5 (high)** review before the Phase 3 exit (a week of unattended nightly runs, [ROADMAP.md](ROADMAP.md)). The rule under review: **no hard deletes of user files anywhere; nothing is overwritten; every move, copy and trash is permission-checked and in the undo log** ([AGENTS.md](../AGENTS.md), [SECURITY.md](SECURITY.md)).

Status: **done 2026-10-18** (reviewed by Claude, on the model the session had when it was run; the brief asked for Opus 5.5 high, so the high-risk items below are worth a second look by Opus). Findings and the decision are at the bottom.

## Scope

1. **Anime filing**: `internal/anime/tracker.go`, the `file` stage (copy, verify, rename, trash the download, remove the torrent), the v2 replacement and the free-space check, both added in block 3.
2. **The broker and undo log**: `internal/perms/broker.go` (`Mkdir`, `Copy`, `Move`, `TrashPath`), `internal/app/undo.go`, and `animeFiler` in `internal/app/anime.go` (its scopes are computed per call).
3. **Drives**: `internal/volumes`, `volume:<label>` resolution, `Portable` in `internal/app/anime.go`, what happens when a drive is missing, full, or mounted somewhere else.
4. **Trash**: `internal/platform/trash_*.go` on all three OSes (only macOS has been run).
5. **Everything else that deletes or renames** (list below), to confirm none touches a user file.

Out of scope: the learning loop's skill snapshots (`internal/learning/library.go` renames inside noskill's own skill folders) unless something there can reach a user file.

## Every delete and rename outside tests

Checked by `grep os.Remove|os.RemoveAll|os.Rename` on 2026-10-16. Each should be noskill's own file; confirm.

| Where | What | Expected |
|---|---|---|
| `internal/perms/broker.go:68, 231` | `Move`, and the final rename of `<to>.part` in `Copy` | The only user-file renames; behind scope checks and the undo log |
| `internal/platform/trash_linux.go:60-66` | Reservation file and move into the XDG trash | Our own reservation file; the move is the trash |
| `internal/app/undo.go:73, 80` | Undo of a move/copy | Moves back; must not overwrite what is there now |
| `internal/tools/script.go:231, 252` | Unpack of a built-in skill's files | noskill's own runtime folder |
| `internal/learning/library.go:106-167, 278-292` | Skill versions, snapshots | noskill's own skill folders |
| `internal/learning/tuned.go:107` (added in 5a) | `TUNED.json.tmp` → `TUNED.json` | `<data>/tuned/<name>/` only; the name is a built-in skill's, never user input |
| `internal/app/train.go:236` (added in 5b) | `MANIFEST.json.tmp` → `MANIFEST.json`, `RUN.json.tmp` → `RUN.json` | `<data>/train/datasets/<id>/` and `<data>/train/runs/<n>/` only; the id is made by `train export` (a timestamp), n is a number |
| `internal/learning/script.go:73`, `internal/app/script.go:39`, `internal/app/simulate.go:179`, `internal/app/eval.go` (the eval task folder and, for `local-docs` tasks, a temp folder of the task's own documents: two more calls, both on an `os.MkdirTemp` path), `internal/sandbox/workspace.go:186` | Scratch folders | noskill-made temp folders only; check the path can't be user-influenced |
| `internal/update/update.go`, `internal/cli/cmd_update.go` | Binary swap and rollback | noskill's own binary |
| `internal/cli/cmd_config.go:179, 198`, `internal/cli/cmd_eval.go:273` | Temp file then rename onto config / progress file | Config file is the user's: check it is atomic and keeps a backup |
| `internal/cli/cmd_doctor.go:233`, `internal/service/service_windows.go:53` | Temp scratch | Temp only |
| `internal/app/registry.go:77` (moved from `cmd_skills.go` in 5c) | `GitFetch`'s shallow clone of a skill or registry | A folder it made with `os.MkdirTemp`; the URL only names what is cloned, never where |

## Things to look at first (from writing the code)

**Anime `file` stage**
- "Already have it" is decided by title confidence (≥ 0.9) plus the episode key. If a library file matches by name but is smaller, partial or another resolution, the download is counted as filed (`filed++`), the torrent is removed and the download goes to the trash. It is recoverable from the OS trash, but check that is the intended outcome, and whether it should require the same resolution or a size check.
- The whole `ContentPath` (a folder, for multi-file torrents) is trashed once every video in it was placed. Subtitles, extras and anything else in that folder go with it. Check whether that is acceptable or should trash only the placed videos.
- Order is: copy → record in state → remove the torrent (data kept) → trash the download. Look at each failure point for a state that loses a file or repeats work forever.
- **v2 replacement**: the old file is trashed **before** the new copy. If the copy fails, the old file is in the OS trash, the episode is removed from the filed list, and the torrent stays so the next run retries. Check the undo log records the trash so `noskill undo` can restore it, and that a retry that also fails can't leave the user with no file.
- Only files noskill filed (path equals `Filed[key].Path`) can be replaced. Check that a user who renamed or moved their copy is never replaced.
- **Free space** (`Tracker.space`): the check is of the nearest existing parent and has a time-of-check/time-of-use gap; a failed copy mid-file must not leave a `.part` file behind that later passes treat as the episode.
- `lib.Has` scans every external drive read-only. Check a slow, hung or disappearing network share can't stall or crash a run.

**Broker**
- `animeFiler.broker` grants `write:` on the download folder and on each granted destination. `TrashPath` needs write on the path: confirm it can trash only inside those scopes and can't be pointed at a granted folder itself or its parent.
- Symbolic links: does `Copy` or `TrashPath` follow them out of a scope? (`docs` and skill import skip links; check the broker does too.)
- Case-insensitive and Unicode-normalising file systems (macOS APFS/HFS+, exFAT, NTFS): "never overwrite" must hold when `Episode.mkv` and `episode.mkv` are the same file.
- Names: `sanitizeName` for `exFAT/NTFS` (reserved names like `CON`, trailing dots/spaces, length limits).

**Drives**
- `Portable` binds a label to a volume UUID on first use. Check a look-alike drive with the same label can't receive files (the resolver is meant to refuse it).
- A drive unplugged between the space check and the copy.

**Trash on Linux and Windows**
- Not run anywhere but macOS. Review `trash_linux.go` against the XDG trash spec (`info` file, per-volume trash for external drives, cross-device moves) and the Windows implementation for what happens on a drive with no recycle bin.

## Tests to add if missing

- A test per failure point in the `file` stage: copy fails, state write fails, torrent removal fails, trash fails.
- `Move`/`Copy`/`TrashPath` across a symbolic link out of scope, and onto an existing file with different case.
- Undo of a v2 replacement restores the first file and removes the second.
- A `grep`-style test that fails if non-test code outside the known list calls `os.Remove`/`os.RemoveAll` on a path that came from a user-controlled value (or document why one can't be written).

## Findings

Fixed in this change (tests in `internal/perms/review_test.go`, `internal/anime/review_test.go`, `internal/app/review_block3_test.go`, `internal/perms/deletes_test.go`):

| # | Severity | What | Fix |
|---|---|---|---|
| 1 | **High** | qBittorrent's "don't create subfolder" makes a multi-file torrent's `content_path` the download folder itself. After filing, the tracker trashed `ContentPath`, so **the whole Downloads folder** went to the trash (the broker allowed it: a scope root is "within" itself). | The tracker trashes the whole content only when it is strictly inside the torrent's save path, else only the videos it placed. The broker now refuses `Move`/`TrashPath` on a granted folder itself (`Set.IsRoot`). |
| 2 | **High** | Anime filing logs under `anime-<id>`, which has no row in `runs`; `noskill undo` answered "no such run", so **filing could not be undone**, and nothing told the user the id. | `Undo` accepts an id that has undo entries; the tracker report ends with `noskill undo anime-<id>` when it filed. Test: replace (trash, then copy) is undone and the first release is back. |
| 3 | Medium | A copy that died (unplugged drive, crash) leaves `<name>.part`; `copyVerified` refuses to start while one exists, so that episode **failed every night for ever**. | `clearStalePart`: a `.part` with no write for 5 minutes goes to the trash (logged, undoable), then the copy runs. A fresh one still blocks. |
| 4 | Medium | Windows: `SHFileOperationW` with `FOF_NOCONFIRMATION` answers "yes" to "permanently delete?" when a file is too big for the bin; paths over MAX_PATH are also deleted for real. | Paths of 248+ characters are refused; `FOF_NOCONFIRMATION` is dropped so the shell asks (an unattended call fails and the file stays). **Not run on Windows.** |
| 5 | Medium | `lib.Has` walks every external drive with no time limit; a hung network share stalls the run until the lease runs out. | Each folder gets 45 s (`ScanTimeout`); a slow one is left out and named in `Problems`. |
| 6 | Medium | A fixed release replaced the filed file even if the user had re-encoded, remuxed or added subtitles to it. | Not replaced when its modification time is after the filing time (+1 min); noskill's copies carry the download's older time. |
| 7 | Low | `sanitizeName` let through control characters, a leading dot (`.hack//SIGN` became a hidden folder), Windows device names (`CON`, `AUX`, `LPT1`…) and over-long names. | Fixed; empty becomes `_`; 200-byte cap on a character boundary. |
| 8 | Low | `download_dir` was used as a write and trash scope without the checks an approved folder gets. | `perms.ValidateApproved` on it. |
| 9 | Low | Linux: after `noskill undo` restored a file, its `.trashinfo` stayed, a broken entry in file managers. | `platform.TrashRestored` removes it. |
| 10 | Low | After a failed copy that followed a v2 trash, the report did not say where the old file was. | It does now ("…is in the trash; the fixed one is retried on the next run"). |

Checked and fine:
- **Every delete/rename call** in the table above reaches only noskill's own files (scratch folders from `MkdirTemp`, skill folders whose names are validated, its binary, the config swap with a `.prev` copy). `TestEveryDeleteAndRenameIsReviewed` fails when a call is added or removed without updating the reviewed list.
- **Symlinks:** `Canonical` resolves them, so `Copy`/`TrashPath`/`Mkdir` through a link that leaves the scope are denied (new test). Dangling links are refused.
- **Case-insensitive file systems:** `absent` uses `Lstat`, so `episode.mkv` over `Episode.mkv` is "exists" (new test, runs on macOS/Windows).
- **Look-alike drives:** `volume:` resolution refuses another drive with the same label once bound; an unbound label with two drives attached is refused on first use.
- **Failure order in the file stage** (new tests): copy fails → torrent and download stay, retried; state write fails → next run sees the file on disk and finishes the clean-up; torrent removal fails → nothing trashed, next run removes without copying again; trash fails → reported once, the file is still in Downloads, nothing lost; v2 copy fails → old file in the trash, fixed release retried.
- **Config edit** (`config set`): the new file is checked before it replaces the old, and the old is kept as `config.toml.prev`.

Accepted, with reasons (not fixed):
- **"Already have it" by title match** trashes the download even if the library file is partial or another resolution. The user's own file is never touched and the download is in the trash. A size sanity check could come later.
- **Extras go with the download:** a multi-file torrent's folder is trashed whole, so external subtitles (`.ass`) in it end up in the trash. Recoverable; copying them beside the episode is a feature to add.
- **No-replace rename window** (`absent` then `rename`, also after the free-space check): a program creating the same file in those milliseconds could be overwritten on Windows (and on Unix, `rename` replaces). `link` + remove would close it but fails on exFAT/FAT/SMB.
- **Unlabelled external drives** (no volume label) stay absolute paths; if such a drive is unmounted and its mount point is an empty folder the user owns, files would go to the internal disk beneath it. Labelled drives (`volume:`) refuse an unmounted drive.
- **Windows undo** can't restore from the Recycle Bin automatically (the shell call returns no path); `noskill undo` says to restore from the Recycle Bin.
- **Undo order edge:** if restoring a trashed source is skipped (the bin was emptied), undoing its cross-drive copy still trashes the only copy, to the trash.

Changed after the review (2026-10-18, by the user's request), same tests style: finished torrents are **stopped from seeding** before filing (no more waiting for a share limit), and the video is **renamed in the download folder before the copy** (a broker `Move` in the undo log). Consequences handled: a single-file torrent's own path is gone after the rename, so the renamed file is what gets trashed; a retry after a failed copy finds the renamed file; a taken name only skips the rename. The fake trash in the tests now never reuses a name, like the real ones.

**Found by the first live filing (2026-10-18):** the library scan includes `~/Downloads`, so a finished download looked like "already have it" (the file being filed was its own library copy). Filing would have counted it as stored, removed the torrent and trashed the download **without copying it**. Files under the torrent's save path no longer count as the library (`TestFileInDownloadsIsNotMistakenForTheLibraryCopy`). Six episodes (about 2.2 GB) were then filed for real into `~/Movies/Anime`, with no leftover `.part` files and nothing left in Downloads.

Not run, so unverified: the Linux trash on a real desktop (XDG, an external drive's `.Trash-<uid>`), the Windows Recycle Bin call, and everything on a real network share.

## Decision

**Fix first: done. Pass for the week-long run on macOS.** Findings 1 and 2 would have been serious in an unattended week (Downloads trashed with no undo id). For Windows and Linux, keep the anime filing and any trash-using job off until one real trash and one real `noskill undo` have been run on each; do that in the block 3 Windows/Linux pass.

## Windows and Linux gate (added 2026-10-18)
The review asks for one real trash and one real `noskill undo` on each OS before the week run is repeated there. `noskill doctor --files` does both in a folder of noskill's own and prints one line per step (it passed on macOS with the real Trash). On Windows expect one warning: the Recycle Bin doesn't say where it put a file, so noskill can't restore a trashed file there (restore it by hand); the rest must be ok. Paste the `files:` lines into the log table in [WEEK_RUN.md](WEEK_RUN.md).
