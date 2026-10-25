---
name: daily-brief
description: Say what happened on this computer overnight (jobs that ran or failed, episodes filed, approvals waiting, low disk space, what runs next) and set reminders from plain sentences like "remind me to call Sam tomorrow at 5pm" or "every Monday at 9 submit the report". Use for "what did I miss", "morning brief", or any "remind me".
license: MIT
noskill:
  version: 0.1.0
  origin: builtin
  status: active
  task_types: [extract, plan]
  min_model: { context: 4096, tools: any }
  schedule_default: "0 7 * * *"
  permissions:
    filesystem: []
    exec: false
  tools:
    - builtin: brief_build      # { hours? } → the digest
    - builtin: reminder_add     # { text } → what was understood, or what to ask
    - builtin: reminder_list
    - builtin: reminder_remove  # { name }
---

# Daily brief and reminders

## Brief
1. `brief_build` (add `hours` if the user said how far back).
2. Give the user the digest as returned, in your own short words. Don't add anything it didn't say.

## Reminder
1. `reminder_add` with the user's own sentence as `text`.
2. Repeat back what it understood: the reminder and `when`. If the tool says it couldn't tell when or what, ask the user that one thing and try again.
3. To list or remove: `reminder_list`, then `reminder_remove` with the name.

## Rules
- Never guess a time yourself. The tool reads the sentence; if it refuses, ask.
- A one-off reminder switches itself off after it fires.

## Scheduled
The morning brief and each reminder are model-free jobs: `noskill schedule add brief --cron "0 7 * * *"`, `noskill remind "call Sam tomorrow at 5pm"`. Both send a notification when they fire.

<!--
=== Implementation spec ===
Logic: internal/chores/brief.go (BriefFacts.Text: the digest is built in code), internal/chores/remind.go (ParseReminder:
every N minutes/hours, every day/weekday/weekend/<weekday>, every month on the Nth, in N minutes/hours/days, today/tonight/
tomorrow, on <weekday>, at <time>; times as 5pm, 17:30, at nine, noon; a bare 1-6 is the afternoon; it refuses rather than
guesses and says what it understood). Jobs: kinds "remind" (Args.Once switches it off after it fires), "brief" and
"diskwatch" run in Go (internal/app/chores.go); `noskill remind` and `noskill brief` are the CLI. Tools:
internal/tools/builtin/chores.go via ChoresService (runServices).
Tests: internal/chores TestParseReminder, TestParseReminderAsksInsteadOfGuessing, TestBriefText.
-->
