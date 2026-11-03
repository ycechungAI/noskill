# Project: Ollama Research Orchestrator (STORM Implementation)

> **Part of the noskill app.** This is the **Research** part of the noskill app (`~/dev/noskill`,
> see `docs/APP.md` there), which starts it together with the noskill agent (`~/dev/noskill`) and
> the task scheduler (`~/dev/scheduler`) and shows its page in one window. It still runs on its own
> with `./start.sh`.
>
> The app runs `.venv/bin/orcastrator-web --port 8400` from this folder and checks that port, so keep
> that entry point, its `--port` flag and the default port 8400 working, or update the app with them.

## 1. Overview
The goal of this project is to build a Python-based orchestration layer that sits on top of **Ollama**. The system will implement the **STORM (Stanford)** methodology—a systematic way to generate high-quality, long-form research reports by breaking down complex topics into sub-questions, researching them independently, and synthesizing the results.

## 2. System Architecture
The orchestrator follows a **Multi-Agent System (MAS)** design where different agents have specific roles, constraints, and goals.

### The Agent Roles:
1.  **Planner:** Analyzes a broad topic and generates a list of 5–10 granular research questions.
2.  **Researcher:** Answers individual sub-questions with deep technical detail (Running in parallel).
3.  **Critic:** Reviews the researcher's output for hallucinations, missing data, or logical gaps.
4.  **Synthesizer/Writer:** Takes all verified research points and weaves them into a cohesive, structured Wikipedia-style article.

---

## 3. The STORM Workflow
The orchestrator will manage the state of the following pipeline:

| Stage | Process | Input | Output |
| :--- | :--- | :--- | :--- |
| **Decomposition** | Planner Agent | User Prompt | List of Research Questions |
| **Researching** | Researcher Agents (Parallel) | Sub-questions | Raw Data/Facts per question |
| **Review** | Critic Agent | Raw Data | Validated/Refined Data |
| **Synthesis** | Writer Agent | Validated Data | Final Markdown Report |

---

## 4. Technical Stack
*   **LLM Engine:** Ollama (Running models like `llama3`, `mistral`, or `phi3`).
*   **Language:** Python 3.10+.
*   **Orchestration Framework:** 
    *   *Option A (High Control):* **LangGraph** (State-machine based).
    *   *Option B (Ease of Use):* **CrewAI** or **Autogen**.
*   **Concurrency:** `asyncio` for parallel API calls to Ollama.

---

## 5. Implementation Strategy

