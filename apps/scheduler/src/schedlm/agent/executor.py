"""The tool-calling agent loop. See Agents.md §2 and §4 (pseudocode)."""

from __future__ import annotations

import logging
import time
from datetime import datetime
from pathlib import Path
from typing import Callable

import httpx
import ollama

from schedlm import daemon as daemon_mod
from schedlm import models as models_mod
from schedlm import schedule as schedule_mod
from schedlm.agent import prompts
from schedlm.agent.tools import ToolContext, execute, schemas
from schedlm.config import Config
from schedlm.report import RunResult
from schedlm.tasks import Task

logger = logging.getLogger(__name__)

FINALIZE_PROMPT = (
    "Iteration limit reached. Write the final report now using only the "
    "information gathered above."
)

# Sent once if the model tries to finish a research task without having
# called a single tool -- small models otherwise "write the briefing" from
# memory and invent the news, URLs included.
RESEARCH_TOOLS = {"web_search", "fetch_page"}
NO_TOOLS_NUDGE = (
    "You have not used any tools yet, so this report would not be based on "
    "current sources. Call web_search and/or fetch_page now to gather the "
    "information the task asks for, then write the report."
)
NO_SOURCES_WARNING = (
    "> **Warning:** this report was written without any web searches or page "
    "fetches, so its facts are unverified."
)

# Ollama dropping a connection mid-response (crash, restart, app update) is
# retried once after this pause instead of failing the whole run.
RETRY_DELAY_SECONDS = 5

_EVENT_PREVIEW_CHARS = 200


def _make_compressor(client, model: str) -> Callable[[str, str], str]:
    """(page_text, source_url) -> shorter text, via one extra chat call using
    cfg.ollama.planner_model (Agents.md §5). Any failure here propagates to
    fetch_page's own try/except, which hard-truncates instead."""

    def compress(text: str, url: str) -> str:
        response = client.chat(
            model=model,
            messages=[
                {"role": "system", "content": prompts.COMPRESSOR_SYSTEM},
                {"role": "user", "content": f"URL: {url}\n\n{text}"},
            ],
        )
        return response.message.content or text

    return compress


def _preview(text: str) -> str:
    text = text.strip().replace("\n", " ")
    if len(text) > _EVENT_PREVIEW_CHARS:
        return text[:_EVENT_PREVIEW_CHARS] + "..."
    return text


def _add_no_sources_warning(report: str) -> str:
    """Put the warning right under the report's H1 title (or first, if none)."""
    first, _, rest = report.partition("\n")
    if first.startswith("# "):
        parts = [first, NO_SOURCES_WARNING, rest]
    else:
        parts = [NO_SOURCES_WARNING, report]
    return "\n\n".join(p.strip("\n") for p in parts if p.strip())


def run_task(
    task: Task,
    cfg: Config,
    client=None,
    on_event: Callable[[str], None] | None = None,
) -> RunResult:
    """Run one task through the tool-calling loop.

    `on_event`, if given, is called with a human-readable line for every
    notable step (tool call, tool result, model thinking, finalization) --
    every event is also always logged via this module's logger regardless,
    so scheduled (daemon-triggered) runs get the same visibility in
    data/sched.log even when no on_event callback is passed.
    """
    if client is None:
        client = ollama.Client(host=cfg.ollama.host)

    def emit(text: str) -> None:
        logger.info("[%s] %s", task.id, text)
        if on_event is not None:
            on_event(text)

    model, note = models_mod.resolve_model(
        client, task.model or cfg.ollama.model, needs_tools=bool(task.tools)
    )
    if note:
        logger.warning("[%s] %s", task.id, note)
        emit(note)
    emit(f"using model {model}")
    compressor_model, _ = models_mod.resolve_model(
        client, cfg.ollama.planner_model, needs_tools=False
    )
    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)

    messages: list = [
        {
            "role": "system",
            "content": prompts.EXECUTOR_SYSTEM.format(now=now.isoformat(), tz=str(tz)),
        },
        {"role": "user", "content": task.prompt},
    ]
    tools = schemas(task.tools)

    ctx = ToolContext(
        notes_dir=Path(cfg.paths.data) / "notes",
        fetch_max_chars=cfg.agent.fetch_max_chars,
        search_max_results=cfg.agent.search_max_results,
        compress=_make_compressor(client, compressor_model),
    )

    # Extended "thinking" output is opt-in per config but not every model
    # supports it (Ollama raises a 400 ResponseError if you ask a model
    # that can't do it) -- try once, fall back silently for the rest of
    # this run if it's rejected.
    think_enabled = cfg.agent.think

    def chat_once(**kwargs):
        nonlocal think_enabled
        if think_enabled:
            try:
                return client.chat(think=True, **kwargs)
            except ollama.ResponseError as e:
                if "not support thinking" in str(e).lower():
                    think_enabled = False
                    emit("model does not support extended thinking; continuing without it")
                else:
                    raise
        return client.chat(**kwargs)

    def chat(**kwargs):
        try:
            return chat_once(**kwargs)
        except (httpx.TransportError, ConnectionError) as e:
            emit(f"lost connection to Ollama ({e}); retrying once in {RETRY_DELAY_SECONDS}s")
            time.sleep(RETRY_DELAY_SECONDS)
            daemon_mod.ensure_ollama_running(cfg)
            return chat_once(**kwargs)

    needs_research = bool(RESEARCH_TOOLS & set(task.tools))
    nudged = False

    def finish(report: str, iterations: int) -> RunResult:
        if needs_research and tool_call_count == 0:
            emit("warning: model finished without using any tools; report facts are unverified")
            report = _add_no_sources_warning(report)
        return RunResult(
            report=report, iterations=iterations, tool_calls=tool_call_count, model=model
        )

    tool_call_count = 0
    for i in range(cfg.agent.max_iterations):
        response = chat(model=model, messages=messages, tools=tools)
        message = response.message
        messages.append(message)

        if getattr(message, "thinking", None):
            emit(f"thinking: {_preview(message.thinking)}")

        calls = message.tool_calls or []
        if not calls:
            if needs_research and tool_call_count == 0 and not nudged:
                nudged = True
                emit("model tried to finish without using any tools; asking it to research first")
                messages.append({"role": "user", "content": NO_TOOLS_NUDGE})
                continue
            emit("final report ready")
            return finish(message.content or "", i + 1)

        for call in calls:
            tool_call_count += 1
            emit(f"calling {call.function.name}({dict(call.function.arguments)})")
            result = execute(call.function.name, dict(call.function.arguments), ctx)
            emit(f"{call.function.name} -> {_preview(result)}")
            messages.append(
                {"role": "tool", "name": call.function.name, "content": result}
            )

    # Iteration cap hit: force finalization without tools (Agents.md §2).
    emit(f"iteration limit ({cfg.agent.max_iterations}) reached, forcing final report")
    messages.append({"role": "user", "content": FINALIZE_PROMPT})
    response = chat(model=model, messages=messages)
    return finish(response.message.content or "", cfg.agent.max_iterations)
