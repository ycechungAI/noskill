"""Tool registry: schemas + implementations. See Agents.md §3."""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Callable

import httpx
from bs4 import BeautifulSoup
from ddgs import DDGS

logger = logging.getLogger(__name__)

# A descriptive UA: some APIs (e.g. CheapShark) reject "generic" UAs; the URL
# is a placeholder until PACKAGING.md decides the home in the noskill repo.
_USER_AGENT = "sched/1.0 (local task scheduler; https://github.com/ycechungAI/noskill)"
_MAX_REDIRECTS = 3
_FETCH_TIMEOUT_SECONDS = 10.0

TOOL_SCHEMAS = [
    {
        "type": "function",
        "function": {
            "name": "web_search",
            "description": (
                "Search the web. Returns JSON list of {title, url, snippet}. "
                "Pass keywords, not URLs and not '#'-fragment strings; URLs "
                "belong in fetch_page."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string", "description": "search query"},
                    "max_results": {
                        "type": "integer",
                        "description": "1-10, default 5",
                    },
                },
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "fetch_page",
            "description": "Fetch a web page and return its readable text, wrapped in <fetched_content> tags.",
            "parameters": {
                "type": "object",
                "properties": {"url": {"type": "string"}},
                "required": ["url"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "current_datetime",
            "description": "Returns the current local date/time in ISO format.",
            "parameters": {"type": "object", "properties": {}},
        },
    },
    {
        "type": "function",
        "function": {
            "name": "read_note",
            "description": "Read a named note saved by a previous run of any task. Returns its text or an error if it does not exist.",
            "parameters": {
                "type": "object",
                "properties": {
                    "name": {
                        "type": "string",
                        "description": "note filename, e.g. 'seen-urls.txt'",
                    }
                },
                "required": ["name"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "write_note",
            "description": "Save a named note for future runs (overwrites). Use for state like already-reported items.",
            "parameters": {
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "content": {"type": "string"},
                },
                "required": ["name", "content"],
            },
        },
    },
]

_TOOL_NAMES = {s["function"]["name"] for s in TOOL_SCHEMAS}

MAX_TOOL_RESULT_CHARS = 12_000


@dataclass
class ToolContext:
    """Per-run context threaded into tool execution (Agents.md §3)."""

    notes_dir: Path
    fetch_max_chars: int = 8000
    search_max_results: int = 5
    # (page_text, source_url) -> shorter text; used when a fetched page
    # exceeds fetch_max_chars (Agents.md §5 Compressor). None = hard-truncate.
    compress: Callable[[str, str], str] | None = field(default=None)


def schemas(names: list[str]) -> list[dict]:
    """Return the subset of TOOL_SCHEMAS whose function name is in names."""
    wanted = set(names)
    return [s for s in TOOL_SCHEMAS if s["function"]["name"] in wanted]


def execute(name: str, arguments: dict, ctx: ToolContext) -> str:
    """Dispatch to the tool implementation; never raises. Truncates any
    result to MAX_TOOL_RESULT_CHARS as a blanket safety net (Agents.md §2) --
    fetch_page has its own, smaller, compressor-eligible budget before this
    ever kicks in."""
    try:
        if name == "web_search":
            max_results = arguments.get("max_results") or ctx.search_max_results
            result = web_search(arguments.get("query", ""), int(max_results))
        elif name == "fetch_page":
            result = fetch_page(arguments.get("url", ""), ctx.fetch_max_chars, ctx.compress)
        elif name == "current_datetime":
            result = current_datetime()
        elif name == "read_note":
            result = read_note(arguments.get("name", ""), ctx.notes_dir)
        elif name == "write_note":
            result = write_note(
                arguments.get("name", ""), arguments.get("content", ""), ctx.notes_dir
            )
        else:
            return json.dumps({"error": f"unknown tool '{name}'"})
    except Exception as e:  # noqa: BLE001 - tools must never raise
        logger.exception("tool '%s' raised unexpectedly", name)
        return json.dumps({"error": str(e)})

    if len(result) > MAX_TOOL_RESULT_CHARS:
        result = result[:MAX_TOOL_RESULT_CHARS]
    return result


def web_search(query: str, max_results: int = 5) -> str:
    if not query:
        return json.dumps({"error": "empty query"})
    try:
        hits = DDGS().text(query, max_results=max_results)
    except Exception as e:
        return json.dumps({"error": f"search unavailable: {e}"})
    if not hits:
        return json.dumps({"error": "no results"})
    results = [
        {
            "title": h.get("title", ""),
            "url": h.get("href") or h.get("url", ""),
            "snippet": h.get("body", ""),
        }
        for h in hits
    ]
    return json.dumps(results)


def fetch_page(
    url: str,
    max_chars: int = 8000,
    compress: Callable[[str, str], str] | None = None,
) -> str:
    notes = []
    if not url:
        return '<fetched_content url="">error: empty url</fetched_content>'
    # The part of a URL after # is browser-only state (a page's filter/sort
    # settings, e.g. isthereanydeal's "#filter:...;sort:expiry") and never
    # reaches the server, so any fetch returns the page without it. Tell the
    # model this once, in the result, instead of letting it assume the fetch
    # honored its filter.
    if "#" in url:
        notes.append(
            "note: the '#' part of this URL is browser-only state (filters/sort) "
            "and never reaches the server; this is the unfiltered page -- apply "
            "any filtering yourself from the content, or use a source that "
            "returns the filtered data directly"
        )
    try:
        with httpx.Client(
            headers={"User-Agent": _USER_AGENT},
            timeout=_FETCH_TIMEOUT_SECONDS,
            follow_redirects=True,
            max_redirects=_MAX_REDIRECTS,
        ) as client:
            resp = client.get(url)
    except httpx.TimeoutException:
        return f'<fetched_content url="{url}">error: request timed out</fetched_content>'
    except httpx.HTTPError as e:
        return f'<fetched_content url="{url}">error: {e}</fetched_content>'

    if resp.status_code >= 400:
        return f'<fetched_content url="{url}">error: HTTP {resp.status_code}</fetched_content>'

    content_type = resp.headers.get("content-type", "")
    # JSON APIs are first-class fetch targets (deal lists, exchange rates,
    # release feeds): render them as text like any other page so the model can
    # pull records out of them.
    if "json" in content_type or url.split("?")[0].endswith(".json"):
        try:
            data = json.loads(resp.text)
            text = json.dumps(data, indent=2, ensure_ascii=False)
            notes.append("note: this URL returns JSON data, not a web page")
        except ValueError:
            text = resp.text
    elif "html" in content_type or "text" in content_type:
        soup = BeautifulSoup(resp.text, "html.parser")
        for tag in soup(["script", "style", "nav", "footer"]):
            tag.decompose()
        text = soup.get_text(" ", strip=True)
    else:
        return (
            f'<fetched_content url="{url}">error: unsupported content-type '
            f'{content_type or "unknown"}</fetched_content>'
        )

    if len(text) > max_chars:
        if compress is not None:
            try:
                text = compress(text, url)
            except Exception:
                text = text[:max_chars]
        else:
            text = text[:max_chars]

    if notes:
        text = "\n".join(notes) + "\n\n" + text
    return f'<fetched_content url="{url}">{text}</fetched_content>'


def current_datetime() -> str:
    return datetime.now().astimezone().isoformat()


def is_error_result(result: str) -> bool:
    """True when a tool's result is an error string (web_search/fetch_page
    failure JSON, or a failed <fetched_content>). Verification uses this to
    judge whether the run actually gathered evidence."""
    return (
        result.startswith('{"error"')
        or (result.startswith('<fetched_content') and ">error: " in result)
    )


def _safe_note_path(name: str, notes_dir: Path) -> Path:
    if not name or "/" in name or "\\" in name or ".." in name:
        raise ValueError(f"invalid note name '{name}'")
    return notes_dir / name


def read_note(name: str, notes_dir: Path) -> str:
    try:
        path = _safe_note_path(name, notes_dir)
    except ValueError as e:
        return json.dumps({"error": str(e)})
    if not path.exists():
        return json.dumps({"error": f"note '{name}' does not exist"})
    return path.read_text()


def write_note(name: str, content: str, notes_dir: Path) -> str:
    try:
        path = _safe_note_path(name, notes_dir)
    except ValueError as e:
        return json.dumps({"error": str(e)})
    notes_dir.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    return json.dumps({"ok": True})
