# orcastrator (Ollama Research Orchestrator)

A Python orchestration layer on top of Ollama implementing a STORM-style (Stanford) multi-agent
research pipeline: Planner → parallel Researchers → Critic → Synthesizer/Writer, producing a
structured, Wikipedia-style long-form report from a topic. Has grown a chat mode with web search
and a live stock/crypto quote engine per recent commits.

## Stack

- Python, packaged with `pyproject.toml`; `requirements.txt` for deps
- `orcastrator/` — core package: `agents.py`, `backends.py`, `checkpoint.py`, `cli.py`,
  `export.py`, `orchestrator.py`, `retrieval.py`, `web.py`, `static/index.html` (web UI)
- `tests/` — pytest suite covering agents, backends, checkpoint, export, integration, parsing,
  research loop, retrieval, web
- `Plan.md` / `Todo.md` — planning docs tracked in the repo (check these for current priorities
  before assuming `.agent-memory/STATE.md` is the only source of "what's next")

## Conventions

- Multi-Agent System design: each role (Planner, Researcher, Critic, Synthesizer/Writer) has a
  distinct constrained responsibility — don't collapse roles together casually.
- Backed by local Ollama models, not cloud APIs.
