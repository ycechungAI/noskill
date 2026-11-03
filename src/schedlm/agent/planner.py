"""Natural-language sentence -> draft Task. See Agents.md §4."""

from __future__ import annotations

import json
import logging
import tempfile
from datetime import datetime
from pathlib import Path

import ollama
import yaml

from schedlm import models as models_mod
from schedlm import schedule as schedule_mod
from schedlm.agent import prompts
from schedlm.config import Config
from schedlm.tasks import Task, TaskValidationError, parse_task_file

logger = logging.getLogger(__name__)

MAX_ATTEMPTS = 2


class PlannerError(Exception):
    """Raised when the planner cannot produce a valid task after retrying."""


def _extract_json(text: str) -> dict:
    """Parse the model's reply as JSON. Strips markdown fences if present --
    smaller models sometimes wrap JSON in ```json ... ``` despite instructions
    not to."""
    text = text.strip()
    if text.startswith("```"):
        text = text.strip("`")
        if text.lower().startswith("json"):
            text = text[4:]
        text = text.strip()
    data = json.loads(text)
    if not isinstance(data, dict):
        raise ValueError("reply is not a JSON object")
    return data


def _draft_to_task_file_text(data: dict) -> str:
    """Render the planner's JSON reply as task-file frontmatter + body, so it
    can be validated by reusing tasks.py's real parser/validator below rather
    than duplicating its rules here."""
    frontmatter: dict = {}
    for key in ("id", "name", "schedule", "fire_at", "enabled", "model", "tools"):
        if key in data and data[key] is not None:
            frontmatter[key] = data[key]
    prompt = data.get("prompt")
    if not isinstance(prompt, str):
        prompt = ""
    return (
        "---\n"
        + yaml.safe_dump(frontmatter, sort_keys=False)
        + "---\n\n"
        + prompt.strip()
        + "\n"
    )


def nl_to_task(sentence: str, cfg: Config, client=None) -> Task:
    if client is None:
        client = ollama.Client(host=cfg.ollama.host)

    tz = schedule_mod.resolve_tz(cfg)
    now = datetime.now(tz)
    messages: list = [
        {
            "role": "system",
            "content": prompts.PLANNER_SYSTEM.format(now=now.isoformat(), tz=str(tz)),
        },
        {"role": "user", "content": sentence},
    ]
    model, note = models_mod.resolve_model(client, cfg.ollama.planner_model, needs_tools=False)
    if note:
        logger.warning("planner: %s", note)

    last_error: Exception | None = None
    for attempt in range(MAX_ATTEMPTS):
        response = client.chat(model=model, messages=messages)
        content = response.message.content or ""
        try:
            data = _extract_json(content)
            file_text = _draft_to_task_file_text(data)
            with tempfile.NamedTemporaryFile(
                mode="w", suffix=".md", delete=False
            ) as tmp:
                tmp.write(file_text)
                tmp_path = Path(tmp.name)
            try:
                # task.path here is a temp file that's about to be deleted;
                # the caller (cli.py) must replace it with the real
                # destination before writing the task for real.
                return parse_task_file(tmp_path)
            finally:
                tmp_path.unlink(missing_ok=True)
        except (json.JSONDecodeError, ValueError, TaskValidationError) as e:
            last_error = e
            logger.warning("planner reply invalid (attempt %d): %s", attempt + 1, e)
            messages.append({"role": "assistant", "content": content})
            messages.append(
                {
                    "role": "user",
                    "content": (
                        f"Your previous reply was invalid: {e}. "
                        "Reply with only the corrected JSON object."
                    ),
                }
            )

    raise PlannerError(
        f"The planner could not produce a valid task definition after "
        f"{MAX_ATTEMPTS} attempts ({last_error}). Please write the task file "
        f"by hand instead (see tasks/*.md for examples)."
    )