> The snippets below are illustrative. The real, corrected implementation lives in
> [`orcastrator/agents.py`](orcastrator/agents.py) and [`orcastrator/orchestrator.py`](orcastrator/orchestrator.py).
> The original sketch here had three bugs that are worth calling out explicitly:
> 1. `Agent.generate` called the **synchronous** `ollama.generate`, so `asyncio.gather` was
>    handed already-completed values instead of coroutines — the "parallel" research would
>    actually run one call at a time, blocking the event loop each time. Fixed by using
>    `ollama.AsyncClient` and an `async def generate`.
> 2. `self.state["questions"] = plan` assigned the Planner's raw string reply directly, so
>    `for q in self.state["questions"]` iterated over characters, not questions. Fixed with a
>    `_parse_questions()` step that splits the reply into a real list.
> 3. The `Critic` role was described in Section 2 but never appeared in the orchestrator class.
>    Fixed by adding a critic-in-the-loop validation step per question (Section 6's "Accuracy Loop").

### A. Agent Wrapper Class
Every agent is an instance of a class that standardizes how we communicate with the local LLM.

```python
import ollama

class Agent:
    def __init__(self, role, system_prompt, model="llama3.2", host=None):
        self.role = role
        self.system_prompt = system_prompt
        self.model = model
        self._client = ollama.AsyncClient()

    async def generate(self, prompt):
        response = await self._client.generate(
            model=self.model,
            system=f"You are a {self.role}. {self.system_prompt}",
            prompt=prompt,
        )
        return response["response"]
```

### B. The Orchestrator (State Management)
The orchestrator manages the flow of information between agents, including the Critic's
accuracy loop, and maintains the "State."

```python
import asyncio

class StormOrchestrator:
    def __init__(self, model="llama3.2", host=None):
        self.planner = Agent("Planner", "Break down a topic into 5-10 specific research questions.", model=model)
        self.researcher = Agent("Researcher", "Provide detailed, factual information on a given sub-topic.", model=model)
        self.critic = Agent("Critic", "Flag hallucinations, missing data, or logical gaps in a research answer.", model=model)
        self.writer = Agent("Writer", "Synthesize research into a structured report with clear headings.", model=model)
        self.state = {"query": "", "questions": [], "results": []}

    async def _research_and_validate(self, question):
        answer = await self.researcher.generate(question)
        verdict = await self.critic.generate(f"Question: {question}\nAnswer: {answer}")
        if verdict.strip().startswith("REVISE"):
            answer = await self.researcher.generate(f"{question}\n\nFix this issue: {verdict}")
        return answer

    async def run(self, topic):
        # 1. Plan
        plan_raw = await self.planner.generate(f"Break down the topic: {topic}")
        self.state["questions"] = parse_questions(plan_raw)  # split reply into a list

        # 2. Research + Critic validation (Parallel Execution)
        tasks = [self._research_and_validate(q) for q in self.state["questions"]]
        self.state["results"] = await asyncio.gather(*tasks)

        # 3. Synthesis
        final_report = await self.writer.generate(f"Write a report based on this: {self.state['results']}")
        return final_report
```

---

## 6. Advanced Features & Optimizations

### Parallelism vs. Sequence
Because research tasks are independent, the Orchestrator must use `asyncio.gather` to run multiple Ollama requests at once (provided your GPU/Hardware supports concurrent inference).

### Context Management
Large research topics can exceed the context window of local models. 
*   **Implementation:** The Orchestrator will not pass *all* raw research to the Writer. Instead, it will pass a **summarized version** of each research point to ensure the final prompt fits within the token limit.

### Accuracy Loop (The Critic)
To mirror the STORM methodology accurately:
1.  Researcher provides an answer.
2.  Critic checks for accuracy.
3.  If the Critic finds errors, the Orchestrator triggers a "Re-research" step for that specific question only.

---

## 7. Development Roadmap
- [x] **Phase 1:** Basic Ollama API connection and single-agent prompt testing (`orcastrator/agents.py`).
- [x] **Phase 2:** Parallel execution logic using `asyncio` (`orcastrator/orchestrator.py`).
- [x] **Phase 3:** State Management (passing data between Planner $\rightarrow$ Researcher $\rightarrow$ Critic $\rightarrow$ Writer).
- [x] **Phase 4:** Markdown output (`--output`). PDF export intentionally deferred — see [`Plan.md`](Plan.md).

Beyond the original roadmap, the orchestrator also has per-request timeout + retry, an
`[unverified]` fallback when the Critic never approves an answer, a character budget that
truncates oversized research before the Writer call, optional progress logging (`--verbose`),
and a `pytest` suite (`tests/`). See [`Plan.md`](Plan.md) and [`Todo.md`](Todo.md) for status.

## 8. Usage

### Quick start

```bash
./start.sh
```

One command does everything: creates the virtualenv and installs dependencies
on first run, starts the Ollama daemon if it isn't already running (and stops
it again on Ctrl-C if the script started it), launches the web UI, and opens
http://127.0.0.1:8400 in your browser. Any `orcastrator-web` flag passes
through, e.g. `./start.sh --port 9000`.

### Web UI (manual)

```bash
pip install -e .
orcastrator-web            # then open http://127.0.0.1:8400
```

The page has two modes, switched by the **Research mode** toggle at the top:

