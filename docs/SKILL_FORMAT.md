# Skill format

noskill skills are **agentskills.io-compatible**: a folder with a `SKILL.md` whose YAML front-matter has at least `name` and `description`. Any skill that works in Claude Code, Codex, Hermes or another compatible runtime should load here unchanged. `noskill skills import` finds such skills in those runtimes' folders (`~/.claude/skills`, plugin `skills/` folders, `~/.codex/skills` or `$CODEX_HOME`, `~/.hermes/skills` or `$HERMES_HOME`, `~/.agents/skills`, and the project's `.claude`, `.codex` and `.agents` folders) and copies one as it is, after telling you what won't carry over: a skill without a `noskill:` block declares no permissions, so it gets none; Claude Code's `allowed-tools`, `model`, `context: fork`, hooks, `$ARGUMENTS` and `!`command`` lines are not honoured.

noskill adds **optional** fields under a `noskill:` key. Other runtimes ignore them; noskill uses them for tools, permissions, scheduling, routing hints and learning.

## Folder layout

```
my-skill/
  SKILL.md              # required
  tools/                # optional: tool definitions + code
    search.tool.yaml
    search.py
  references/           # optional: docs loaded on demand
  templates/            # optional
  tests/                # optional but required for learned skills
    cases.yaml
  CHANGELOG.md          # auto-maintained for learned skills
  .noskill-sig.json     # optional: signature over every file (FR-S10, ADR-0016)
```

## SKILL.md

```markdown
---
name: web-search
description: Search the web and read pages. Use when the user asks about current events, facts you are unsure of, or anything that needs a source.
license: MIT
noskill:
  version: 1.2.0
  origin: builtin            # builtin | user | learned | imported
  status: active             # draft | candidate | active | stale | archived
  triggers:                  # extra phrases for embedding retrieval
    - "look up"
    - "latest news on"
  task_types: [research, chat]
  min_model: { context: 4096, tools: any }   # any | native | none
  budget: { steps: 160, tokens: 400000 }     # optional: a skill that needs a long run asks for more than the default 25 steps / 60k tokens (at most 200 steps, 500k tokens); the user's --budget-steps and --budget-tokens win
  prefer_models: []          # routing hints, e.g. ["qwen3:*"]
  permissions:
    network: ["*"]           # domains; "*" = any (needs user approval at install)
    filesystem: []           # e.g. ["read:~/Documents", "write:volume:RED/ANIME2026"]
    exec: false
    subprocess: false        # scripts may start child processes (uv installs, build tools); off by default (ADR-0008)
    secrets: ["BRAVE_API_KEY?"]  # a trailing ? = optional; quote it, YAML rejects a bare "NAME?" in a [list]
  confirm: []                # tool names that require approval before running
                             # filesystem may use "write:${approved.dest}": folders the user picked in this skill's approvals
  tools:
    - tools/search.tool.yaml
    - tools/fetch.tool.yaml
  depends_on: []             # other skills
  schedule_default: null     # e.g. "0 3 * * *"
  decisions:                 # named question sets for the `decide` tool (JEV models or emulated)
    reliable_source:
      noul: "Is this page a primary or authoritative source for the question?"
      threshold: 0.6         # used by code/tests; the model sees only the answer
---

# Web search

## When to use
...

## Steps
1. Turn the question into 1–3 short queries.
2. Call `search`. Pick the 3 most relevant results.
3. Call `fetch` on each. ...

## Output
Answer, then a "Sources" list with URLs.

## Pitfalls
- ...
```

**Writing for small models:** the body should be short, numbered and imperative. Long prose hurts 3B–8B models. Put detail in `references/` and tell the model when to read it.

## Tool definition (`*.tool.yaml`)

