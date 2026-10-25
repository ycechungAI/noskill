---
name: media-renamer
description: Give the episodes in a folder one clean naming pattern, such as "Show Episode 12 [480p].mkv", reading the show and episode number from messy file names (release groups, brackets, dots). Use when the user asks to rename, clean up or fix the names of episodes or videos in a folder.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan]
  min_model: { context: 4096, tools: any }
  permissions:
    filesystem:                  # a drive needs --allow read:volume:RED and write:volume:RED (or [skills.media-renamer] allow)
      - "read:$VIDEOS"
      - "write:$VIDEOS"
      - "read:$DOWNLOADS"
      - "write:$DOWNLOADS"
    exec: false
  confirm: [rename_apply]
  tools:
    - builtin: rename_plan    # { folder, show?, style? } → count, examples, left_alone; changes nothing
    - builtin: rename_apply   # { folder, show?, style? } → plans again and carries it out (asks first)
---

# Media renamer

The names are worked out in Go with the anime tracker's own reader, so renamed files match the ones the tracker files.

## Steps
1. `rename_plan` with the folder. If the user gave the show's name, pass it as `show`; otherwise it is read from each file. `style` is `episode` ("Show Episode 3", the default), `ep` ("Show EP 3") or `sxxexx` ("Show S01E03").
2. Show the user the first `examples` (old name, new name) and say how many files change (`renames`) and how many are left alone (`left_alone` says why: can't tell the episode, several episodes in one file, and so on).
3. Then call `rename_apply` with the same arguments right away. Don't ask the user in words first: the tool asks them, and nothing is renamed until they say yes.
4. Say how many were renamed and that `noskill undo <run-id>` puts the old names back.

## Rules
- Only `rename_apply` changes names; don't rename any other way.
- Renames stay inside the folder. A name that would collide is skipped.
- If the show's name in the files looks wrong, ask the user for the right one and run the plan again with `show`.

<!--
=== Implementation spec ===
Logic: internal/chores/rename.go (PlanRenames: anime.ParseRelease for show, season, episode and resolution; anime.Naming
for the pattern, so the library and the tracker agree; batches, unreadable names and files already named that way are kept
with a reason). Tools: internal/tools/builtin/chores.go (rename_plan, rename_apply: plans again when applying; renames go through the
permission broker, so they are logged for `noskill undo`).
Tests: internal/chores TestPlanRenames (a second pass changes nothing).
-->
