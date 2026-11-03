# The noskill app

The noskill app is one macOS app that starts and runs everything you need for local AI on this
machine, with one window to use it from. It brings together three projects:

| Part | What it does | Project folder | Port |
|---|---|---|---|
| **Agent** (noskill) | Skills, model routing, the OpenAI/Ollama proxy, jobs and approvals | `~/dev/noskill` | 11435 (status page at `/ui`) |
| **Research** (orcastrator) | Long research reports: a planner, parallel researchers, a critic and a writer (STORM) | `~/dev/noskill/apps/orcastrator` | 8400 |
| **Tasks** (scheduler, `sched`) | Prompts that run on a cron schedule and save a Markdown report each time | `~/dev/noskill/apps/scheduler` | 8787 |

All three run on **Ollama** (port 11434), which the app also starts if it isn't running.

The app is built in [apps/macos/](../apps/macos/README.md). Why it runs the three projects as they
are, rather than merging their code, is in [ADR-0017](adr/0017-noskill-app-runs-three-projects.md).

## Install from the DMG
`apps/macos/make-dmg.sh` makes `apps/macos/build/noskill-<version>.dmg`. Open it and drag
**noskill** to **Applications**. Dragging to `/Applications` needs an administrator; without one,
copy it to `~/Applications` instead (make that folder if it isn't there). Both work the same.

The app is signed ad hoc, not with a Developer ID, and isn't notarized. A copy built on the same
Mac opens straight away. A DMG downloaded onto another Mac is blocked the first time: open
**System Settings › Privacy & Security** and press **Open Anyway** next to noskill.

## The first run: Setup
When there's no `~/Library/Application Support/noskill/app.json` yet, the app opens on
**Setup**, which says in plain words what noskill is and what each part does, then checks this
Mac. Nothing starts, downloads or changes until you press a button. Setup also opens by itself
on any later launch where a part needs fixing, and is always in the sidebar (with a count of
what needs attention).

Each part gets one row: what was found and, when something is missing or broken, why and a
one-click fix. Fixes that download something, take long or change files ask first and say what
they'll download and how big it is. A running fix shows its progress and a Stop button, and its
full output is in **Logs › Setup**. After each fix Setup checks again; if the problem is still
there it says so.

| Part | What Setup checks | The fix |
|---|---|---|
| Ollama | the `ollama` program and whether it answers on its port | **Install Ollama**: downloads Ollama's latest release from GitHub (checksum checked) into `~/Applications` and opens it, as noskill's `start.sh` does; or get it from ollama.com. Installed but stopped: **Start Ollama** |
| Models | the recommended models: granite4:3b and gemma3:1b, or only gemma3:1b on a Mac with under 8 GB of memory | **Download the models**, through Ollama's API with a progress bar |
| Agent | `~/.local/bin/noskill` exists and runs, and `~/.noskill/config.toml` exists | **Install the agent**: copies the noskill program that comes inside the app (no download), then runs `noskill init`. An app built without it builds from the noskill folder with `./start.sh install -y` (which fetches Go if it's missing) |
| Research | the orcastrator folder, its `start.sh`, and a working `.venv` | **Set up Python**: finds or fetches uv and a uv-managed Python 3.12, then runs orcastrator's own `./start.sh --setup` |
| Tasks | the scheduler folder, its `_venv.sh`, and a working `.venv` | **Set up Python**: the same uv Python, then sched's own `_venv.sh` with `SCHED_PYTHON` set to it |

Homebrew's Python is never used: on a Mac where `/opt/homebrew` belongs to another user it can
hang. A `.venv` counts as broken when it was made with Homebrew's Python, its Python doesn't
start, its program is missing, or it was made in another folder (the program's first line points
elsewhere).

**Folder access.** A project copied from another macOS user often keeps its files but loses the
owner's own permissions (`d---r-x---`); it works through ACLs until an edit drops them.
Setup spots a folder or program like that and offers **Repair access**, which only adds your own
permissions (`chmod -R u+rwX`). A folder that belongs to another user can't be repaired from
here; copy it into your home folder and choose the copy.

**Folders elsewhere.** Every project row has **Choose Another Folder…**; the choice is saved in
`app.json` and checked straight away. A project that isn't on this Mac at all has to be copied
here first (orcastrator and sched have no public download yet).

When everything is ready, **Start noskill** saves `app.json` and starts all the parts. With
something still missing, **Start What's Ready** does the same for the parts that are installed.

## What you get
- **One start button.** Start All (on Overview and in the menu bar) brings up Ollama, the noskill
  agent, the research server, the task scheduler and its web page, in that order, each only once
  its folder and program are found. Stop All stops what the app started and leaves alone anything
  you started yourself.
- **One window.** The sidebar has Overview, Setup, Agent, Research, Tasks, Ollama and Logs:
  - **Overview** lists every part with its state (running, stopped, starting, failed or not
    installed), its port, and Start, Stop and Restart buttons.
  - **Agent**, **Research** and **Tasks** show each project's own page inside the app. Their
    toolbar's Start, Stop and Restart act on that part only: stopping Research leaves the Agent
    running, and the other way round. Tasks covers both sched parts (its runner and its page).
  - **Ollama**: see below.
  - **Logs** shows the last output of each part the app started, Setup's fixes and model
    downloads.
- **A menu-bar icon** that shows whether everything is up, with Start All, Stop All, Setup and
  Open noskill.

## Ollama
The Ollama pane is for people who've never used it:
- **Install** Ollama (the same fix as Setup's) or start and stop it.
- **Models**: what's installed and how big, the recommended models still missing, and a box to
  download any model by name, with progress and Stop.
- **ollama.com account**: Sign In runs `ollama signin` and opens the link it prints in your
  browser; Sign Out runs `ollama signout`. Only needed for Ollama's cloud models. Both talk to
  ollama.com, so they run only when pressed.
- **Find a model**: a built-in guide to popular models in plain words (what each is good for,
  its size, and a warning when it needs more memory than this Mac has), with Download or Use in
  Chat. **Favourites** are the ones you star; **Recent** the ones you downloaded or chatted with
  lately. Nothing here goes online; a link to ollama.com's library is there for more.
- **Chat** with any installed model, streamed as it answers, with Stop and Clear.

## How it decides a part is running
The app checks each port about every 3 seconds. A part counts as running when its port answers,
whether the app started it or something else did (for example `noskill daemon install`, or the
scheduler's login agents from its `./start.sh`). The app never starts a second copy on a port that
is already in use.

## What each part needs
The app runs each project's own programs:

| Part | The app runs |
|---|---|
| Ollama | `ollama serve`, from `/usr/local/bin`, `/opt/homebrew/bin`, `/Applications/Ollama.app`, `~/Applications/Ollama.app` or `~/.local/bin` |
| Agent | `~/.local/bin/noskill serve` |
| Research | `.venv/bin/orcastrator-web --port 8400` |
| Tasks | `.venv/bin/sched daemon` and `.venv/bin/sched serve --port 8787` |

## Settings
The project folders and ports are in `~/Library/Application Support/noskill/app.json`, written
when you finish Setup or choose a folder:

| Key | Default |
|---|---|
| `noskillBinary` | `~/.local/bin/noskill` |
| `noskillDir` | `~/dev/noskill` (only used to build the agent when the app has none inside) |
| `orcastratorDir`, `schedulerDir` | `~/dev/noskill/apps/orcastrator`, `~/dev/noskill/apps/scheduler` |
| `ollamaPort`, `agentPort`, `researchPort`, `tasksPort` | 11434, 11435, 8400, 8787 |
| `startAllOnLaunch` | `true`; set `false` to press Start All yourself |

A missing key takes its default, so an older file still loads; a file that doesn't parse gives
the defaults and is left as it is. Favourite and recent models are kept in the app's own
preferences, not here.

Quitting the app stops what it started. If the app crashes instead, what it started keeps running;
the next launch shows those parts as started outside the app, and you stop them yourself.

## Checking it
- `apps/macos/test.sh`: the unit tests (detection, the fixes and their scripts, models, panes;
  well under a minute). `build-app.sh` runs them first and won't build when one fails.
- `apps/macos/make-dmg.sh`: builds, packages, mounts the DMG, verifies the signatures and starts
  the app from it with `--self-test` (prints what Setup would show and checks the agent inside
  runs; changes nothing).
- `NoskillApp --snapshot DIR`: saves a picture of the Setup, Overview, Ollama and Logs panes the
  app draws itself, then quits; for checking the window where screen recording isn't allowed.
  Starts and changes nothing. (The sidebar and web pages don't show in these pictures.)

## What it doesn't do yet
- It's for macOS only. On Windows and Linux use `noskill-tray` and each project's own start
  script.
- It can't fetch orcastrator or sched onto a Mac that doesn't have them.
- It isn't signed with a Developer ID or notarized (see "Install from the DMG").
