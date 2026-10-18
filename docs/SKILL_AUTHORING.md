# Skill authoring guide

How to write a noskill skill: a folder with a `SKILL.md` that tells the model what to do, plus optional tools, references and tests. Skills are **agentskills.io-compatible** — if a skill loads in Claude Code, Codex or Hermes, it loads here unchanged.

This guide covers the format as built (through block 5c). For the full field reference see [SKILL_FORMAT.md](SKILL_FORMAT.md); for the learning loop that creates, improves and retires skills see [LEARNING.md](LEARNING.md).

**In a hurry:** [section 1](#1-the-simplest-possible-skill) is a whole skill. Sections 3–4 are what decides whether a 4B model picks it and follows it. [Section 15](#15-trying-and-fixing-a-skill) is how to try it and see why it went wrong.

---

## 1. The simplest possible skill

```
my-skill/
  SKILL.md
```

```markdown
---
name: hello-world
description: Say hello. Use when the user says hello or asks how you are.
---

# Hello world

## Steps
1. Say hello back, briefly.

## Rules
- One short sentence. Don't ask questions.
```

That's it. noskill finds it in `~/.noskill/skills` (or `./.noskill/skills` in a project, which needs your OK the first time), the selector surfaces it when the description matches, and the agent loop runs it. No tools, no permissions, no tests. `noskill skills list` shows it; `noskill run --skill hello-world "hi"` forces it for one run.

---

## 2. Anatomy of a skill

```
my-skill/
  SKILL.md              # required — instructions + metadata
  tools/                # optional — tool definitions + code
    search.tool.yaml
    search.py
  references/           # optional — loaded on demand, not at start
    providers.md
  templates/            # optional
  tests/                # optional but required for learned skills
    cases.yaml
    fixtures/
      replay.json
  CHANGELOG.md          # auto-maintained for learned skills
```

### What the model sees vs. what you keep for yourself

`SKILL.md` has two layers:

1. **Front-matter + body** — what the model sees. Keep it short, numbered, imperative.
2. **HTML comments** (`<!-- ... -->`) — implementation notes, stripped before the model sees them. Use this for tool specs, provider chains, test notes — anything a human or the skill-author needs but a 4B model would just be confused by.

---

## 3. Writing the front-matter

Every field under `noskill:` is optional. Other runtimes ignore the whole block; noskill uses it for tools, permissions, scheduling, routing and learning.

```yaml
---
name: web-search
description: Search the internet and read web pages. Use when the question is about current events, prices, releases, or any fact you are not sure of, or when the user asks for sources.
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
  prefer_models: []          # routing hints, e.g. ["qwen3:*"]
  permissions:
    network: ["*"]           # domains; "*" = any (flagged at install)
    filesystem: []           # e.g. ["read:~/Documents", "write:volume:RED/ANIME2026"]
    exec: false
    subprocess: false        # scripts may start child processes; off by default
    secrets: ["BRAVE_API_KEY?"]  # trailing ? = optional; quote it (YAML)
  confirm: []                # tool names that require approval before running
  tools:
    - tools/search.tool.yaml
    - tools/fetch.tool.yaml
  depends_on: []             # other skills
  schedule_default: null     # e.g. "0 3 * * *"
  decisions:                 # named question sets for the `decide` tool
    reliable_source:
      noul: "Is this page a primary or authoritative source for the question?"
      threshold: 0.6
---
```

### The fields that matter most

| Field | Why it matters |
|---|---|
| `description` | The primary retrieval signal. Write it as the user's question, not the function name. "Search the web and read pages. Use when…" not "A web search tool." |
| `triggers` | Backup for when no embedding model is installed. Keywords and phrases that should select this skill. |
| `permissions` | The security contract. Declare exactly what the skill needs — no more. Undeclared access is denied or prompts the user. |
| `confirm` | Tool names that pause for approval before running. Scheduled jobs never run these unattended; they become approvals. |

### Writing descriptions for small models

The selector retrieves by embedding similarity, then the model picks from a shortlist. Your description needs to:

- **Read like a goal, not a label.** `description: "Research a topic and write a cited report"`, not `description: "Research skill"`.
- **Include the trigger phrases.** The model sees only name + description at selection time. If the user asks "what changed in the latest Ollama release?", the description should contain language that matches.
- **Say when to use it.** "Use when the user asks about current events, prices, releases, or any fact you are not sure of."
- **Say when NOT to use it.** "Don't use for offline tasks or anything that needs files on this machine."

---

## 4. Writing the body

The body is the skill's instructions. It loads when the skill is selected — not at conversation start. That means it can be longer than the description, but it still goes to a possibly-small model.

### Rules for small models

1. **Numbered steps.** "1. Write 1–3 short search queries. 2. Call `search`." Not paragraphs.
2. **One idea per line.** If a line has two verbs, split it.
3. **Imperative voice.** "Call `search`", not "You should call `search`."
4. **Short.** The built-in skills' bodies run 15–40 lines. Put detail in `references/` and tell the model when to read it.
5. **Explicit about tool names.** Use backticks. The model matches the string to the tool registry.

### Template

```markdown
# <Skill name>

## When to use
<one or two sentences>

## Steps
1. <imperative verb> <what to do>
2. ...

## Output
<what the final answer should look like>

## Rules
- <constraint>
- <constraint>

## Pitfalls
- <known failure mode>
- <known failure mode>

<!--
=== Implementation spec (not shown to the model) ===

tool definitions, provider chains, test plans, anything a human needs
-->
```

### Progressive disclosure

Only name + description go into context at start. The full `SKILL.md` loads when selected. References and scripts load on demand. This means:

- **Keep the body tight.** If a 4B model can't follow it, it won't.
- **Put long-form info in `references/`.** "Read `references/providers.md` before choosing a search provider."
- **Don't inline large tables.** Move them to `references/` and load them when needed.

---

## 5. Tools

A skill without tools just gives the model instructions. Tools let it act.

### Built-in tools

Go-implemented, always available, no sandbox needed. Reference by name:

```yaml
tools:
  - builtin: list_dir
  - builtin: move_many
  - builtin: run_python
```

Built-in tools are listed in a skill's `tools:` as `builtin: <id>`. The ones that exist:

| Area | Tools |
|---|---|
| Files | `list_dir`, `read_text`, `write_text` (new files only), `move`, `move_many` (dry run first), `copy_verify`, `make_dir`, `trash`, `volumes`, `sanitize_name` |
| Code | `run_python` (sandboxed) |
| Models | `list_models`, `machine_info`, `suggest_models`, `pull_model`, `remove_model`, `bench_model`, `route`, `delegate`, `plan_questions`, `notes` |
| Docs | `docs_index`, `docs_search`, `docs_read` |
| Watching | `watch_add`, `watch_check`, `watch_list`, `watch_remove` |
| Anime | `anime_detect`, `anime_run`, `anime_status` |

Web search and page fetching are tools of the `web-search` skill (`tools/search.tool.yaml`, `tools/fetch.tool.yaml`, with `runtime: builtin` and `entry: web.search`). A skill that needs them lists `web-search` in `depends_on`.

Nothing deletes for good: `trash` moves to the OS trash, `write_text` and `move` refuse to replace a file, and every file change goes through the permission broker and the undo log ([SECURITY.md](SECURITY.md)).

### Script tools

```yaml
tools:
  - tools/search.tool.yaml
```

```yaml
name: search
description: Web search. Returns title, url, snippet for each result.
runtime: python            # builtin | python | node | bun | sh | pwsh | wasm
entry: search.py
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
os: [darwin, linux, windows]   # optional; per-OS entry: { windows: x.ps1, default: x.sh }
```

**Protocol for script tools:** args JSON on **stdin**, result JSON on **stdout**, logs on **stderr**, non-zero exit = error. Same on every OS.

**Sandbox:** script tools run in an OS sandbox with a copy-in workspace. They never see your real folder paths — they get `work/in/<file>` and write to `work/out/`. The broker checks outputs against the skill's `write` scopes and applies them through the undo log.

### Choosing between built-in and script

| Use built-in when… | Use script when… |
|---|---|
| The operation is file/web/process and already exists | You need custom logic (parsing, math, API orchestration) |
| You want it to work identically on every OS | You need a Python/Node library |
| You don't want to manage a runtime dependency | The operation is skill-specific |

---

## 6. Permissions

The security contract. Declare what the skill needs; the broker enforces it.

```yaml
permissions:
  network: ["graphql.anilist.co", "*.github.com"]   # domains, "localhost", or "*"
  filesystem:
    - "read:~/Documents"
    - "write:volume:RED/ANIME2026"
  exec: false                # run arbitrary host programs (red flag at install)
  subprocess: false          # sandboxed script may start child processes (uv, build tools)
  secrets: ["ANILIST_TOKEN?"] # trailing ? = optional; quote names ending in ?
  confirm: ["move_many"]     # tool names that pause for approval
```

### Tokens

| Token | Meaning |
|---|---|
| `read:<path>` | Read files under this path |
| `write:<path>` | Write, move-to-trash under this path |
| `volume:LABEL/PATH` | A mounted volume by label |
| `$DOWNLOADS`, `$DOCUMENTS` | OS folders |
| `${config.x}` | A config value |
| `${approved.field}` | A folder the user picked in this skill's approvals |

### Scope rule

Skills can **narrow** permissions, never widen them. If `anime-tracker` calls `files.move_many`, it passes only `write:volume:RED/ANIME2026` — even if `files` has broader scopes.

---

## 7. Decisions (JEV question sets)

Named question sets that the skill's steps or tools call with `decide(<name>, state)`. Served by the resident JEV decision model, or emulated by the chat model.

```yaml
decisions:
  reliable_source:
    noul: "Is this page a primary or authoritative source for the question?"
    threshold: 0.6
  same_series:
    choice:
      instructions: "Are these two titles the same series?"
      criteria:
        same: "Same series, different season or language"
        different: "Different series"
        ambiguous: "Could be either"
```

Three question types: `noul` (yes/no → probability), `choice` (label + probabilities), `score` (number + legend + probabilities).

---

## 8. Tests

Skills that are learned by the system **must** have tests before they activate. User-authored skills should have them too — they're the verification gate.

### Skill-level replay tests (what the learning loop produces)

```yaml
- name: replay of run 01JB2X9K...
  input: "sort my downloads into Documents/PDFs"
  network: recorded
  fixtures: fixtures/replay.json
  expect:
    tools: [list_dir, move_many]
    args_contain: ["~/Downloads"]
    answered: true
- name: failed-01JB2X9K
  required: true
  ...
```

### Tool-level tests

```yaml
- name: finds ollama release notes
  input: "latest ollama release"
  expect:
    tool_called: search
    output_contains_any: ["github.com/ollama/ollama/releases", "ollama.com/blog"]
  network: live
```

### Test kinds

| Kind | What it checks |
|---|---|
| **tool-level** | Call a tool with args, check output |
| **skill-level** | Run the agent loop with a prompt, check tool usage and final answer |

Recorded fixtures make tests deterministic and offline. The verification gate passes when: the first case passes, every `required` case passes, and at least half the others pass.

**Running your own:** `noskill skills test <name>` is not built yet. The gate runs a learned skill's tests when it is made or refined. For a skill you wrote, pin a run you were happy with as a checked task (`noskill eval pin last --id my-case`) and run the checked tasks with `noskill eval run`; the same tasks are what `noskill skills optimise` scores against.

---

## 9. The skill lifecycle

```
draft ──verify pass──▶ candidate ──A/B win or user approves──▶ active
  ▲                        │                                     │
  └──── verify fail ◀──────┘                      unused 30d ──▶ stale ──90d──▶ archived
```

Built-in and user skills start `active` and are never auto-changed. The learning loop forks them into `learned/` instead.

Two ways a skill gets better without you editing it:
- **Learned skills** are refined from their failures. Versions are kept: `noskill skills versions <name>`, `skills diff <name>`, `skills rollback <name>`.
- **Built-in and learned skills' instructions** can be rewritten against the checked tasks that name them: `noskill skills optimise <name>` (FR-L9, [ADR-0014](adr/0014-skill-optimisation.md)). It changes at most three lines a round, never a line that says what must or mustn't happen, and keeps a rewrite only if it also wins on tasks it wasn't trained on. A built-in's rewrite is stored as a tuned copy (`skills list` shows `builtin (tuned N)`) until you `adopt` it, and `skills optimise revert` puts the original back. A learned skill's rewrite goes through the usual trial. **Skills you wrote are never rewritten**; improve them by hand ([section 15](#15-trying-and-fixing-a-skill)). A skill needs at least 5 checked tasks that name it before it can be optimised.

### Statuses

| Status | Meaning |
|---|---|
| `draft` | Written, not yet verified |
| `candidate` | Passed verification, in trial (ask before use) |
| `active` | Verified and promoted; used automatically |
| `stale` | Unused for 30 days |
| `archived` | Unused for 90 days; kept but not offered |

---

## 10. Checklist for a new skill

- [ ] `SKILL.md` exists with `name` and `description`
- [ ] Description reads like a goal and includes trigger phrases
- [ ] Body is numbered, imperative, under ~40 lines
- [ ] Long-form info lives in `references/` with instructions to load it
- [ ] Tools declared (built-in by name, scripts with `.tool.yaml`)
- [ ] Permissions declared — exactly what the skill needs, no more
- [ ] `confirm` set on any destructive or hard-to-undo tool
- [ ] Implementation notes in HTML comments, not in the body
- [ ] Tests written (required for learned skills, recommended for all)
- [ ] Loads with `noskill skills list`, and a run with `noskill run --skill <name> "…"` does what you meant
- [ ] Tried on a small model (`--model qwen3:4b`), not just a big one
- [ ] Shared skills: signed with `noskill skills sign` ([section 12](#12-sharing-importing-and-signing))

---

## 11. Examples

| Skill | What it demonstrates |
|---|---|
| [web-search](../skills/web-search/SKILL.md) | Minimal permissions, provider chain in comments, built-in tools |
| [code-runner](../skills/code-runner/SKILL.md) | Built-in tool, triggers tuned to avoid routing conflicts, sandbox notes |
| [research](../skills/research/SKILL.md) | Multi-step with references, output format, source-citation rules |
| [anime-tracker](../skills/anime-tracker/SKILL.md) | Approvals with editable fields, volume tokens, scheduled job, depends_on |
| [files](../skills/files/SKILL.md) | Library skill with no folders of its own; gets scopes from the caller |
| [model-manager](../skills/model-manager/SKILL.md) | Model discovery, benchmarking, cleanup — no user files touched |

---

## 12. Sharing, importing and signing

### Bring in skills from elsewhere

Skills written for Claude Code, Codex, Hermes or `.agents` load unmodified; they're plain `SKILL.md` folders.

```bash
noskill skills import                      # finds them in those tools' usual folders
noskill skills import my-skill --from claude
noskill skills install ./my-skill          # a folder
noskill skills install https://host/org/repo.git   # a git URL, pinned to the commit you reviewed
```

Every install shows what the skill would be allowed to do (network, files, running programs, secrets), says whether it is signed, and waits for a yes typed in a terminal. The loader keeps front-matter it doesn't know. Claude-style skills with `scripts/` and no `noskill.tools` still work: their scripts are offered as `run_script` calls under `exec` permission, which asks.

### Take a skill elsewhere

Other runtimes ignore the `noskill:` block, so a skill folder works in them as it is. `noskill skills export` isn't built yet; to share a plain version, copy the folder and delete the `noskill:` block by hand.

### Sign a skill, publish a registry (FR-S10)

A signature says who published a skill and that its files haven't changed since. It doesn't say the skill is safe: the permission screen and the sandbox still do that ([ADR-0016](adr/0016-signed-skill-registry.md)).

```bash
noskill registry key new                       # once: a signing key, kept in the OS keychain
noskill registry key show                      # the public key to give people
noskill skills sign ./my-skill                 # writes .noskill-sig.json into the folder

noskill registry publish ./my-registry --name team   # signs every skill folder in it, writes a signed index.json
```

A registry is a folder, or a git repository, of skill folders plus `index.json` and `index.json.sig`. Anyone who has your public key can use it:

```bash
noskill registry add team https://host/org/team-skills.git --key <public key>
noskill registry search pdf
noskill skills install team/pdf-tools
noskill skills verify pdf-tools               # later: were its files edited after install?
```

Things to know:
- **The key is passed by the person adding the registry.** A registry can't vouch for itself.
- **`publish` re-signs every skill with the registry's key**, replacing any signature a skill came with.
- **Removing a registry is how you stop trusting a key.** There's no revocation list.
- **A folder with a symlink in it can't be signed**, because an install wouldn't copy the link.
- **Change a file, sign again.** The signature covers every file except `.git` and itself.

---

## 13. Tips from the built-in skills

- **`web-search`**: Provider chain lives in comments. The body just says "call `search`". The model doesn't need to know there are four providers.
- **`code-runner`**: Triggers avoid "python" on purpose — that would pull "write a python function" away from the `code` routing rule. Think about what your skill's triggers might collide with.
- **`anime-tracker`**: Uses `${approved.dest}` so the user picks the folder at approval time, not hard-coded. The skill never writes anywhere the user hasn't chosen.
- **`files`**: Declares no folders. It gets scopes from whatever skill brings it in. This keeps the permission surface minimal.

---

## 14. Common mistakes

| Mistake | Fix |
|---|---|
| Description is a label, not a goal | Write "Use when the user asks about X" not "A tool for X" |
| Body is prose, not steps | Number everything. One verb per line. |
| Too many triggers | Triggers are for keyword fallback. 3–5 is plenty. |
| Underspecified permissions | Declare exactly what you need. The broker will deny the rest. |
| No `confirm` on destructive tools | If a tool deletes, moves, or overwrites, put it in `confirm`. |
| Tests only cover the happy path | Add a failure case and a variation. The learning loop's `required` cases are what catch regressions. |
| Implementation notes in the body | Move them to HTML comments. A 4B model will try to follow them. |

---

## 15. Trying and fixing a skill

```bash
noskill run --skill my-skill --model qwen3:4b "the request you wrote it for"
noskill traces list
noskill traces show <run-id>
```

Force the skill with `--skill` so you test the skill, not the selector. Then leave it off and run the request again: if the skill isn't chosen, the description is the problem (`noskill route explain "…"` shows what would be picked and why).

Reading a trace, in this order:
1. **Was it offered?** The `run_start` line lists the skills shown. Not there: fix the description and triggers.
2. **Which tools did it call, with what arguments?** Wrong tool: the body is unclear about which. Wrong arguments: say the argument names in the step, in backticks.
3. **Did a call fail or get refused?** A refused call names the missing permission: add it to `permissions`, or change what the skill is asking for.
4. **Did it stop early or loop?** A loop guard stops a run that repeats a call; the trace says which. Add the step that comes after it.

Then change one thing and run it again. A 4B model's runs vary, so try each change two or three times before believing it.

Rate runs you're happy with (`noskill rate`): good runs are what the learning loop, `skills optimise` and, if you turn it on, the LoRA pipeline ([ADR-0015](adr/0015-lora-pipeline.md)) learn from.
