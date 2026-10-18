---
name: watcher
description: Watch web pages, RSS feeds, GitHub releases or search results on a schedule and tell the user when something new or relevant appears. Use when the user says "tell me when", "keep an eye on", "watch for", or wants a regular digest on a topic.
license: MIT
noskill:
  version: 0.2.0
  origin: builtin
  status: active
  task_types: [research, extract]
  min_model: { context: 4096, tools: any }
  depends_on: [web-search]
  schedule_default: "0 8 * * *"
  permissions:
    network: ["*"]
    filesystem: []
    exec: false
  tools:
    - builtin: watch_add      # { name, kind: page|rss|github_releases|search, target, topic?, schedule? }
    - builtin: watch_list
    - builtin: watch_check    # { name } → new items since last check (diffed, deduped)
    - builtin: watch_remove
---

# Watcher

## Set up a watch
1. Work out what to watch:
   - a specific page → `page`
   - a feed URL → `rss`
   - a GitHub project's releases → `github_releases` (target `owner/repo`)
   - a topic with no fixed source → `search` (target = search query)
2. Call `watch_add` with a short name, the topic in the user's words, and a schedule (default: daily 08:00).
3. Confirm what will be watched and when.

## Scheduled check
1. `watch_check` for the watch. It returns only items that are new since the last check, already filtered by the decision model for relevance to the topic (`not_relevant` says how many were dropped). The first check only records what is there (`baseline`).
2. Decide which new items matter enough to tell the user right away and which can wait for a digest: breaking changes, security fixes and releases are right away; the rest is a digest.
3. Each item gets one line: what's new and the link.
4. If `new` is empty, say nothing happened. Don't invent news.

## Rules
- Only report items that are new since the last check.
- If a page changes only in ads, dates or counters, it's not new.

<!--
=== Implementation spec ===
Implemented (Block 3): internal/watch (parsing and diffing, no I/O), internal/store/watch.go
(migration 0008: watches, watch_items), internal/tools/builtin/watch.go (watch_add, watch_list,
watch_check, watch_remove), internal/app/watch.go (each watch is a scheduler job "watch: <name>").
Deviations from the design below:
  - The relevance filter runs inside watch_check (decision model, else the chat model; threshold 0.6),
    so a 4B driver never judges relevance one item at a time. The "importance" split is left to the
    driver model: the skill-level `decisions:` block isn't wired to a tool yet.
  - Approve/ignore reactions don't tune thresholds yet; calibration (FR-B6c) covers router and judge only.
  - A watch's job notifies through the "os" channel; change it with `noskill schedule` like any job.
- This generalises the "scheduled search" pattern; anime-tracker is a specialised watcher.
- Storage: watches table + per-watch item fingerprints (URL + content hash of readable text).
- page: fetch → readability text → normalise (strip dates/numbers in boilerplate) → diff by paragraph.
- rss: ETag/Last-Modified; github_releases: GitHub REST releases API (unauthenticated, cached; optional token).
- search: web-search provider chain; new = URL not seen before for this watch.
- Each watch is a scheduler job (FR-J1); `noskill schedule list` shows them.
- Learning: approve/ignore reactions to notifications tune the relevant/importance thresholds per watch.
Tests: fixture pages with cosmetic-only changes → no alert; new release → alert; dedupe across runs.
-->
