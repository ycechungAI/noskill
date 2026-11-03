"""Deterministic verification of runs (agent/verify.py) -- regression tests for
the honest-status work after the daily-game-deals incident: runs that only
connected to the internet must not be recorded as 'success' with fabricated
deal tables."""

from pathlib import Path

from schedlm.agent import verify
from schedlm.tasks import ChecksSpec, Task, OutputSpec


def make_task(tmp_path: Path, sources: list[str] | None = None, checks: dict | None = None) -> Task:
    path = tmp_path / "t.md"
    path.write_text(
        "---\nid: t\nschedule: \"0 7 * * *\"\n"
        + (f"sources:\n" + "".join(f"- {u}\n" for u in sources) if sources else "")
        + (f"checks:\n" + "".join(f"  {k}: {v}\n" for k, v in (checks or {}).items()))
        + "---\n\nDo it.\n"
    )
    task = Task(
        id="t",
        name="t",
        schedule="0 7 * * *",
        fire_at=None,
        enabled=True,
        model=None,
        tools=["web_search", "fetch_page"],
        timeout_minutes=10,
        catchup=False,
        output=OutputSpec(),
        sources=list(sources or []),
        checks=ChecksSpec(**(checks or {})),
        prompt="Do it.",
        path=path,
    )
    return task


def _ok_fetch(url: str) -> verify.ToolCall:
    return verify.ToolCall("fetch_page", {"url": url}, f'<fetched_content url="{url}">some text</fetched_content>')


def _err_fetch(url: str) -> verify.ToolCall:
    return verify.ToolCall("fetch_page", {"url": url}, f'<fetched_content url="{url}">error: HTTP 404</fetched_content>')


def _search(url: str) -> verify.ToolCall:
    import json

    return verify.ToolCall(
        "web_search",
        {"query": "x"},
        json.dumps([{"title": "t", "url": url, "snippet": "s"}]),
    )


def test_no_web_calls_at_all_is_failed(tmp_path):
    task = make_task(tmp_path)
    ev = verify.evidence([])
    failures = verify.verify_run(task, ev, "# Report\n\nmade up everything")
    assert [f.check for f in failures] == ["no_web_data"]


def test_required_source_must_be_fetched(tmp_path):
    task = make_task(tmp_path, sources=["https://isthereanydeal.com/deals/#filter:x"], checks={"require_sources": True})
    ev = verify.evidence([_search("https://isthereanydeal.com/")])
    failures = verify.verify_run(task, ev, "# Report\n\nhttps://isthereanydeal.com/deals/")
    assert [f.check for f in failures] == ["required_source_not_fetched"]
    assert "never fetched" in str(failures[0])


def test_required_source_failed_reports_failed(tmp_path):
    task = make_task(tmp_path, sources=["https://isthereanydeal.com/deals"], checks={"require_sources": True})
    ev = verify.evidence([_err_fetch("https://isthereanydeal.com/deals")])
    failures = verify.verify_run(task, ev, "# Report\n\nno data")
    assert [f.check for f in failures] == ["no_web_data", "required_source_failed"]


def test_unbacked_report_fails_when_required(tmp_path):
    task = make_task(tmp_path, sources=["https://cheapshark.com/api/1.0/deals"], checks={"require_cited_sources": True})
    ev = verify.evidence([_ok_fetch("https://cheapshark.com/api/1.0/deals")])
    failures = verify.verify_run(task, ev, "# Deals\n\n| IsThereAnyDeal | $0 | N/A | ... |\n\nSource: https://random.com/")
    assert [f.check for f in failures] == ["unbacked_report"]


def test_min_report_links_fails_low_citation_count(tmp_path):
    task = make_task(tmp_path, sources=["https://cheapshark.com/api/1.0/deals"], checks={"min_report_links": 3})
    ev = verify.evidence([_ok_fetch("https://cheapshark.com/api/1.0/deals")])
    report = "# Deals\n\nhttps://cheapshark.com/api/1.0/deals\n"
    failures = verify.verify_run(task, ev, report)
    assert [f.check for f in failures] == ["min_report_links"]
    task.checks = ChecksSpec(min_report_links=1)
    assert verify.verify_run(task, ev, report) == []


def test_fragment_is_ignored_for_source_identity(tmp_path):
    task = make_task(tmp_path, sources=["https://isthereanydeal.com/deals/#filter:N4IgDgT"], checks={"require_sources": True})
    ev = verify.evidence([_ok_fetch("https://isthereanydeal.com/deals/")])
    assert verify.verify_run(task, ev, "# Report\n\nhttps://isthereanydeal.com/deals/") == []


def test_evidence_finds_only_ok_fetches(tmp_path):
    ev = verify.evidence([_ok_fetch("https://a.com/x"), _err_fetch("https://a.com/x"), _search("https://a.com/x")])
    assert "https://a.com/x" in ev.fetched_ok
    assert "https://a.com/x" in ev.fetched_keys
    assert "https://a.com/x" in ev.fetch_errors
    assert "https://a.com/x" in ev.searched_ok
    assert ev.any_research_ok
