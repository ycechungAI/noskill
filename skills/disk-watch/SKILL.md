---
name: disk-watch
description: Report free space on every drive and what takes the space in a folder or drive, and warn when a drive runs low. Use when the user asks how much space is left, what is filling a drive, or what is big and old, or wants a low-space alert.
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [extract]
  min_model: { context: 4096, tools: any }
  schedule_default: "30 6 * * *"
  permissions:
    filesystem: []             # a folder or drive to look inside needs --allow read:<path> (or [skills.disk-watch] allow)
    exec: false
  tools:
    - builtin: disk_report     # { path?, idle_days? } → volumes (free, low?), biggest_folders, large_idle_files
---

# Disk watch

## Steps
1. `disk_report`. Add `path` when the user names a folder or drive and wants to know what is filling it.
2. Say which drives are low (`low` is true; `why` says how). Then list the others in one line each: name, free space.
3. With a `path`, name the three biggest folders and the three biggest files nobody has opened in a year, with sizes.
4. Suggest, don't do: "the old files could go to the trash" is for the user to decide.

## Rules
- Read-only. Never move or delete anything from this skill.
- Report the numbers the tool gave you; don't estimate.

## Alerts
The nightly alert is a model-free job: `noskill schedule add diskwatch --cron "30 6 * * *"`. It stays quiet while every drive has enough room and sends one notification when one doesn't. Limits are in `[skills.disk-watch]`: `min_free_gb` (default 20), `min_free_percent` (default 5), `volumes` (a list of labels; default all).

<!--
=== Implementation spec ===
Logic: internal/chores/disk.go (CheckVolumes against the limits, Usage: biggest folders and large idle files under a
path, time-boxed; Alert text). Job kind "diskwatch" (internal/app/chores.go, scheduler Result.Quiet: no notification on
a good night). Uses platform.ListVolumes, so it works on every OS.
Tests: internal/chores TestCheckVolumesAndAlert, TestUsageFindsBigFoldersAndIdleFiles.
-->
