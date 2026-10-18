# S5 results: Hermes Agent teardown
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1 · Hermes Agent by Nous Research (MIT). Read: release v0.22.2 (cloned). Installed and run: `main` at v0.22.1+163 (the installer's default) · model `qwen3.5:4b` on Ollama 0.40.3 · harness [spikes/s5](../../spikes/s5/) · compared with [S1](S1-results.md)

**Deviations from the plan:**
- **macOS only.** There was no Windows or Linux machine, so Windows-native versus WSL and the install experience on L1/W1 are read from Hermes's docs, not tested.
- **Model.** `qwen3.5:4b` with thinking off (as in S1), at your instruction, instead of `qwen3:8b`.
- **Sandboxed install and runs.** Hermes's own installer ran stage by stage under macOS `sandbox-exec` with a throwaway HOME, so nothing touched your shell config or real home folder. Runs were also sandboxed (writes only under `spikes/s5/out`, network only to localhost). I skipped the installer's optional browser and computer-use downloads and the interactive setup stages. None of the ten task failures came from the sandbox (checked in Hermes's logs). In the later multi-step run one write failed because I started it from the wrong folder and the sandbox denied it.
- **Context.** Hermes requires at least 64,000 tokens of context, so the model ran on a separate Ollama with `OLLAMA_CONTEXT_LENGTH=65536` (port 11500, your Ollama untouched).
- **Tasks.** 10 of `tools-50` that map to real tools: `t01 t05 t06 t09 t12 t14 t23 t25 t38 t39`. Web tasks weren't run (network blocked). `t01` checks a live clock instead of the mock's fixed one. Fixture files are real (empty) files in the sandbox's home. The S1 comparison is **not controlled**: S1 used mock tools and a different harness.
- **Skill creation.** One run with `skills.creation_nudge_interval` lowered from 10 to 2, one run asking directly for a skill.
- **Mistake to report:** when stopping my second Ollama server I used a name pattern that also stopped your own server. Ollama.app restarted it within seconds and nothing was loaded; no data was lost.

## Numbers

### Install and footprint
| Item | Result |
|---|---|
| Install, non-interactive (`install.sh --non-interactive`) | **217 s**: download 61 s, Python 3.14 + `uv` 14 s, dependencies (incl. Node and ripgrep) 110 s, build 32 s |
| Still to do after install | `hermes setup` (API keys, model) or `hermes config set …` (4 commands here), and for Ollama a 64k context set server-side |
| Disk | **~2.0 GB** (checkout 0.9 GB, `~/.hermes` 1.0 GB incl. Node and Python, `~/.local` 72 MB), without the browser tools |
| Code | 2,543 Python files and **937,000 lines** outside tests (8,503 files and 2.3 M lines with tests); 282 files in `tools/`; 58 bundled skills (+153 optional) |
| Python | 3.14 only (exact-pinned dependencies); it downloads its own interpreter, Node and ripgrep |
| Platforms (docs) | Tier 1: macOS Apple Silicon, Windows 10/11 native (x64, arm64), Linux/WSL2, Docker. Intel Mac bundles aren't built by default |

### Speed and memory
| Item | Result |
|---|---|
| `hermes --version`, cold / warm | **1.58 s / 0.70 s**, 66 MB |
| One query to an instant stand-in model | **6.8–9.1 s** and 157 MB peak: even `hermes chat --oneshot` starts a separate gateway process |
| Resident after a one-shot | The gateway stays running: **134 MB idle** |
| Calls out at start-up | Fetches a model catalog from nousresearch.com and GitHub (failed harmlessly with no network) and audits the host's SSH settings |

### What Hermes sends before the user's first word
One-line question ("What time is it right now?"), captured with a stand-in server:

| Part | Size | Tokens (`qwen3:4b` / `qwen3.5:4b` tokenizers) |
|---|---|---|
| System prompt | 14.9 KB | ~3,400 / ~3,500 |
| 20 tool schemas (of 282 tool files) | 37.5 KB | ~8,000 / ~9,100 |
| **Whole first request** | **51.9 KB** | **11,400 / 12,600** |

- The three biggest tools are `delegate_task`, `manage_connections` and `browser_exec` (3.4–4.7 KB each).
- Reading that on this M4 took **48 s (`qwen3.5:4b`) to 67 s (`qwen3:4b`)** before the first answer token; later calls reuse the cache (99% cache hits in its logs).
- A second, small request per session generates a title.
- Hermes's docs say agents need **at least 64,000 tokens** of context and smaller windows are rejected at startup (not tested); Ollama defaults to 4,096 below 24 GB and the API can't change it, so the user must set `OLLAMA_CONTEXT_LENGTH` server-wide or bake a Modelfile.
- With a 64k window `qwen3.5:4b` took **5.1 GB, growing to 9.5 GB** over the ten sessions (MLX prompt cache, as in [S3](S3-results.md#memory)); free memory fell from 42% to 25%.

### Ten `tools-50` tasks, real tools, `qwen3.5:4b`, thinking off
| Task | Result | Seconds |
|---|---|---|
| t05 list Downloads | ✓ | 70 (first request, cold) |
| t01 current time | ✓ | 31 |
| t06 count PDFs | ✓ | 30 |
| t09 move one file | ✓ | 12 |
| **t12 17.5% of 2,340 "using Python"** | ✗ | 9: answered **389.5** (correct: 409.5). Hermes **blocks `execute_code` in one-shot (`-q`) mode** because no user is present to approve it, and the model guessed the arithmetic |
| t14 SHA-256 | ✓ | 8 |
| t23 move all PDFs | ✓ | 30 |
| **t25 move the images** | ✗ | 10: printed the `mv` commands in a code block and moved nothing |
| t38 rename a file | ✓ | 20 |
| t39 find a file | ✓ | 10 |

**8 of 10; median 16 s per task** (7.7–70 s). S1's `qwen3.5:4b` native got 30 of 30 on the same ten tasks in the mock world, at ~5.8 s per run. A harder multi-step task (sort Downloads into three folders and write a summary) took **161 s and 25 model calls**, hit several blocked commands and failed writes, and Hermes's own "file-mutation verifier" correctly warned that two writes had failed despite the answer's wording.

### Learning loop
| Part | What Hermes does |
|---|---|
| Trigger | After a turn, once **10 tool iterations** have passed (configurable) a background fork replays the whole conversation and asks whether to save a memory or skill. Its code comments put this at **~30,000 tokens per review** |
| Bias | The review prompt says "be ACTIVE: most sessions produce at least one skill update" |
| Rules for the reviewer | Prefer updating a loaded skill, then an existing umbrella, then a support file, and only then a new class-level skill; names must not be session artifacts; a skill can only be edited after a fresh `skill_view` ("read-before-write", enforced) |
| Verification | **None before use.** No tests, no trial period, no promotion step. A skill security scan exists (`skills_guard`) |
| Curator | Runs weekly when idle ≥ 2 h: marks skills **stale after 14 days, archives after 30** (recoverable, never deletes); hub-installed skills are never pruned; the consolidation pass is **off** by default |
| Memory | `MEMORY.md`/`USER.md`, a nudge every 10 turns, session search, optional Honcho user modelling |
| Our run | With the nudge lowered to 2 and a 25-call task, no skill was written (`skill_manage` wasn't among the 20 tools sent). Asked directly, the 4B model wrote one in 154 s: [downloads-sorter/SKILL.md](../../spikes/s5/skill-written/downloads-sorter/SKILL.md) |

The skill it wrote is 149 lines (~1,500 tokens) for a three-step job, and it isn't correct: it points at a script that doesn't exist (`DownloadsSorter.py`) and a command that doesn't exist (`hermes-cli downloads-sorter`), tells the agent to run pseudo-calls like `terminal(command="python 'create_folder_structure()'")`, creates six folders (the request had three, one misspelt "Arcives"), and contradicts itself on copy versus move. It was saved and loadable at once.

### Safety and interop (read from its docs and code)
- **Security model:** "the only security boundary against an adversarial LLM is the operating system." The approval gate and scanners are heuristics. The default terminal backend runs commands directly on the host as the user; isolation is opt-in (Docker, SSH, cloud sandboxes).
- **Skills:** agentskills.io-compatible `SKILL.md` with an extra `metadata.hermes` block. **MCP** client and server, and an OpenAI-compatible API server, exist. ~20 messaging platforms (Telegram, Discord, Slack, WhatsApp, Signal, …), a cron scheduler, seven terminal backends.
- **Routing:** one model per session, `/model` to switch, fallback providers and "Mixture of Agents" presets. No hardware-aware memory slots.

## The "beat this" table
| Goal | Hermes, measured | noskill target |
|---|---|---|
| G1 Universal | macOS Apple Silicon, Windows native, Linux/WSL2, Docker. Intel Mac not built by default. Needs Python 3.14, Node, ripgrep | One binary on 6 targets, incl. Intel Mac and Windows arm64 |
| G2 Easy | 217 s install + setup + a server-side context change for Ollama, 2.0 GB | Under 2 minutes to a first skill run; core binary < 40 MB |
| G3 Fast | Cold start 1.6 s (0.7 s warm); 6.8–9.1 s for one query to an instant model; **134 MB idle**; first request 12k tokens | Cold start < 150 ms; idle < 50 MB; < 20 ms added per turn |
| G4 Small models | Needs ≥ 64k context; 11–13k tokens of preamble; 8/10 on real tasks with a 4B; 5–9.5 GB of memory for the model at 64k | Built-ins pass on a 3–4B model **in 8k context**; preamble ≤ ~2k tokens |
| G5 Self-improving | Skills written from one conversation, ~30k-token review, no verification, "be active" bias; curator never deletes | Draft → verify → trial → promote with rollback and a regression gate ([LEARNING.md](../LEARNING.md)) |
| G6 Orchestration | One model per session, manual `/model`, fallbacks | Hardware-aware slots, learned routing, JEV decisions |
| G7 Interop | agentskills.io, MCP both ways, OpenAI-compatible API server | Same, plus an Ollama-compatible proxy so other UIs gain skills |
| G8 Safe by default | OS is the only boundary; default backend is the host; one-shot blocks code and flagged commands | Per-skill permissions, OS sandbox, copy-in workspace, egress proxy; learned skills verified first |

## Ideas to copy
1. **File-mutation verifier:** after a turn, compare what the model *says* it changed with what actually changed, and warn. noskill's trace already records file effects; add the warning to the answer.
2. **Read-before-write for skill edits**, enforced by the tool (refuse an edit unless the skill was loaded in this review).
3. **Review prompt design:** prefer updating a loaded skill, then an umbrella, then a support file, and only then a new skill; class-level names, never session artifacts; user preferences go into the skill body.
4. **Run the review after the answer is delivered**, on a fork that shares the prompt prefix, with a tool whitelist. On local models also cap its cost (their ~30k tokens is minutes on a 4B).
5. **Curator lifecycle:** stale after N days, archived (recoverable) after M, never deleted, hub-installed and pinned skills exempt, consolidation opt-in. Matches our "no hard deletes" rule.
6. **"Per-conversation prompt caching is sacred":** don't change the system prompt or toolset mid-conversation; defer changes to the next session. For noskill that means a stable prefix: skill instructions go after the client's system prompt in a fixed order, and a selected skill shouldn't reshuffle earlier text.
7. **Lazy tool disclosure** (`tool_search`, `tool_describe`, `tool_call`) and cheap auxiliary models for side tasks (titles, compression): the right idea, but even with it Hermes sends 8–9k tokens of schemas. noskill should send 3–5 core tools (≤ 1.5k tokens) and everything else by lookup.
8. **Exact-pinned dependencies** after a PyPI supply-chain worm, and a documented antivirus false-positive for its unsigned bundled `uv`: both support our signing and `go.sum` plans ([PACKAGING.md](../PACKAGING.md)).

## Decision
- **ADR-0003 stands: noskill stays standalone.** Hermes covers the same feature list (skills, learning, scheduler, MCP, OpenAI API, messaging) but is built for 64k+ context and cloud-class models. On a 16 GB Mac it needs 11–13k tokens of preamble, a minute to read it, 134 MB resident, and 2 GB of disk, and a 4B model got 8 of 10 tasks right. That's the gap noskill is for.
- **Where noskill must prove itself** (these become eval targets): an 8k-context preamble, verified learned skills, a start-up under 150 ms, and a default-sandboxed tool runtime.
- **What not to compete on:** messaging platforms, cloud terminal backends, voice and desktop apps (already non-goals).
- **Risk to watch:** Hermes may add small-model support or a lighter profile; recheck before v1.
- **Not tested here:** Windows and Linux installs, web tasks, interactive approvals, Hermes's curator run, its MCP server, and a second 4B model.
