# Start here: the menu

`start.sh` (macOS, Linux) and `start.cmd` (Windows) gather the everyday commands in one menu, so you don't need to remember them. Both read the same list of items; every item is also a plain `noskill` command, shown when you run it.

```bash
./start.sh                    # the menu: type a number or a name, q quits
./start.sh research "heat pumps"   # one item, straight away
./start.sh --list             # every item
./start.sh -n ui              # show the command without running it
./start.sh --help             # the options
```

On Windows use `start.cmd` in place of `./start.sh` (it runs `start.ps1` for you, with the script policy relaxed for that one run).

## Which noskill it uses

The installed one (`noskill` on your PATH, or `NOSKILL_BIN`). If that one doesn't have a command yet, because it was installed before the command was added, the script builds the current source into a scratch folder and uses that, and says so. It never replaces the installed noskill, and `--source` forces the source build. Items marked Dev need Go.

## Items

## Setup

### install

Build and install noskill on this computer, and everything it needs that is missing. In order, each step asking first (`./start.sh install -y` agrees to all of them except the optional 4 GB model):

1. **Go**, if it isn't installed: the official build from go.dev (about 70 MB), SHA-256 checked, into `~/.local/go` or `%LOCALAPPDATA%\Programs\go`. No sudo or admin.
2. **noskill**: builds this source tree into `~/.local/bin` (macOS, Linux) or `%LOCALAPPDATA%\Programs\noskill` (Windows); `NOSKILL_INSTALL_DIR` chooses another folder. It asks before replacing an installed noskill and is separate from the background service's copy in `~/.noskill/bin`. On Windows it adds the folder to your user `PATH`; on macOS and Linux it prints the line to add.
3. **Ollama** and the **models**: see the next item.
4. `noskill init`, which finds your backends and models and writes a starting config.

Runs: `go build -o <folder>/noskill ./cmd/noskill`, then `noskill init`

### ollama

Install Ollama if it is missing and download the recommended models. It looks for `ollama` on your PATH and in the usual app folders. If it isn't there it offers to install it, per user and with the checksum checked: the macOS app into `~/Applications` (about 210 MB), the Linux build into `~/.local` (about 1.5 GB, needs `zstd`, no sudo), or on Windows Ollama's own installer (about 1.5 GB, its signature checked too). Then it starts Ollama, waits for it and offers the models noskill recommends for this much memory:

- **8 GB or more:** `granite4:3b` (about 2 GB, the main model: tools, about 9 seconds a task) and `gemma3:1b` (about 0.8 GB, the backup for plain tasks).
- **Under 8 GB:** `gemma3:1b` only.
- **12 GB or more:** it also asks about `qwen3.5:4b` (about 4 GB, slower and stronger), default no.

Models already installed are skipped and they are fetched one at a time. `./start.sh ollama -y` agrees without asking.

Runs: `ollama pull <model>` for each

## Use

### ui

Status page: approvals, downloads, history. Opens the status page in your browser: Overview, Approvals, Downloads, History. If the daemon is already serving it, that page opens; otherwise a page-only server (no scheduled jobs) starts and stays up until Ctrl-C. Same as `noskill ui [tab]`.

Runs: `noskill ui`

### approvals

What is waiting for your yes. Lists what noskill is asking you: shows to add, episodes to download, learned skills to use. Answer with `noskill approvals approve <id>` or `skip <id>`, or on the status page.

Runs: `noskill approvals`

### run

Run one task. Runs one task. Type it when asked, or put it after the name: `./start.sh run "summarise ~/Documents/notes.md"`.

Runs: `noskill run {}`

### chat

Chat in the terminal. A chat in the terminal with the same skills and routing as `run`.

Runs: `noskill chat`

### research

STORM research report on a topic. A STORM report: five viewpoints (practitioner, academic, skeptic, economist, historian) each ask questions, search the web and read the pages; the report cites only pages it fetched. Writes a new folder under `Documents/noskill-research`. Say "quick look" or "deep" in the topic for 2 or 5 questions per viewpoint. See [LEARNING.md](LEARNING.md).

Runs: `noskill run --skill storm {}`

### ask-docs

Ask your own documents. Answers from the folders you added, with the file named. Add a folder first (`docs-add`).

Runs: `noskill run --skill local-docs {}`

### docs-add

Let noskill read a folder. Lets noskill read a folder (text, Markdown, HTML, Word; not PDFs yet) and indexes it. Nothing outside added folders is read.

Runs: `noskill docs add {}`


## Anime

### anime

Tracker status. Shows what the anime tracker has planned, added and filed, and whether your torrent app answers.

Runs: `noskill anime status`

### anime-dry

Try a run without downloading. Runs the tracker's stages without downloading or filing anything, so you can see what it would do.

Runs: `noskill anime run --dry-run`

### anime-add

Add a download by link. Adds one download from a magnet link, a .torrent link or a Nyaa page; it is filed in the folder you choose when it finishes.

Runs: `noskill anime add {}`

### anime-audit

Check the last 24 hours. The week run's check of the last 24 hours: did the jobs run, were files named and filed correctly. Writes `logs/week-audit.jsonl`.

Runs: `noskill anime audit --since 24h`


## Check

### doctor

Check the setup. Checks folders, config, database, backends, memory, sandbox, service and disk, and prints a fix for each problem.

Runs: `noskill doctor`

### models

Models and their speed. Installed models with size, capabilities and measured speed.

Runs: `noskill models`

### jobs

Running jobs and recent runs. Jobs running now and recent runs.

Runs: `noskill jobs`

### schedule

Scheduled jobs. The scheduled jobs and when they next run.

Runs: `noskill schedule list`

### skills

Installed skills. Built-in, learned and installed skills, with their status.

Runs: `noskill skills list`


## Test

### bench

Benchmark a model: speed, memory, quality. Measures tokens per second, memory at the 8k context and a cold load (it unloads the model first), then runs a 12-task quality check (about 30 seconds on a 4B model). The scores are saved in your noskill folder and the router uses them as its starting guess for that model. Press Enter for `granite4:3b`.

Runs: `noskill models bench <model>`

### bench-speed

Benchmark speed and memory only. The same speed and memory test without the quality check, and it saves no quality scores. Press Enter for `granite4:3b`.

Runs: `noskill models bench <model> --perf-only`

### eval

Run the checked tasks on a model. Runs every checked task on one model (about 7 minutes for `granite4:3b`) and records the score for the status page. Type the model name or press Enter for `granite4:3b`.

Runs: `noskill eval run --model {}`

### optimise

Quick trial of a better wording for a skill. A quick trial (8 tasks, 2 rewrites, nothing saved) of whether a model can write better instructions for a built-in skill. Enter for `files`. Real runs use `noskill skills optimise <skill>` ([LEARNING.md](LEARNING.md)).

Runs: `noskill skills optimise {} --model granite4:3b --dry-run`


## Dev

### test

Go tests. `go test ./...` in this source tree. Needs Go.

Runs: `go test ./...`

### vet

Vet all six targets. `go vet ./...` for the six targets (macOS, Linux and Windows on arm64 and amd64). Needs Go.

Runs: `vet`

### site

Build the docs site. Builds the docs site into `site/` from the Markdown. Needs Go.

Runs: `go run ./scripts/docsite`


## Help

### readme

Show the guide. Prints this guide.

Runs: `readme`

## Adding an item

Add one line to the table in `start.sh` **and** the same line to `start.ps1`, then a `###` section here. A test fails if the two tables differ or an item has no section.
