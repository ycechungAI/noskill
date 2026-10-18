---
name: files
description: List, read, write, move, rename and organise files and folders, including on external drives. Use when the user asks to sort, rename, find or store files.
license: MIT
noskill:
  version: 0.2.0
  origin: builtin
  status: active
  task_types: [extract, plan]
  min_model: { context: 4096, tools: any }
  permissions:
    filesystem: []          # granted per call site: skills that depend on `files` pass their own scopes
    exec: false
  confirm: [move_many]       # a dry_run call needs no approval
  tools:
    - builtin: list_dir       # { path, glob?, recursive? } → entries with size, mtime
    - builtin: read_text      # { path, max_chars? }
    - builtin: write_text     # { path, content }: creates a new file, never replaces one
    - builtin: join_files     # { from: [paths], to }: a new text file made of the files in order (a report from its sections); never replaces one
    - builtin: make_dir       # { path }: creates the folder and any missing parents inside the allowed scope
    - builtin: move           # { from, to }  (rename = move in same dir)
    - builtin: move_many      # { plan: [{from,to}] } dry-run first, then apply
    - builtin: copy_verify    # { from, to } copy to <to>.part, verify hash, rename to <to>; never overwrites; source kept
    - builtin: trash          # moves to OS trash, never hard delete
    - builtin: volumes        # mounted volumes: label, uuid, mount path, free bytes, filesystem
    - builtin: sanitize_name  # { name, target_fs } → safe filename
---

# Files

## Steps for organising or renaming many files
1. `list_dir` to see what's there.
2. Build a plan: a list of `{from, to}` moves. Use `sanitize_name` for every new name.
3. Call `move_many` with `dry_run: true`. Show the user the plan if running interactively.
4. Apply with `move_many`. Report what moved and anything skipped.

## Rules
- Never delete. Use `trash` only if asked.
- Never overwrite an existing file: pick another name. (There is no way to replace a file.)
- Use path tokens: `~`, `$DOWNLOADS`, `$VIDEOS`, `volume:<label>`.

<!--
=== Implementation spec ===
- All paths canonicalised; checked against the *calling* skill's permission scopes; symlink escape blocked.
- move across filesystems = copy + verify (size + xxhash) + remove source; resumable; progress events.
- volumes(): macOS diskutil / Linux lsblk / Windows GetVolumeInformationW; see docs/PLATFORMS.md.
- Free-space reserve check before copy (default 5%); FAT32 4 GB limit check.
- copy_verify: used by anime-tracker's filing loop (copy → verify → rename, then the caller trashes the source). A leftover `.part` from an interrupted copy is replaced on retry; the source is untouched until the caller trashes it.
- Every applied move_many writes an undo log → `noskill undo <run-id>`.
-->