```yaml
name: search
description: Web search. Returns title, url, snippet for each result.
runtime: python            # builtin | python | node | bun | sh | pwsh | wasm
entry: search.py           # or builtin id, or wasm module
deps: ["httpx==0.27.*"]    # python via uv; node via package.json
timeout: 20s
max_output: 16kb
input_schema:
  type: object
  required: [query]
  properties:
    query: { type: string }
    max_results: { type: integer, default: 5, maximum: 10 }
output_schema:
  type: array
  items: { type: object, properties: { title: {type: string}, url: {type: string}, snippet: {type: string} } }
os: [darwin, linux, windows]   # optional restriction; per-OS entry allowed: entry: { windows: x.ps1, default: x.sh }
```

Protocol for script tools: args JSON on **stdin**, result JSON on **stdout**, logs on **stderr**, non-zero exit = error. Same on every OS.

**As built (block 2b): `runtime: wasm`.** `entry` is a `.wasm` file in the skill (WASI preview 1, for example Go's `GOOS=wasip1`, Rust's `wasm32-wasip1`, TinyGo or C). It uses the script protocol. It runs under wazero, with the files given as `format: path` arguments at `in/<name>` (relative paths resolve from `/`), anything it writes to `out/` saved through `{"outputs": [{"from", "to"}]}` and the broker as for scripts, and `/tmp`. It can't reach the network or any other file. A skill's `network` permission doesn't apply to a wasm tool yet.

## Decisions (`noskill.decisions`)

Named question sets that the skill's steps or tools call with `decide(<name>, state)`. Each question is one of:

| Key | JEV type | Answer |
|---|---|---|
| `choice: {instructions, criteria: {label: description}}` | `choice` | label + probabilities |
| `noul: "<yes/no question>"` | `noul` | probability of yes (0–1) |
| `score: {instructions, criteria: [low … high]}` | `score` | number + legend + probabilities |

A set can hold several questions; they're sent in one request. Served by the resident JEV decision model, or emulated by the chat model (see [MODEL_ADAPTERS.md](MODEL_ADAPTERS.md#jev-decision-models)). Decisions can be tested like tools (`decision: reliable_source`, `state: {...}`, `expect: {noul_gte: 0.6}`).

## Tests (`tests/cases.yaml`)

```yaml
- name: finds ollama release notes
  input: "latest ollama release"
  expect:
    tool_called: search
    output_contains_any: ["github.com/ollama/ollama/releases", "ollama.com/blog"]
  network: live          # live | recorded (uses tests/fixtures/*.har)
- name: handles empty results
  tool: search
  args: { query: "zzqxv-no-results-zzqxv" }
  expect: { output_schema_valid: true }
```

Test kinds: **tool-level** (call a tool with args, check output) and **skill-level** (run the agent loop with a prompt, check tool usage and output). Recorded fixtures make tests deterministic and offline.

**As built (block 2a): skill-level replay cases.** The learning loop writes, and the verification gate reads, this form:

```yaml
- name: replay of run 01JB2X9K...
  input: "sort my downloads into Documents/PDFs"
  network: recorded
  fixtures: fixtures/replay.json         # under tests/: [{tool, args, result}], served by tool name in order
  expect:
    tools: [list_dir, move_many]         # called in this order; other calls may come between
    args_contain: ["~/Downloads"]        # each appears in some call's arguments
    answered: true                       # the run ends with an answer
- name: failed-01JB2X9K
  required: true                         # must pass on every new version
  ...
```

