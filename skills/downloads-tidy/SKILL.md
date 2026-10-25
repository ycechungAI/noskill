---
name: downloads-tidy
description: Sort the loose files in Downloads (or another folder) into folders by type (Documents, Images, Installers, Archives and so on) and trash old installers. Use when the user says tidy, clean up, sort or organise Downloads or Desktop, or that the folder is a mess.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [plan]
  min_model: { context: 4096, tools: any }
  schedule_default: "0 10 * * 0"
  permissions:
    filesystem:
      - "read:$DOWNLOADS"
      - "write:$DOWNLOADS"
    exec: false
  confirm: [tidy_apply]
  tools:
    - builtin: tidy_plan      # { folder?, only?, min_age_days? } → counts per folder, examples, left_alone; changes nothing
    - builtin: tidy_apply     # { folder?, only?, min_age_days? } → plans again and carries it out (asks first)
---

# Downloads tidy

The plan is made in Go: you call one tool, show the result and apply it.

## Steps
1. `tidy_plan` (add `folder` if the user named another one, `only` if they want just some kinds).
2. Tell the user in two or three lines what it found: how many files go to which folder (`summary`), a couple of `examples`, how many old installers go to the trash, and that `left_alone` files stay.
3. `tidy_apply` with the same arguments. It asks the user first, then moves the files and trashes the old installers.
4. Report what moved and was skipped. Say that `noskill undo <run-id>` puts the moves back and that trashed installers are in the OS trash.

## Rules
- Never move or trash files any other way; `tidy_apply` is the only tool that changes anything.
- Folders and recent files are left alone on purpose. Videos wait two weeks because the anime tracker downloads to this folder and files them itself.

<!--
=== Implementation spec ===
Logic: internal/chores/tidy.go (PlanTidy: categories by extension; skips folders, links, hidden files, partial
downloads, files touched in the last 24 h, videos newer than 14 days, anything with no obvious category; installers
older than 30 days go to the trash). Tools: internal/tools/builtin/chores.go (tidy_plan, tidy_apply). tidy_apply plans the folder again at the moment of
applying and carries out that plan (moves through the permission broker, so they are logged for `noskill undo`;
installers to the OS trash), so the model can't pass or alter a plan. It does not depend on the files skill: a first
live run with granite4:3b used the files skill's ungated `trash` instead of the gated tool.
Settings ([skills.downloads-tidy]): folder (default "Sorted"), allow (extra folders).
Tests: internal/chores TestPlanTidy; builtin tests.
-->
