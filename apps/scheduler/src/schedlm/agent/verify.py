"""Deterministic run verification (Agents.md §6). After the model writes its
final report, these code-level checks decide whether the run honestly
*completed the task* or merely didn't crash -- the gap that let a deal-watch
task "pass" with a table of invented prices and one failed search.

Checks are structural: did the run make successful web calls, did it fetch the
task's required sources, does the report cite pages actually fetched or
searched. Judging *content* (is this table's data plausible?) stays with the
model's own system prompt; do not add a model-judge here without measuring a
3B model's judge accuracy first.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from urllib.parse import urlsplit

from schedlm.tasks import Task

from schedlm.agent.tools import is_error_result

_URL_RE = re.compile(r"https?://[^\s<>\"')\]]+", re.IGNORECASE)


@dataclass
class ToolCall:
    """One recorded tool call from the run (executor.py keeps the log)."""

    name: str
    arguments: dict
    result: str


@dataclass
class RunEvidence:
    """What the run actually gathered -- derived once from the tool log."""

    calls: list[ToolCall] = field(default_factory=list)
    fetched_ok: set[str] = field(default_factory=set)  # URLs fetched without error
    fetched_keys: set[str] = field(default_factory=set)  # scheme+host+path of fetched_ok
    fetch_errors: set[str] = field(default_factory=set)
    searched_ok: set[str] = field(default_factory=set)  # URLs returned by search results

    @property
    def any_research_ok(self) -> bool:
        return bool(self.fetched_ok or self.searched_ok)


def _key(url: str) -> str:
    """Identity of a URL ignoring fragment (browser-only), query, case, and
    trailing slash -- enough to say a page was fetched without demanding a
    byte-identical copy."""
    parts = urlsplit(url)
    path = parts.path or "/"
    return f"{parts.scheme.lower()}://{parts.netloc.lower()}{path.rstrip('/')}"


def evidence(calls: list[ToolCall]) -> RunEvidence:
    ev = RunEvidence(calls=calls)
    for call in calls:
        if call.name == "fetch_page":
            url = str(call.arguments.get("url", ""))
            if not url:
                continue
            if is_error_result(call.result):
                ev.fetch_errors.add(url)
            else:
                ev.fetched_ok.add(url)
                ev.fetched_keys.add(_key(url))
        elif call.name == "web_search" and not is_error_result(call.result):
            try:
                hits = json.loads(call.result)
                if isinstance(hits, list):
                    for hit in hits:
                        url = hit.get("url") if isinstance(hit, dict) else None
                        if url:
                            ev.searched_ok.add(url)
            except ValueError:
                pass
    return ev


def _report_urls(report: str) -> set[str]:
    return {match.group(0).rstrip(".,").rstrip("/") for match in _URL_RE.finditer(report)}


@dataclass
class Failure:
    check: str
    detail: str

    def __str__(self) -> str:
        return f"{self.check}: {self.detail}"


def verify_run(task: Task, ev: RunEvidence, report: str) -> list[Failure]:
    """Check the final report against what the run actually did. Every failure
    is stated so the executor can be told exactly what to fix."""
    failures: list[Failure] = []

    research_tools = {"web_search", "fetch_page"} & set(task.tools)
    if research_tools and not ev.any_research_ok:
        failures.append(
            Failure(
                "no_web_data",
                "no successful web_search or fetch_page call -- nothing in the "
                "report can be verified against a source, so any facts are "
                "invented",
            )
        )

    # Required sources: each must have been fetched successfully (same
    # page, ignoring the browser-only fragment).
    if task.sources and task.checks.require_sources and "fetch_page" in task.tools:
        for url in task.sources:
            if _key(url) not in ev.fetched_keys:
                if _key(url) in {_key(u) for u in ev.fetch_errors}:
                    failures.append(
                        Failure(
                            "required_source_failed",
                            f"required source could not be fetched: {url}",
                        )
                    )
                else:
                    failures.append(
                        Failure(
                            "required_source_not_fetched",
                            f"required source was never fetched with fetch_page: {url} "
                            "(fetch it exactly as written; searching for it doesn't count)",
                        )
                    )

    # The report must cite pages the run actually obtained -- a table built
    # only from homepages/assumed URLs is the classic small-model fabrication.
    # Opt-in per task (checks.require_cited_sources), since citation matters
    # only for research-ish tasks that ask for sources.
    if research_tools and ev.any_research_ok and task.checks.require_cited_sources:
        searched_keys = {_key(s) for s in ev.searched_ok}
        cited = sorted(
            {
                url
                for url in _report_urls(report)
                if _key(url) in ev.fetched_keys or _key(url) in searched_keys
            }
        )
        if not cited:
            failures.append(
                Failure(
                    "unbacked_report",
                    "the report contains no URL that this run actually fetched "
                    "or found through search -- every figure in it is unbacked",
                )
            )

    # Explicit per-task minimum (checks.min_report_links).
    if task.checks.min_report_links is not None and research_tools:
        distinct = len({url.rstrip("/") for url in _report_urls(report)})
        if distinct < task.checks.min_report_links:
            failures.append(
                Failure(
                    "min_report_links",
                    f"the report cites {distinct} URL(s); the task requires at "
                    f"least {task.checks.min_report_links}",
                )
            )

    return failures


def verdict(ev: RunEvidence, task: Task, tools: list[str]) -> str:
    """Honest run status once verification is done (stored in runs.status):
    'failed' when the run produced no verifiable evidence at all, 'success'
    when every check the task requires is satisfiable, 'partial' in between."""
    research_tools = {"web_search", "fetch_page"} & set(tools)
    if research_tools and not ev.any_research_ok:
        return "failed"
    # 'partial' is decided by the caller from verify_run()'s failures.
    return "success"