The gate passes when the first case passes, every `required` case passes, and at least half of the others pass ([LEARNING.md](LEARNING.md#as-built-in-block-2a)). A refinement adds each failing run as a required case. A merge adds the duplicate's replay as one. Tool-level cases aren't run yet.

## Status lifecycle

```
draft ──verify pass──▶ candidate ──A/B win or user approves──▶ active
  ▲                        │                                     │
  └──── verify fail ◀──────┘                      unused 30d ──▶ stale ──90d──▶ archived
```

Built-in and user skills start `active` and are never auto-changed. The learning loop forks them into `learned/` instead.

## Compatibility notes
- Unknown front-matter is preserved on write.
- Claude-style skills with `scripts/` and no `noskill.tools` still work: their scripts are offered as generic `run_script` calls under `exec` permission (prompted).
- Exporting: `noskill skills export <name> --plain` strips the `noskill:` block.
- `.noskill-sig.json` is a dotfile other runtimes ignore. It holds an ed25519 signature over a manifest (`noskill-skill-v1`, the name, the version, then `sha256  path` for every file but `.git` and itself), the file list, the key ID and the public key. `noskill skills sign` writes it; a folder with a symlink in it can't be signed ([ADR-0016](adr/0016-signed-skill-registry.md)).

## Details added while building the loader and runtime (block 1b)
- **Quote values with `: ` or a trailing `?`.** `description: Work with PDFs: split` and `secrets: [KEY?]` are invalid YAML; write `description: "Work with PDFs: split"` and `secrets: ["KEY?"]`. The loader says so in its error.
- **Inline builtin tools:** `tools: [{ builtin: list_dir }]` selects a tool implemented in Go. Its description and schema come from the Go registry.
- **Implementation notes** in HTML comments (`<!-- … -->`) are stripped before the model sees the skill.
- **Script tools** get `format: path` string arguments copied into `work/in/` and receive the copy's relative path. A script can leave files with `{"outputs": [{"from": "out/x.md", "to": "~/Documents/x.md"}]}`: each destination is checked against the skill's write scopes, never overwritten, and logged for undo.
- **Library skills** (`files`) declare no folders: they get the scopes of the skill that brought them in (`depends_on`), or what you grant with `--allow` or `[skills.<name>] allow = "write:~/Downloads"`.
- **`confirm`** gates a tool; a call with `dry_run: true` needs no approval.
- **Project skills** (`./.noskill/skills`) load but stay disabled until you run `noskill skills enable`; changing their files withdraws that.

## File ops from a script
Added in block 2d ([ADR-0013](adr/0013-script-file-ops-through-the-broker.md)). A script tool sees read-only copies of its `format: path` inputs, so it can't move, trash or change them itself. Instead it prints the changes it wants, and the broker applies each one to the real files:

```json
{"ops": [{"op": "make_dir", "path": "in/docs/old"},
         {"op": "move",  "from": "in/docs/a.txt", "to": "in/docs/old/a.txt"},
         {"op": "copy",  "from": "in/docs/b.md",  "to": "in/docs/backup/b.md"},
         {"op": "write", "path": "in/docs/all.txt", "text": "…"},
         {"op": "trash", "path": "in/docs/c.tmp"}],
 "count": 3}
```

- Paths name the copies (`in/<input>/…`, with or without a leading `/` or `./`), and map back to the real file behind that input. A path outside `in/<input>`, or one that leaves it with `..`, fails.
- Each op is checked against the skill's permissions:
  - `move` and `trash` need write access to the source; `copy` needs read access.
  - Every destination needs write access.
  - Nothing is overwritten. A target's parent folder is created first.
  - `trash` uses the OS trash, and every effect goes to the undo log.
- At most 500 ops per call, and at most 4 MB of UTF-8 per `write`.
- The result passes on any other fields the script printed. It adds `done` (short descriptions such as `move docs/a.txt → docs/old/a.txt`) and `failed` for ops that didn't apply.
- With `"dry_run": true` in the call's arguments, nothing is applied. The result lists `would_do` and has `dry_run: true`, so a `confirm:` tool can be previewed without approval.
- `{"outputs": [...]}` for files left in `out/` works as before, when there's no `"ops"`.

Learned skills from [pass d](LEARNING.md#pass-d-script-tools) have one such tool: `tools/<name>.tool.yaml` (`runtime: python`, `entry: <name>.py`, a required `folder` argument with `format: path`, and up to 4 string arguments) plus `tools/<name>.py`.
