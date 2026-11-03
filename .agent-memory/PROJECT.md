# sched (scheduler)

A single-machine scheduler that runs LLM-powered tasks at scheduled times using local Ollama
models, with web search ability. Each run produces a Markdown report on disk. No cloud LLM APIs
— the only network access is web search / page fetching over HTTP. Think "local, private
scheduled Claude tasks": a task is a Markdown file with a prompt + cron schedule; a daemon wakes
up, an agent loop (Ollama model + tools) executes it, result lands in `runs/<task-id>/<ts>.md`.

## Stack

- Python, `pyproject.toml`, `_venv.sh` / `start.sh` bootstrap the venv
- Document map (read in this order per the README): `spec.md` → `Architecture.md` →
  `DataModel.md` → `Agents.md` → `API.md` → `Testing.md`
- `src/` — implementation; `data/` — task/db storage; `runs/` — generated reports
- Three agents (Executor, Planner, Compressor), each a single Ollama chat-call pattern — see
  `Agents.md` for the exact model table and verbatim system prompts.

## Conventions

- No frameworks/LangChain — agents are plain Ollama chat calls per `Agents.md`.
- Small-model instruction-following under tool failure is a known weak point (see `Agents.md`'s
  "Reliability note" on `llama3.2:3b` hallucinating after a tool reports failure) — prefer
  `qwen3:8b`+ over the low-RAM fallback column when report trustworthiness matters.
- Full rules live in `Agents.md` at the repo root — this file is the orientation summary.