- **Research on (default):** the STORM pipeline. Type a topic, pick any model
  installed in Ollama, and optionally tick **Extended thinking** — unchecked
  runs the fast preset (~1–2 min); checked runs a deeper five-stage pipeline
  (~5–10 min): each answer gets a challenge-and-rebuttal debate round on top
  of critic review, and the report goes through draft → editorial review →
  rewrite instead of a single writing pass. The stage tracker grows to
  Plan → Research → Draft → Review → Rewrite accordingly.
- **Research off:** a plain chatbot. Chat directly with the selected model;
  replies stream in token-by-token and the conversation keeps its history.

While a report generates you watch a live progress dashboard:

- an **elapsed timer** counting up for the whole run,
- a **stage tracker** (Plan → Research → Write) that lights up and check-marks
  each stage as it completes,
- a **per-question status list** — every research question shows a live badge
  (Researching → Reviewing → Approved / Revising / Unverified / Done) with a
  running "N / total completed" counter,
- a collapsible **activity log** with timestamped detail.

When it finishes, the rendered Markdown report appears with copy/download
buttons. Advanced settings (Ollama URL, timeout, retries) are tucked behind a
collapsible section. Use `--port`/`--bind` to change where the server listens
(it binds to localhost only by default).

### Speed presets

Wall-clock time scales with questions × LLM calls per question × tokens per
reply, so the presets trim those:

| Preset | Questions | Critic review | Answer cap | Rough time (llama3.2) |
| :--- | :--- | :--- | :--- | :--- |
| `fast` (web default; Extended thinking off) | 4 | off | 300 tokens | ~1–2 min |
| `balanced` (CLI default) | 6 | on | 500 tokens | ~3 min |
| `thorough` | up to 10 | on | unbounded | ~5 min |
| `extended` (web: Extended thinking on) | up to 8 | on + challenge/rebuttal | 600 tokens | ~5–10 min |

The `extended` preset also replaces the single writing pass with a
draft → editorial review → rewrite cycle (a Reviewing Editor agent critiques
the whole draft, then the Writer applies the feedback).

### Backends: Ollama, LM Studio, and Jan.ai

Both the pipeline and chat mode run against any of the three local servers —
pick one in the web UI's **Server** dropdown or with
`--backend ollama|lmstudio|jan` on the CLI:

- **Ollama** (default) — native API at `$OLLAMA_HOST` / `http://localhost:11434`.
- **LM Studio** — OpenAI-compatible API at `$LMSTUDIO_HOST` /
  `http://localhost:1234`. Enable it in LM Studio under Developer → "Start
  server". Inline `<think>…</think>` blocks from reasoning models are stripped
  from pipeline replies and hidden in chat; if a capped reply comes back empty
  because reasoning consumed the whole token budget, it is retried uncapped.
  LM Studio doesn't report model memory, so research runs 2 tasks at a time
  (override with `--max-concurrency`).
