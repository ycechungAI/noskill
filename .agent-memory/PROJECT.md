# noskill

A planned runtime that gives local models (Ollama, LM Studio, llama.cpp, MLX, or any
OpenAI-compatible server) skills — web search, research, files, running code, scheduled jobs —
and lets them learn new skills from their own runs. One binary, MIT licensed, targets macOS,
Windows and Linux.

Status: Phase 3, in active use and tested on macOS against a real Ollama. Linux and Windows
build and vet cleanly but haven't been run on real machines. No release/installer/website
published yet.

## Stack

- Language: Go
- Source of truth for requirements: [SPEC.md](../SPEC.md) (requirement IDs like `FR-S3`) and
  [docs/](../docs/) (kept in sync with the code — `CONFIG.md`, `DATA_MODEL.md`, `PROXY.md`, etc.)
- Architecture decisions: `docs/adr/`
- Roadmap / what's next: [docs/ROADMAP.md](../docs/ROADMAP.md)
- Build/test: `go vet ./...` and `go test ./...`; vet all six OS/arch targets before finishing a
  change (see AGENTS.md for the loop).
- Module path is `noskill` — a placeholder until the GitHub org is chosen.

## Conventions

- Reference requirement IDs (e.g. `FR-O2`) in commits, PRs, and code comments where that
  requirement is implemented.
- If an implementation needs to deviate from the spec, update the spec in the same change (and
  add an ADR for architectural choices).
- Every feature must work on macOS, Windows and Linux — no POSIX-only assumptions outside
  `internal/platform/`.
- Design for small models first (3B–8B, 8k context); see AGENTS.md for which test models to use.
- Skills are agentskills.io-compatible; never break loading of plain `SKILL.md` files.
- No hard deletes of user files — use the OS trash.
- Full rules live in [AGENTS.md](../AGENTS.md) — read it alongside this file, don't duplicate it
  here. This file is the stable "what is this project" summary; AGENTS.md is the detailed,
  actively-maintained rulebook.
