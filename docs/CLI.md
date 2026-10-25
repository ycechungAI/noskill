# CLI reference

The CLI is the top-priority interface (FR-I1). It's the same on macOS, Windows and Linux. Commands work on the shared database, so most work without the daemon; `run` and `chat` run in-process. Scheduled jobs only run while the daemon (or `noskill serve`) is up ([ADR-0009](adr/0009-scheduler-through-the-database.md)).

## Conventions
- Global flags: `--json` (machine-readable output), `--quiet`, `--home <dir>`, `--no-color`, `--verbose`.
- Exit codes: `0` ok, `1` error, `2` bad usage, `3` waiting for approval, `4` budget exceeded, `5` backend unavailable.
- No color-only signals: every status has a word (`ok`, `failed`, `waiting`).
- Long-running commands stream progress to stderr and the result to stdout, so `noskill run … > out.md` works.

## Setup

| Command | Does |
|---|---|
| `noskill init` | Detect backends and models, probe tool strategy, suggest pulls for your RAM (incl. a JEV decision model), optional quick bench, offer daemon install |
| `noskill doctor [--client openwebui] [--reindex]` | Check backends, Ollama version (≥ 0.35 for decisions) and recommended Ollama settings, daemon, service, tray, sandbox strength, permissions, disk, drives; print fixes. `--client` checks a chat app's connection from both sides; `--reindex` rebuilds the database index from skill and trace files |
| `noskill daemon install [--tray]` / `uninstall` / `start` / `stop` / `status` / `logs [-f]` | Manage the background service (launchd / systemd --user / Task Scheduler) |
| `noskill config get|set|edit <key> [value]` | Read/write `config.toml` ([CONFIG.md](CONFIG.md)) |
| `noskill config check` | Validate types, unknown keys, paths and secret references; show which layer (default, file, project, env, flag) set each value with `--explain` |
| `noskill secrets set|rm|list <name>` | Store secrets in the OS keychain; reference them in config as `secret:NAME` |
| `noskill update [--check] [--to <version>] [--channel stable|beta]` | Signed self-update with rollback; prints the package-manager command if installed via brew/winget/Scoop/apt ([PACKAGING.md](PACKAGING.md#noskill-update)) |
| `noskill uninstall [--purge]` | Remove service, tray login item and self-installed binaries; keeps data unless `--purge`, which moves it to the OS trash |
| `noskill export [--all] <file.tar.zst>` / `import <file>` | Move config (secret references only), skills and history to another machine ([DATA_MODEL.md](DATA_MODEL.md#export--import)) |

## Running

| Command | Does |
|---|---|
| `noskill run "<task>"` | One-shot agent run. Flags: `--model <m>` (pin), `--preset fast|balanced|best`, `--skill <name>` (force), `--depth quick|normal|deep`, `--budget-steps N`, `--no-learn`, `--out <file>` |
| `noskill chat` | Interactive session in the terminal (multi-line input, `/model`, `/skills`, `/rate`, `/learn`, `/exit`) |
| `noskill rate <run-id|last> good|bad ["note"]` | Explicit feedback for learning (FR-L1) |
| `noskill learn <run-id|last>` | "Remember how to do this": draft a skill from that run |
| `noskill hook qbittorrent-finished\|transmission-finished\|torrent-finished [--dry-run] [<hash>]` | For the torrent app to run when a download finishes: stops that torrent seeding and files it now (anime-tracker's file stage, one torrent). qBittorrent: Options › Downloads › Run external program on torrent finished: `noskill hook qbittorrent-finished "%I"` (full path to the binary on macOS and Windows). Transmission: point it at `scripts/transmission-done.sh` (copy it next to the binary in `~/.noskill/bin`; it runs `noskill hook transmission-finished`, which reads `TR_TORRENT_HASH`, and logs to `~/.noskill/logs/hook.log`). macOS: Settings › Transfers › Management › "Call script when download completes"; elsewhere `script-torrent-done-enabled` and `script-torrent-done-filename` in settings.json. On macOS the script runs under Transmission, so Transmission itself needs access to the library drive (Full Disk Access); a failed hook only delays the file to the next scheduled run |
| `noskill undo <run-id>` | Revert file moves from a run (uses the undo log). Also takes the `anime-…` id that `noskill anime run` prints after it filed episodes |

## Skills

| Command | Does |
|---|---|
| `noskill skills list [--learned|--status <s>]` | Name, origin, status, version, uses, success rate |
| `noskill skills show <name>` | Front-matter, permissions, stats, versions |
| `noskill skills install <git-url|path|registry/skill>` | Shows permissions and who signed it, asks, installs (pinned to commit, or to the registry's version and digest). A folder whose `.noskill-sig.json` doesn't match its files is refused ([ADR-0016](adr/0016-signed-skill-registry.md)) |
| `noskill skills sign <folder>` | Write `.noskill-sig.json` into a skill folder, signed with your registry key (FR-S10) |
| `noskill skills verify <name>` | Check an installed skill against its signature; names the files changed since |
| `noskill skills import [<name>|<folder>] [--from claude|codex|hermes|agents]` | Finds skills written for Claude Code, Codex, Hermes or `.agents` (their usual folders, plugins included), says what carries over and what doesn't (`allowed-tools`, `$ARGUMENTS`, `!`command``, hooks, scripts), shows the permissions it would have, asks, and copies the folder unchanged (FR-S8) |
| `noskill skills enable|disable|remove <name>` | |
| `noskill skills test <name>` | Run its `tests/` in the sandbox |
| `noskill skills setup <name>` | Guided config (e.g. qBittorrent host and default folder for anime-tracker) |
| `noskill skills rollback <name> [version]` / `diff <name> [v1] [v2]` | Versions of learned skills |
| `noskill train export [--task t] [--since 30d] [--base m]` | Write a dataset of good runs from real use (mlx-lm chat format with tools, one example per assistant turn) and a REVIEW.md to read first (FR-L8, [ADR-0015](adr/0015-lora-pipeline.md)) |
| `noskill train lora --data <id> --base <model> --reviewed [--iters n] [--dry-run] [--force]` | Train a LoRA with MLX through uv, import it into Ollama as `<base>-ns<n>`, benchmark it against its base on the checked tasks. Refuses below `learning.lora.min_runs` good runs, off Apple Silicon, on battery, or with under 8 GB free |
| `noskill train list` · `adopt <n>` · `revert <n>` | Datasets and training runs; an adopted model stands in for its base on its task type |
| `noskill skills optimise <name> [--budget 4] [--model m] [--tasks n] [--adopt] [--dry-run] [--force]` | Rewrite a built-in or learned skill's instructions against the checked tasks that name it, keep the rewrite only if it wins on held-out tasks too (FR-L9). `--model` is the model the tasks run on; a model that reasons first (`qwen3:4b`) takes about 3 minutes a task, so `--tasks 8 --budget 2` makes a quick run. Each task line shows how long it took. `--adopt` puts a built-in's accepted rewrite straight into use (`revert` undoes it); it can't be combined with `--tasks`, because a partial list is too noisy. `--dry-run` is a short trial (8 tasks and 2 rewrites unless you pass `--tasks` and `--budget`) that saves no version. Every search, dry runs included, writes all its rewrites and scores to `<data>/optimise/<skill>-<time>.json`. `adopt <name>`, `revert <name> [--to N]`, `show <name>` manage a built-in's tuned versions ([LEARNING.md](LEARNING.md#prompt-optimisation-fr-l9)) |
| `noskill skills export <name> [--plain]` | Copy out; `--plain` strips the `noskill:` block |

## Registries

Built in block 5c (FR-S10, [ADR-0016](adr/0016-signed-skill-registry.md)). A registry is a folder or git repository with skill folders, `index.json` and `index.json.sig`.

| Command | Does |
|---|---|
| `noskill registry key new [--replace]` · `key show` | Make your ed25519 signing key (kept in the OS keychain, never shown) and print its public half to share |
| `noskill registry publish <folder> [--name n]` | Sign every skill folder under it with your key and write the signed index |
| `noskill registry add <name> <folder|git-url> --key <public key>` | Fetch the registry, check its index against the key you were given, and keep it. The key is required: the registry's own copy of it proves nothing |
| `noskill registry list` · `remove <name>` · `update [name]` | Sources you added; `update` fetches the indexes again and refuses one older than the last seen |
| `noskill registry search <words>` | Search the kept indexes (no network) |

## Checks for the week run

| Command | Does |
|---|---|
| `noskill anime add <link> [--to folder] [--name n]` | Add a download by hand: a magnet link, an https `.torrent` link or a Nyaa page. It is filed in the folder when it finishes (every file; one file takes `--name`), and stops seeding then. The status page's Downloads tab does the same |
| `noskill anime file-now` | Stop finished downloads seeding and file them now, as the next filing run would |
| `noskill anime audit [--since 24h] [--job anime-nightly]` | The week run's morning check as one command: the job ran and didn't fail, every file it filed is named title + episode (+ resolution) with no release leftovers, nothing changed in its destination folders that it didn't record, and a dry-run undo plans every step. Prints PASS or FAIL, exits 1 on a FAIL, and appends the result to `<data>/logs/week-audit.jsonl` ([WEEK_RUN.md](WEEK_RUN.md)) |
| `noskill doctor --files` | Adds one real move, copy, trash and `undo` in a folder of noskill's own (it uses the OS trash and leaves two small items there). The Windows and Linux gate for Phase 3: no Recycle Bin location is reported on Windows, so a trash step can't be undone by noskill there, and the test says so as a warning |

## Schedules and approvals

| Command | Does |
|---|---|
| `noskill schedule add <skill|"prompt"> --cron "<expr>" | --every <dur>` | Flags: `--when volume:<label>`, `--when ac-power`, `--when unmetered`, `--when window:22:00-06:00` (repeatable; all must hold), `--missed skip|once`, `--timeout`, `--notify os,ntfy`. Target `anime:<stages>` (`anime:all`, `anime:download,file`) runs the anime tracker's stages directly, with no model, so it needs no Ollama and takes seconds; it takes no `--prompt` or `--model` |
| `noskill schedule list` / `pause|resume|rm|run <job>` | |
| `noskill jobs` / `jobs stop <id|--all>` | Running jobs and kill switch |
| `noskill approvals` | Pending items: id, source (job/skill/learning), what, expires |
| `noskill approvals approve|skip <id|--all> [--set field=value] ["note"]` | Resolve items; `--set` changes an editable field (e.g. `--set dest=/Volumes/RED/ANIME2026`). Also from tray, status page, chat or notification actions |

## Models and routing

| Command | Does |
|---|---|
| `noskill models list` | Backends, models, capabilities (incl. `decide`), loaded state, scores |
| `noskill models bench <model> [--perf-only]` | Speed, memory, cold load, and a quality score per task type from a 12-task micro-eval (about 30 s on a 4B model). `--perf-only` skips the quality part. A JEV decision model gets the decision suite instead (accuracy on obvious questions, calibration, reworded and shuffled inputs, instructions hidden in the data, hedging on ambiguous input, determinism, one question against six in a call), and an embedding model the embedding suite (picking the right skill for a request, the same request in other languages, long paragraphs, paraphrases, batch speed). Scores land in the quality column as `decide-*` and `retrieval`, `mrr`, `cross-lingual`, `long-docs`, `paraphrase` |
| `noskill route explain "<prompt>"` | Which model would be picked and why |
| `noskill decide --model <m> --state '<json>' --questions '<json>'` | Raw decision call (debugging FR-B6) |
| `noskill calibration [show]` | Per decision and model: decisions made and labelled, calibration error (ECE), Brier score, reliability bins, the threshold in force, the suggested one and recent changes (FR-B6c) |
| `noskill calibration tune <decision> <model> [--apply]` | Show the suggested threshold; `--apply` stores it (works with `auto_tune` off) |
| `noskill calibration revert <decision> <model>` | Undo the last threshold change |

## Documents

| Command | Does |
|---|---|
| `noskill docs add <folder>` | Adds a folder noskill may read for the `local-docs` skill (your grant), and indexes it now: text, Markdown, HTML and Word files; PDFs are listed as left out |
| `noskill docs list` / `remove <folder>` | Folders with files, passages, how many have a meaning vector, when updated, and what was left out. `remove` deletes the folder's index data only |
| `noskill docs index [folder]` | Brings the index up to date (changed files only); also adds meaning vectors once an embedding model is installed |
| `noskill docs search "<question>" [-k N]` | The same search the skill does, with scores and sections |

## Memory, traces, eval

| Command | Does |
|---|---|
| `noskill memory list|add|forget <key>` | User facts and preferences (FR-L6) |
| `noskill volumes [forget <label>]` | Attached drives and the label → UUID bindings behind `volume:<label>`; `forget` after replacing a drive (PLATFORMS.md "External drives") |
| `noskill traces list|show <id>|purge --before <date>` | Run history |
| `noskill eval [--since 30d]` | Success rate, steps, tokens, latency trends; learning report |

## Serving

| Command | Does |
|---|---|
| `noskill ui [overview\|approvals\|downloads\|history] [--port N] [--no-open]` | Open the status page on a tab. If the daemon or a `noskill serve` is answering, that page opens; otherwise a page-and-API server with no scheduled jobs starts and runs until Ctrl-C. Same on macOS, Windows and Linux (replaces `ask.sh` / `ask.ps1` once the installed noskill has it). `start.sh` / `start.cmd` is a menu of everyday commands: [START.md](START.md) |
| `noskill serve` | Foreground server (the daemon runs this too) on `127.0.0.1:11435`: OpenAI-compatible `/v1/*`, Ollama-compatible `/api/*` (same layout as Ollama on 11434, so apps just change the port), status page `/ui`. Flags: `--port`, `--lan --token <t>` to expose beyond localhost, `--no-jobs` (page and API only: no scheduled jobs, learning or approval sweeps run in this process, so a second server can sit next to the daemon; `ask.sh` uses it). Setup per chat app: [PROXY.md](PROXY.md) |

## Example session

```
$ noskill init
✓ Ollama 0.35.2 on :11434 · 6 models
✓ qwen3:8b       tools: native   · 47 tok/s
✓ qwen2.5-coder:7b tools: native · 52 tok/s
✓ tev1:0.8b      decide          · 235 ms
! no embedding model: pull nomic-embed-text? [Y/n]
? Install background service (needed for schedules)? [Y/n]

$ noskill skills setup anime-tracker
? qBittorrent Web UI address [localhost:8080]:
✓ qBittorrent 5.0.4 · 3 RSS feeds found
? Default folder for new shows (you can change it for each show) [~/Videos/Anime]: /Volumes/RED/ANIME2026
✓ RED mounted (exFAT, 1.6 TB free) · bound to volume UUID
? Download to [~/Downloads]:
? Preferred resolution [480p, then 720p, then 1080p]:
✓ Scheduled daily at 03:00 · every new show needs your approval

$ noskill approvals
ID   SOURCE          WHAT                                                        EXPIRES
a12  anime-tracker   Add "Frieren Season 2" · 480p · → /Volumes/RED/ANIME2026    in 23h
a13  learning        Use learned skill release-notes-lookup                      in 6d
$ noskill approvals approve a12 --set dest=/Volumes/RED/ANIME2026-AUTUMN
✓ Frieren Season 2 → /Volumes/RED/ANIME2026-AUTUMN/Frieren Season 2/
✓ rule "noskill: Frieren Season 2" created (480p) · matches 2 items in feed "subs"
```

## As built in block 1b
Working now: `init`, `doctor`, `run` (`--model --preset --skill --allow --depth --budget-steps --no-learn --think --out`), `chat`, `rate`, `undo [--dry-run]`, `skills list|show|enable|disable|remove|install|prepare`, `approvals [approve|skip <id|--all> --set k=v]`, `models list|probe|bench|pull|rm`, `route explain`, `decide`, `memory list|add|forget`, `traces list|show|purge --before`, `config check|get|set|edit|path`, `secrets set|rm|list` (the value is read from a hidden prompt or `--stdin`, never an argument), `serve [--port --lan --bind]`, `daemon install|uninstall|start|stop|status|logs`, `update [--check --to --channel --yes]`, `uninstall [--purge]`, `version`.
Built in block 1c: `schedule add|list|pause|resume|rm|run|history` (`--cron`, `--every`, `--name`, `--prompt`, `--model`, `--allow`, `--missed once|skip`, `--overlap skip|queue`, `--timeout`, `--retries`, `--notify <channels>|none`, `--paused`), `jobs [stop <job|run>|--all] [history [job]]`, `run --resume <approval-id>`. The CLI only edits the database; `serve` (the daemon) runs the jobs ([ADR-0009](adr/0009-scheduler-through-the-database.md)). Block 3 added `--when` (FR-J5): a due run waits, checked every few seconds, until its preconditions hold; if the next occurrence comes first it is recorded as `unmet` and skipped. `schedule run` starts a job without checking them. Block 3 also added `volumes [forget <label>]` and `anime detect | scan [--offline] [folder…] | run [--stage answers|suggest|batches|download|file] [--dry-run] | status` (skills/anime-tracker: finds Transmission or qBittorrent installed for the user or system wide, scans Videos, Downloads, Desktop and external drives for anime you have (checked on AniList), suggests this season's new shows you don't have for approval, downloads approved shows' English-subbed episodes from Nyaa that you don't have yet, and files them).
Not built yet: `skills test|setup|export`, `serve --token` (tokens come from `[[server.tokens]]`, so they never appear on a command line).
Built in block 2a:
- `learn <run-id|last> [--later]`: draft a skill from that run now, or only queue it.
- `learn queue`: the queue, and whether the background worker can run now.
- `skills versions <name>`, `skills diff <name> [from [to]]`, `skills rollback <name> [version]`: the newer version is kept.
- `skills enable <name>`: also brings back a `stale` or `archived` learned skill.
- `skills prepare code-runner`: installs numpy, pandas, openpyxl and matplotlib for `run_python`.
- `approvals` answers two new kinds: `learned_skill` ("noskill learned a way to do this… Use it?") and `outcome_check` ("Was this answer right?": approve = yes, skip = no).

Built in block 2b:
- `eval list|run|report|history|pin|gate|simulate`: the checked tasks (see [LEARNING.md](LEARNING.md#as-built-noskill-eval-block-2b)). `eval run [--model m] [--type t] [--task id] [--quick]` runs them on one model (default: the smallest chat model) and compares with the last run of the same model and suite. `--type` and `--task` add up (`--type files --task math-01`). `--no-learned` keeps learned skills out of agent tasks (a separate suite, for comparing with and without them). `eval gate [--rollback]` is the same and exits 1 on a regression; with `--rollback` it also puts back each learned skill that changed since the last run and took part in a task that fell. `eval simulate [--model m] [--days 28] [--per-day 3] [--seed 1] [--progress-json file]` is the learning exit check: weeks of simulated use in a fresh home (`--home`), then the held-out multi-step tasks again; it exits 1 when learning didn't help by 15%. `eval pin <run-id|last> [--id name] [--contains text]` saves a finished run as a task of yours.
- `route explain` also prints each candidate's quality, how many rated runs moved it and its expected time.
- `models bench` runs the quality micro-eval after the speed test.
- The tray app is a separate binary, `noskill-tray`; the status page is `http://127.0.0.1:11435/ui` while `serve` runs ([ADR-0011](adr/0011-tray-and-status-page-over-http.md)). MCP isn't planned.

Built in block 5a:
- `skills optimise <name>|adopt|revert|show` (FR-L9, [ADR-0014](adr/0014-skill-optimisation.md)). `eval gate --rollback` also puts back a tuned built-in.

Built in block 5b:
- `train export|lora|list|adopt|revert` (FR-L8, [ADR-0015](adr/0015-lora-pipeline.md)). Tested with a fake trainer only; no real training has run yet.

The learning worker itself runs inside `serve`.
Exit codes in use: 0 ok, 1 error, 2 usage, 3 waiting for approval, 4 budget exceeded, 5 no backend; 130 on Ctrl-C.