- **Jan.ai** — OpenAI-compatible API at `$JAN_HOST` / `http://localhost:1337`.
  Enable it in Jan under Settings → "Local API Server" (Jan has no CLI to start
  the server, so a clear error is raised if it isn't running). It behaves like
  LM Studio: `<think>` blocks are stripped, an empty capped reply is retried
  uncapped, and model memory isn't reported (2 tasks at a time by default).

### Grounded research (citations & verified links)

On by default (web UI "Web research" checkbox; disable with `--no-web`): each
research question is searched across a set of **keyless public sources** and the
results are merged and deduplicated. The default engines are:

- **Wikipedia** — encyclopedia articles (lead-section extracts).
- **OpenAlex** — scholarly papers with abstracts and DOI links.
- **DuckDuckGo Instant Answers** — curated abstracts (Britannica, Wikipedia, …).
- **DuckDuckGo web search** — real ranked result links + snippets (a search
  engine, not just one abstract; keyless but can be rate-limited).
- **Stock quotes** — live prices for equities/ETFs/crypto/indices via Yahoo
  Finance's public endpoints, so "what's the price of X" gets a real-time
  answer instead of a stale figure from the model's training data.
- **arXiv** — scholarly preprints with abstracts.
- **Semantic Scholar** — scholarly papers, citations, influential works.
- **Stack Exchange** — Q&A (default site: Stack Overflow).
- **Hacker News** — tech news/discussion via the Algolia API.

Pick which engines to use with `--sources` on the CLI (comma-separated) or the
**Research sources** toggles in the web UI. Example:
`--sources wikipedia,ddg,arxiv`. Unknown names are ignored with a warning; omit
the flag to use all engines. This is handy for dropping a fragile engine (e.g.
the DuckDuckGo HTML scraper) or narrowing to scholarly-only sources.

The Researcher must answer **only from those numbered sources**, citing each
claim like `[1]`. The Critic verifies claims against the same sources and
rejects unsupported ones.

Citations render as **clickable inline links** — each `[n]` in the report body
links straight to that source — and the report ends with a **References**
section built programmatically from the fetched URLs. The model never invents
links: a URL only appears after its content was downloaded (Wikipedia) or its
liveness was checked over HTTP (papers, encyclopedias), so every reference is
live and every fact is traceable. Questions with no usable sources are answered
from model knowledge and explicitly marked *unsourced*.

Uncheck **Inline citations** (or pass `--no-citations`) to drop the `[n]`
markers from the body for clean prose while keeping the References section.

### Ollama cloud models

Ollama can serve cloud-hosted models (e.g. `gpt-oss:120b-cloud`,
`deepseek-v3.1:671b-cloud`) through the local daemon after `ollama signin`.
They appear in the web UI's model picker under "Ollama Cloud" and work with
`--model <name>-cloud` on the CLI; Orcastrator registers the model with the
daemon automatically on first use. Cloud models skip the local memory check
(they don't use your RAM) and run research at full parallelism.

### Model handling

- **Thinking models** (e.g. gemma4): detected via Ollama's capabilities API and
  run with hidden reasoning disabled — thinking tokens count against the answer
  caps and can otherwise consume the whole budget, returning empty replies.
- **Memory-aware concurrency:** the research fan-out is sized from the model's
  measured memory footprint vs system RAM (<25% → 4 at a time, <45% → 2,
  otherwise 1), so a large model that Ollama can only serve serially isn't
  flooded with parallel requests that queue past the timeout. Override with
  `--max-concurrency`.

### Command line

```bash
pip install -e .          # or: pip install -r requirements.txt
orcastrator "The history of the Roman aqueducts" --model llama3.2 -o report.md
# equivalently: python -m orcastrator.cli "..."

# quicker, lighter run (4 questions, no critic pass, short answers)
orcastrator "The water cycle" --preset fast

# watch the pipeline progress
orcastrator "The water cycle" -v

# point at a non-default Ollama daemon (or set $OLLAMA_HOST)
orcastrator "..." --host http://192.168.1.50:11434

# narrow the web research to specific engines (see "Grounded research")
orcastrator "..." --sources wikipedia,ddg,arxiv
```

### Environment variables
- `OLLAMA_HOST` — Ollama daemon URL when `--host` is not passed (default `http://localhost:11434`).
- `LMSTUDIO_HOST` — LM Studio server URL when `--host` is not passed (default `http://localhost:1234`).
- `JAN_HOST` — Jan.ai server URL when `--host` is not passed (default `http://localhost:1337`).
- `RUN_OLLAMA_TESTS=1` — opt in to the live integration test (`tests/test_integration.py`).
- `OLLAMA_TEST_MODEL` — model used by that integration test (default `llama3.2`).

## 9. Development

```bash
pip install -e ".[dev]"
pytest                    # unit tests only (live integration test auto-skips)
RUN_OLLAMA_TESTS=1 pytest # also runs the end-to-end test against a live daemon
```