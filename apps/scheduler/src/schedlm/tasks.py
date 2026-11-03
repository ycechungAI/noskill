"""Task dataclass and task-file parsing/validation. See DataModel.md §1."""

from __future__ import annotations

import hashlib
import json
import logging
import re
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path

import yaml
from croniter import croniter

from schedlm.agent.tools import TOOL_SCHEMAS

logger = logging.getLogger(__name__)

ALL_TOOL_NAMES = [t["function"]["name"] for t in TOOL_SCHEMAS]
VALID_TOOLS = set(ALL_TOOL_NAMES)

_ID_RE = re.compile(r"[a-z0-9]+(-[a-z0-9]+)*")
_FRONTMATTER_RE = re.compile(r"\A---[ \t]*\n(.*?)\n---[ \t]*\n(.*)\Z", re.DOTALL)

_KNOWN_KEYS = {
    "id",
    "name",
    "schedule",
    "fire_at",
    "enabled",
    "model",
    "tools",
    "timeout_minutes",
    "catchup",
    "output",
    "sources",
    "checks",
}
_KNOWN_OUTPUT_KEYS = {"keep", "dir", "filename", "raw"}
_KNOWN_CHECK_KEYS = {"require_sources", "min_report_links", "require_cited_sources"}


class TaskValidationError(Exception):
    """Raised when a task file fails validation; message names the offending field."""


@dataclass
class OutputSpec:
    keep: int = 30
    dir: str | None = None
    filename: str | None = None
    raw: bool = False


@dataclass
class ChecksSpec:
    """Deterministic verification of a run (executor.py / verify.py).

    `require_sources` (default True when the task has `sources:`) fails the run
    unless every listed URL was fetched successfully. `min_report_links` fails
    the run unless the report cites at least that many distinct URLs.
    `require_cited_sources` fails the run whose report cites no URL the run
    actually fetched or found via search (the classic small-model fabrication:
    a table built from homepage snippets)."""

    require_sources: bool = True
    min_report_links: int | None = None
    require_cited_sources: bool = False


@dataclass
class Task:
    id: str
    name: str
    schedule: str | None
    fire_at: str | None
    enabled: bool
    model: str | None
    tools: list[str] = field(default_factory=lambda: list(ALL_TOOL_NAMES))
    timeout_minutes: int = 10
    catchup: bool = False
    output: OutputSpec = field(default_factory=OutputSpec)
    sources: list[str] = field(default_factory=list)
    checks: ChecksSpec = field(default_factory=ChecksSpec)
    prompt: str = ""
    path: Path = field(default_factory=Path)


def content_hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def parse_task_file(path: Path) -> Task:
    return parse_task_text(path.read_text(), path)


