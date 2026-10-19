# noskill

> Your local model starts with no skills. It learns them.

## Install
No release is published yet, so for now you install from a clone (the repo is private, so `git` must be signed in to GitHub, e.g. `gh auth login`). The installer builds noskill, and first installs **Go**, **Ollama** and the recommended models (`granite4:3b`, `gemma3:1b`) if they are missing, asking before each download.

**macOS / Linux**
```bash
git clone https://github.com/ycechungAI/noskill && cd noskill && ./start.sh install
```

**Windows (PowerShell)**
```powershell
git clone https://github.com/ycechungAI/noskill; cd noskill; .\start.cmd install
```

Open a new terminal afterwards (and add `~/.local/bin` to your `PATH` on macOS/Linux if it says so). Then `noskill ui` opens the status page, and `./start.sh` (Windows: `start.cmd`) is a menu of everyday commands ([guide](docs/START.md)). `./start.sh ollama` does just the Ollama-and-models part. To do it by hand: `CGO_ENABLED=0 go build -o <folder>/noskill ./cmd/noskill && noskill init`. Once a release exists, `scripts/install.sh` (macOS/Linux) and `scripts/install.ps1` (Windows) will download, verify and install it for you ([PACKAGING.md](docs/PACKAGING.md)).

noskill is a planned runtime that gives **local models** (Ollama, LM Studio, llama.cpp, MLX or any OpenAI-compatible server) **skills**: web search, research, files, running code, scheduled jobs. It also lets them **learn new skills** from their own runs. It's one binary for macOS, Windows and Linux, written in Go, MIT licensed.

> **Status: Phase 3 and in use on macOS.** The runtime, learning loop, skill optimisation, scheduler, anime-tracker, signed skill registry and an opt-in LoRA pipeline are built and tested on macOS against a real Ollama. Linux and Windows build and vet cleanly but haven't been run on real machines. No release, installer or website is published yet; the GitHub org, signing key and domain are decided last ([PACKAGING.md](docs/PACKAGING.md)). This repo also holds the spec, the design docs and the Phase 0 test harness (`spikes/`, Python).

## What it does
- **Add skills to any local model.** Skills are agentskills.io-compatible `SKILL.md` packages. They work on models with native tool calling and, through constrained output, on models without it.
- **Learn skills.** When a multi-step task succeeds, noskill drafts a skill and tests it. It asks you before using it the first time, then uses it automatically after 3 successful runs. Skills that keep failing get refined or retired.
- **Route between your models.** It picks the right installed model per request (code, vision, quick chat) within your machine's memory budget, and learns from outcomes which model works best.
- **Run jobs on a schedule** in the background, with approvals for anything risky. The first example is an anime tracker: it suggests new shows, waits for your approval, adds qBittorrent RSS rules, then renames and files downloads.
- **Plug into existing apps.** It serves an OpenAI- and Ollama-compatible API on `:11435`, so Open WebUI, Continue and similar clients get skills by changing one URL.
- **Use JEV decision models** (Ollama `/v1/systemone`) for fast routing, judging and safety checks, with probabilities. Without one, the chat model answers instead.

Design target: half of a 16 GB machine. Models get 6 GB (a 3B–4B chat model next to a small decision model) and grow toward ~10.7 GB only while the rest of the machine is free ([ADR-0012](docs/adr/0012-shared-machine-6gb-budget-with-burst.md)).

## Commands
Not sure where to start? `./start.sh` (Windows: `start.cmd`) is a menu of the everyday commands, with `--help`; the guide is [docs/START.md](docs/START.md).

```bash
noskill ui                   # the status page: approvals, downloads, history
noskill init                 # find backends and models, benchmark them
noskill run "research the best NAS for Plex under $500 and cite sources"
noskill skills list          # built-in and learned skills, with success rates
noskill schedule add anime-tracker --cron "0 3 * * *"
noskill approvals            # approve picks, learned skills and risky actions
noskill serve                # OpenAI/Ollama-compatible API on :11435
noskill skills install team/pdf-tools   # from a signed registry you added (noskill registry add)
```

