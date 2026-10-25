---
name: duplicate-finder
description: Find duplicate files (same content, any name) in folders or on a drive, show how much space they waste, and trash the extra copies after the user approves. Use when the user asks about duplicates, copies, wasted space or "do I have this twice".
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan]
  min_model: { context: 4096, tools: any }
  permissions:
    filesystem:                  # a drive needs --allow read:volume:RED and write:volume:RED (or [skills.duplicate-finder] allow)
      - "read:$DOWNLOADS"
      - "write:$DOWNLOADS"
      - "read:$DOCUMENTS"
      - "write:$DOCUMENTS"
      - "read:$DESKTOP"
      - "write:$DESKTOP"
      - "read:$PICTURES"
      - "write:$PICTURES"
      - "read:$VIDEOS"
      - "write:$VIDEOS"
    exec: false
  confirm: [dupes_trash]
  tools:
    - builtin: dupes_find     # { folders, min_mb? } → groups { keep, files }, space_wasted; changes nothing
    - builtin: dupes_trash    # { keep, paths } → the extra copies to the OS trash, after checking they match keep
---

# Duplicate finder

## Steps
1. `dupes_find` with the folders the user named (one or several). A drive is `volume:<label>`.
2. Tell the user how many groups and how much space (`space_wasted`). Show the three biggest groups: the file name, the size and which copy is kept (`keep`).
3. Ask which to remove: all the extra copies, or only some groups.
4. For each chosen group call `dupes_trash` with `keep` and the other paths in that group. It checks again that they match and moves them to the trash.
5. Say how much space is freed once the trash is emptied.

## Rules
- Never trash the `keep` path, and never a path that isn't in a group `dupes_find` returned.
- If `note` says the scan was cut short, say so: the biggest files were checked first.
- Nothing is deleted for good. Say that the files are in the trash.

<!--
=== Implementation spec ===
Logic: internal/chores/dupes.go (FindDuplicates: size groups, then a hash of the first and last 64 KB, then the whole
file; skips symlinks, hidden folders, app bundles, hard links; bounded by a file count and a time budget, biggest first;
DupeKeeper picks the copy outside Downloads/Desktop/temp, not named like a copy, then oldest; SameContent re-checks at
trash time). Tools: internal/tools/builtin/chores.go (dupes_find, dupes_trash). dupes_trash needs the write scope on the
copy and the confirm approval.
Tests: internal/chores TestFindDuplicates.
-->