def parse_task_text(text: str, path: Path) -> Task:
    """Same validation as parse_task_file, but takes the text directly so
    callers (e.g. the web API) can validate content before it is written to
    disk. `path` is only used for warning messages and recorded on the
    returned Task -- it need not exist yet."""
    match = _FRONTMATTER_RE.match(text)
    if not match:
        raise TaskValidationError("frontmatter: missing leading '---' YAML block")
    fm_text, body = match.group(1), match.group(2)

    try:
        fm = yaml.safe_load(fm_text)
    except yaml.YAMLError as e:
        raise TaskValidationError(f"frontmatter: invalid YAML ({e})") from e
    if not isinstance(fm, dict):
        raise TaskValidationError("frontmatter: must be a YAML mapping")

    for key in fm:
        if key not in _KNOWN_KEYS:
            logger.warning("%s: unknown frontmatter key '%s'", path, key)

    body = body.strip()
    if not body:
        raise TaskValidationError("prompt: body is empty")

    raw_id = fm.get("id")
    if not isinstance(raw_id, str) or not raw_id:
        raise TaskValidationError("id: required")
    if not _ID_RE.fullmatch(raw_id):
        raise TaskValidationError(
            f"id: '{raw_id}' must be kebab-case (lowercase letters, digits, hyphens)"
        )

    schedule = fm.get("schedule")
    fire_at = fm.get("fire_at")
    if (schedule is None) == (fire_at is None):
        raise TaskValidationError(
            "schedule/fire_at: exactly one of schedule or fire_at is required"
        )
    if schedule is not None:
        if not isinstance(schedule, str) or not croniter.is_valid(schedule):
            raise TaskValidationError(f"schedule: invalid cron expression '{schedule}'")
    if fire_at is not None:
        try:
            datetime.fromisoformat(str(fire_at))
        except ValueError as e:
            raise TaskValidationError(
                f"fire_at: invalid ISO-8601 timestamp '{fire_at}'"
            ) from e
        fire_at = str(fire_at)

    enabled = fm.get("enabled", True)
    if not isinstance(enabled, bool):
        raise TaskValidationError("enabled: must be true or false")

    model = fm.get("model")
    if model is not None and not isinstance(model, str):
        raise TaskValidationError("model: must be a string")

    tools = fm.get("tools", list(ALL_TOOL_NAMES))
    if not isinstance(tools, list):
        raise TaskValidationError("tools: must be a list")
    unknown = [t for t in tools if t not in VALID_TOOLS]
    if unknown:
        raise TaskValidationError(f"tools: unknown tool(s) {unknown}")

    timeout_minutes = fm.get("timeout_minutes", 10)
    if (
        not isinstance(timeout_minutes, int)
        or isinstance(timeout_minutes, bool)
        or timeout_minutes <= 0
    ):
        raise TaskValidationError("timeout_minutes: must be a positive integer")

    catchup = fm.get("catchup", False)
    if not isinstance(catchup, bool):
        raise TaskValidationError("catchup: must be true or false")

    output_raw = fm.get("output") or {}
    if not isinstance(output_raw, dict):
        raise TaskValidationError("output: must be a mapping")
    for key in output_raw:
        if key not in _KNOWN_OUTPUT_KEYS:
            logger.warning("%s: unknown output key 'output.%s'", path, key)
    keep = output_raw.get("keep", 30)
    if not isinstance(keep, int) or isinstance(keep, bool) or keep <= 0:
        raise TaskValidationError("output.keep: must be a positive integer")
    out_dir = output_raw.get("dir")
    if out_dir is not None and not isinstance(out_dir, str):
        raise TaskValidationError("output.dir: must be a string")
    filename = output_raw.get("filename")
    if filename is not None and not isinstance(filename, str):
        raise TaskValidationError("output.filename: must be a string")
    raw_flag = output_raw.get("raw", False)
    if not isinstance(raw_flag, bool):
        raise TaskValidationError("output.raw: must be true or false")

    sources_raw = fm.get("sources") or []
    if not isinstance(sources_raw, list):
        raise TaskValidationError("sources: must be a list of URLs")
    sources: list[str] = []
    for url in sources_raw:
        if not isinstance(url, str) or not re.fullmatch(r"https?://\S+", url):
            raise TaskValidationError(f"sources: '{url}' is not an http(s) URL")
        if url not in sources:
            sources.append(url)

    checks_raw = fm.get("checks") or {}
    if not isinstance(checks_raw, dict):
        raise TaskValidationError("checks: must be a mapping")
    for key in checks_raw:
        if key not in _KNOWN_CHECK_KEYS:
            raise TaskValidationError(f"checks: unknown key '{key}'")
    require_sources = checks_raw.get("require_sources", True)
    if not isinstance(require_sources, bool):
        raise TaskValidationError("checks.require_sources: must be true or false")
    min_report_links = checks_raw.get("min_report_links")
    if min_report_links is not None and (
        not isinstance(min_report_links, int)
        or isinstance(min_report_links, bool)
        or min_report_links < 1
    ):
        raise TaskValidationError(
            "checks.min_report_links: must be a positive integer"
        )
    require_cited = checks_raw.get("require_cited_sources", False)
    if not isinstance(require_cited, bool):
        raise TaskValidationError("checks.require_cited_sources: must be true or false")

    name = fm.get("name", raw_id)
    if not isinstance(name, str):
        raise TaskValidationError("name: must be a string")

    return Task(
        id=raw_id,
        name=name,
        schedule=schedule,
        fire_at=fire_at,
        enabled=enabled,
        model=model,
        tools=list(tools),
        timeout_minutes=timeout_minutes,
        catchup=catchup,
        output=OutputSpec(keep=keep, dir=out_dir, filename=filename, raw=raw_flag),
        sources=sources,
        checks=ChecksSpec(
            require_sources=require_sources,
            min_report_links=min_report_links,
            require_cited_sources=require_cited,
        ),
        prompt=body,
        path=path,
    )


