# ADR-0013: Scripts change files by asking the broker

Status: accepted (block 2d). Extends the script protocol in [ADR-0008](0008-macos-script-sandbox.md) and [SECURITY.md](../SECURITY.md) "Script tools".

## Context
Learning pass d ([LEARNING.md](../LEARNING.md#pass-d-script-tools)) turns a repeated file chore into one script tool call. That cuts a 5-turn run (list, make the folder, dry run, apply, answer) to 2 turns. The learned file chores move, copy, write and trash files in a folder the user named.

A script tool, though, sees only read-only copies of its inputs in `work/in/`. It could already leave new files in `out/` for the broker to copy out (`{"outputs": [...]}`), but it couldn't move or trash anything. Giving the sandbox write access to the real folder would mean:
- no undo log
- no OS trash (a script's `os.remove` is a hard delete)
- no overwrite check
- no per-path permission check

All four are rules of the project (no hard deletes, undo for every effect, FR-S4).

## Decision
A script describes the changes it wants and noskill applies them.
- The script prints `{"ops": [...]}` with `make_dir`, `move`, `copy`, `write` and `trash` ops on paths under its input copies (`in/<input>/…`).
- The executor maps each path back to the real file behind that input, refusing anything outside it.
- It applies each op through the permission broker, as the built-in file tools do:
  - write scopes are checked per path
  - nothing is overwritten
  - trash means the OS trash
  - every effect goes to the undo log
- A call with `dry_run: true` applies nothing and returns `would_do`, so `confirm:` tools can be previewed the usual way.
- The format is in [SKILL_FORMAT.md](../SKILL_FORMAT.md#file-ops-from-a-script).

The author model replies to pass d in plain text (header lines and a fenced code block), not schema-constrained JSON: inside a JSON string, the 4B author looped until its tokens ran out on every try. Values the model writes into the code anyway are lifted into arguments by the parser when they are exactly a request value, and code that opens files for writing is refused before it runs.

Pass d checks a learned script against its source run on noskill's own rebuilt copy of the folder. The code must print the same ops as the run's file effects and hold no values from the run. The script runs in the macOS script sandbox only. Where no sandbox enforces file access, pass d is skipped and the skill keeps plain steps. The static check accepts a learned skill's own tool only when it is one such Python tool with no packages.

## Consequences
- Learned file chores take one call, and keep the same safety as the built-in tools (trash, undo, no overwrite).
- The script reads a copy, so a large folder is copied in before each call: clones where the filesystem supports them (APFS), plain copies elsewhere.
- Ops are applied one at a time, and a failed op doesn't stop the rest. The result lists `failed` ops for the model to report, and undo covers the ones that applied.
- Pass d needs the macOS sandbox, so on Linux and Windows learned skills stay as plain steps until those sandboxes exist.
- A draft's sandboxed tryout leaves its workspace under `runs/draft/`, which is removed after the check. That workspace is noskill's own scratch, not user files.