## Phase 0 findings so far
Measured on a 16 GB Mac mini M4:
- **Tool calling** ([S1](docs/spikes/S1-results.md)): native tool calls beat grammar-constrained ones on `llama3.2:3b` (64% vs 48% success) and were close on `qwen3.5:4b`. Native is the default, constrained the fallback ([ADR-0005](docs/adr/0005-native-tool-calling-default.md)).
- **Proxy** ([S2](docs/spikes/S2-results.md)): one port serving the OpenAI and Ollama APIs works with Open WebUI (both connection types), the OpenAI SDK and the `ollama` CLI, adding under 1 ms. Open WebUI sends ~11k tokens of its own tools per request, so noskill caps client tools to a budget.
- **Memory** ([S4](docs/spikes/S4-results.md)): on 16 GB, a 4B or a 7B coder model fits next to the resident decision model; `qwen3:8b` and a vision 7B push macOS into memory-pressure warnings. Swapping a 7–8B model takes 4–7 s.
- **Decision models** ([S3](docs/spikes/S3-results.md)): `tev1` (4B) is accurate (92.5% task type) but too slow and memory-hungry for 16 GB. `tev1:0.8b` asking one question is 81.5% right in ~235 ms and is the 16 GB default. Ollama's prompt cache grows up to ~8.5 GB per model, so noskill reloads the decision model to cap it ([ADR-0006](docs/adr/0006-decision-model-on-16gb.md)).

- **Script sandbox** ([S9](docs/spikes/S9-results.md), macOS): a generated `sandbox-exec` profile blocked all 24 attacks (reading `~/.ssh`, escaping the workspace, direct network, DNS, fork bombs, …) with no admin rights, while Python and Node work ran through the egress proxy. It adds ~45 ms per call. Linux and Windows aren't tested yet.

Phase 0 ended in a **conditional go** ([PHASE0_GO_NO_GO.md](docs/spikes/PHASE0_GO_NO_GO.md)). Progress is tracked in [ROADMAP.md](docs/ROADMAP.md).

## Docs
Read them as a site: `go run ./scripts/docsite` writes `site/` (static HTML, no JavaScript; open `site/index.html`, or `docs-site` in `.claude/launch.json` serves it on :8767). It fails on a broken link, so the docs can't point at files that aren't there. Nothing is hosted yet. **Writing a skill? Start with [SKILL_AUTHORING.md](docs/SKILL_AUTHORING.md).**

**The status page** (Overview, Approvals, Downloads, History): `./ask.sh` builds the current source, serves the page on `http://127.0.0.1:11436/ui` next to your daemon without running any jobs, and opens it (`./ask.sh downloads` opens that tab).

| Doc | Contents |
|---|---|
| [SPEC.md](SPEC.md) | Requirements and acceptance criteria |
| [VISION.md](docs/VISION.md) | Problem, goals, non-goals |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components and tech choices |
| [CLI.md](docs/CLI.md) | Command reference |
| [SKILL_AUTHORING.md](docs/SKILL_AUTHORING.md) · [SKILL_FORMAT.md](docs/SKILL_FORMAT.md) · [LEARNING.md](docs/LEARNING.md) | How to write a skill; the format; how skills are learned, optimised and retired |
| [ORCHESTRATOR.md](docs/ORCHESTRATOR.md) · [MODEL_ADAPTERS.md](docs/MODEL_ADAPTERS.md) | Model routing, memory budget, backends, decision models |
| [PROXY.md](docs/PROXY.md) · [SECURITY.md](docs/SECURITY.md) | Chat-app proxy; permissions and sandboxing |
| [PLATFORMS.md](docs/PLATFORMS.md) · [PACKAGING.md](docs/PACKAGING.md) | Per-OS install, daemon, scheduler; builds and signing |
| [CONFIG.md](docs/CONFIG.md) · [DATA_MODEL.md](docs/DATA_MODEL.md) | Settings; storage |
| [ROADMAP.md](docs/ROADMAP.md) · [PHASE0_TESTS.md](docs/PHASE0_TESTS.md) | Plan; Phase 0 experiments and pass/fail rules |
| [OPEN_QUESTIONS.md](docs/OPEN_QUESTIONS.md) · [adr/](docs/adr/) · [PRIOR_ART.md](docs/PRIOR_ART.md) | Decisions log, decision records, related projects |
| [skills/](skills/) | Specs for the first built-in skills |

## License
MIT. See [LICENSE](LICENSE).