def load_tasks_dir(dir_path: Path) -> tuple[list[Task], list[tuple[Path, str]]]:
    """Parse every *.md file in dir_path. Returns (valid tasks, [(path, error)]).

    Duplicate ids: first file (alphabetical) wins; later duplicates are reported
    as invalid (spec FR-2).
    """
    valid: list[Task] = []
    invalid: list[tuple[Path, str]] = []
    seen: dict[str, Path] = {}
    if not dir_path.exists():
        return valid, invalid
    for path in sorted(dir_path.glob("*.md")):
        try:
            task = parse_task_file(path)
        except TaskValidationError as e:
            invalid.append((path, str(e)))
            continue
        if task.id in seen:
            invalid.append(
                (path, f"duplicate id '{task.id}' (already defined in {seen[task.id]})")
            )
            continue
        seen[task.id] = path
        valid.append(task)
    return valid, invalid


def render_task_file(task: Task) -> str:
    """Serialize a task to task-file text (frontmatter + prompt body)."""
    fm: dict = {"id": task.id}
    if task.name != task.id:
        fm["name"] = task.name
    if task.schedule is not None:
        fm["schedule"] = task.schedule
    if task.fire_at is not None:
        fm["fire_at"] = task.fire_at
    fm["enabled"] = task.enabled
    if task.model is not None:
        fm["model"] = task.model
    fm["tools"] = list(task.tools)
    fm["timeout_minutes"] = task.timeout_minutes
    fm["catchup"] = task.catchup
    if task.sources:
        fm["sources"] = list(task.sources)
    checks: dict = {}
    if not task.checks.require_sources:
        checks["require_sources"] = False
    if task.checks.min_report_links is not None:
        checks["min_report_links"] = task.checks.min_report_links
    if task.checks.require_cited_sources:
        checks["require_cited_sources"] = True
    if checks:
        fm["checks"] = checks

    output: dict = {"keep": task.output.keep}
    if task.output.dir:
        output["dir"] = task.output.dir
    if task.output.filename:
        output["filename"] = task.output.filename
    if task.output.raw:
        output["raw"] = task.output.raw
    fm["output"] = output

    return (
        "---\n"
        + yaml.safe_dump(fm, sort_keys=False)
        + "---\n\n"
        + task.prompt.strip()
        + "\n"
    )


def write_task_file(task: Task) -> None:
    """Fully (re)serialize a task to task.path. Used for newly created files."""
    task.path.write_text(render_task_file(task))


def set_enabled(path: Path, enabled: bool) -> None:
    """Surgically flip the `enabled:` key in a task file's frontmatter, preserving
    all other formatting/comments (spec US7)."""
    text = path.read_text()
    match = _FRONTMATTER_RE.match(text)
    if not match:
        raise TaskValidationError("frontmatter: missing leading '---' YAML block")
    fm_text, body = match.group(1), match.group(2)
    new_val = "true" if enabled else "false"
    if re.search(r"(?m)^enabled:\s*\S+", fm_text):
        new_fm_text = re.sub(
            r"(?m)^enabled:\s*\S+", f"enabled: {new_val}", fm_text, count=1
        )
    else:
        new_fm_text = re.sub(
            r"(?m)^(id:.*)$", rf"\1\nenabled: {new_val}", fm_text, count=1
        )
    path.write_text(f"---\n{new_fm_text}\n---\n{body}")


def set_model(path: Path, model: str | None) -> None:
    """Surgically set the `model:` key in a task file's frontmatter (or remove
    it, with None, so the task uses config.yaml's default), preserving all
    other formatting/comments like set_enabled(). Validates before writing."""
    text = path.read_text()
    match = _FRONTMATTER_RE.match(text)
    if not match:
        raise TaskValidationError("frontmatter: missing leading '---' YAML block")
    fm_text, body = match.group(1), match.group(2)
    if model is None:
        new_fm_text = re.sub(r"(?m)^model:.*\n?", "", fm_text, count=1).rstrip("\n")
    else:
        line = f"model: {json.dumps(model)}"
        if re.search(r"(?m)^model:", fm_text):
            new_fm_text = re.sub(r"(?m)^model:.*$", lambda _: line, fm_text, count=1)
        else:
            anchor = "name" if re.search(r"(?m)^name:", fm_text) else "id"
            new_fm_text = re.sub(
                rf"(?m)^({anchor}:.*)$", lambda m: f"{m.group(1)}\n{line}", fm_text, count=1
            )
    new_text = f"---\n{new_fm_text}\n---\n{body}"
    parse_task_text(new_text, path)  # raises TaskValidationError, file untouched
    path.write_text(new_text)
