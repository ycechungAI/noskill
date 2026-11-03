import asyncio

from orcastrator.orchestrator import (
    StormOrchestrator,
    _concurrency_for,
    _linkify_citations,
    _renumber_citations,
    _strip_citations,
)
from orcastrator.retrieval import Source


def _script(responses):
    """Return an async callable that yields the given responses in order."""
    it = iter(responses)

    async def _fake(prompt):
        return next(it)

    return _fake


def _make_orchestrator():
    # Constructing agents only builds httpx clients; no network call happens here.
    return StormOrchestrator(model="dummy", use_web=False)


def test_approved_on_first_pass():
    orch = _make_orchestrator()
    orch.researcher.generate = _script(["draft answer"])
    orch.critic.generate = _script(["APPROVED: the refined answer"])

    result = asyncio.run(orch._research_and_validate(0, "Q?"))
    assert result == "the refined answer"


def test_revise_then_approve():
    orch = _make_orchestrator()
    orch.researcher.generate = _script(["weak draft", "stronger draft"])
    orch.critic.generate = _script(["REVISE: missing detail", "APPROVED: stronger draft"])

    result = asyncio.run(orch._research_and_validate(0, "Q?"))
    assert result == "stronger draft"


def test_approved_without_colon_falls_back_to_answer():
    # Malformed critic reply (missing "APPROVED: <refined answer>" format) shouldn't crash;
    # it should fall back to the researcher's last answer instead of raising IndexError.
    orch = _make_orchestrator()
    orch.researcher.generate = _script(["draft answer"])
    orch.critic.generate = _script(["APPROVED"])

    result = asyncio.run(orch._research_and_validate(0, "Q?"))
    assert result == "draft answer"


def test_never_approved_marks_unverified():
    orch = _make_orchestrator()
    orch.researcher.generate = _script(["draft1", "draft2", "draft3"])
    orch.critic.generate = _script(["REVISE: nope", "REVISE: still nope"])

    result = asyncio.run(orch._research_and_validate(0, "Q?"))
    assert result.startswith("[unverified]")
    assert "draft3" in result


def test_critic_disabled_skips_validation():
    orch = StormOrchestrator(model="dummy", use_critic=False, use_web=False)
    orch.researcher.generate = _script(["draft answer"])
    orch.critic.generate = _script([])  # any critic call would raise StopIteration

    result = asyncio.run(orch._research_and_validate(0, "Q?"))
    assert result == "draft answer"


def test_max_questions_truncates_planner_output():
    orch = StormOrchestrator(model="dummy", max_questions=2, use_web=False)
    orch.planner.generate = _script(["1. A?\n2. B?\n3. C?\n4. D?"])
    orch.researcher.generate = _script(["ans1", "ans2"])
    orch.critic.generate = _script(["APPROVED: ans1", "APPROVED: ans2"])
    orch.writer.generate = _script(["FINAL REPORT"])

    asyncio.run(orch.run("some topic"))
    assert orch.state["questions"] == ["A?", "B?"]


def test_extended_pipeline_debate_and_rewrite():
    orch = StormOrchestrator(model="dummy", extended=True, use_web=False)
    events = []
    orch._progress = events.append

    orch.planner.generate = _script(["Only question?"])
    orch.researcher.generate = _script(["first answer", "rebutted answer"])
    orch.challenger.generate = _script(["weakest claim: X"])
    orch.critic.generate = _script(["APPROVED: validated answer"])
    orch.writer.generate = _script(["DRAFT REPORT", "FINAL REPORT"])
    orch.reviewer.generate = _script(["1. tighten the intro"])

    result = asyncio.run(orch.run("some topic"))
    assert result == "FINAL REPORT"

    stages = [e["stage"] for e in events if e["kind"] == "stage"]
    assert stages == ["planning", "researching", "drafting", "reviewing", "rewriting"]
    # The planning event announces all five stages for the UI tracker.
    announced = next(e for e in events if e.get("stage") == "planning")["stages"]
    assert [s["key"] for s in announced] == stages

    statuses = [e["status"] for e in events if e["kind"] == "question"]
    assert statuses == ["researching", "challenging", "rebutting", "reviewing", "approved"]


def test_standard_pipeline_announces_three_stages():
    orch = StormOrchestrator(model="dummy", use_web=False)
    events = []
    orch._progress = events.append
    orch.planner.generate = _script(["Only question?"])
    orch.researcher.generate = _script(["answer"])
    orch.critic.generate = _script(["APPROVED: answer"])
    orch.writer.generate = _script(["REPORT"])

    asyncio.run(orch.run("some topic"))
    announced = next(e for e in events if e.get("stage") == "planning")["stages"]
    assert [s["key"] for s in announced] == ["planning", "researching", "writing"]


def test_concurrency_scales_with_model_memory_share():
    GIB = 2**30
    # Small model on a big machine: full fan-out.
    assert _concurrency_for(2 * GIB, 16 * GIB) == 4      # 12.5% of RAM
    # Mid-size model: halve it.
    assert _concurrency_for(5 * GIB, 16 * GIB) == 2      # ~31%
    # Model dominating RAM (gemma4:12b case: 7.5 of 16 GiB): one at a time.
    assert _concurrency_for(int(7.5 * GIB), 16 * GIB) == 1
    # Unknown memory: safe default.
    assert _concurrency_for(None, 16 * GIB) == 4
    assert _concurrency_for(2 * GIB, None) == 4


def test_max_concurrency_bounds_parallel_research(monkeypatch):
    orch = StormOrchestrator(model="dummy", use_critic=False, max_concurrency=1, use_web=False)

    active = 0
    peak = 0

    async def tracking_researcher(prompt):
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)  # give overlapping tasks a chance to interleave
        active -= 1
        return "answer"

    async def no_thinking_check():
        return None

    orch.planner.generate = _script(["A?\nB?\nC?"])
    orch.researcher.generate = tracking_researcher
    orch.writer.generate = _script(["REPORT"])
    monkeypatch.setattr(orch, "_configure_thinking", no_thinking_check)

    asyncio.run(orch.run("some topic"))
    assert peak == 1  # explicit max_concurrency=1 means strictly sequential


def test_linkify_citations_makes_markers_clickable():
    refs = [_source(1), _source(2)]
    text = "Fact [1]. Other [2]. Out of range [7]."
    assert _linkify_citations(text, refs) == (
        "Fact [[1]](https://en.wikipedia.org/wiki/Page_1). "
        "Other [[2]](https://en.wikipedia.org/wiki/Page_2). "
        "Out of range [7]."
    )


def test_strip_citations_removes_markers():
    assert _strip_citations("Fact [1]. Other [12].") == "Fact. Other."
    assert _strip_citations("Keep [unverified] tags.") == "Keep [unverified] tags."


def test_renumber_citations_shifts_only_valid_source_numbers():
    text = "Claim A [1]. Claim B [2]. Bogus [9]."
    # Section has 2 sources and follows 3 earlier references.
    assert _renumber_citations(text, offset=3, n_sources=2) == "Claim A [4]. Claim B [5]. Bogus [9]."


class _FakeRetriever:
    def __init__(self, per_query):
        self.per_query = per_query
        self.queries = []

    async def search(self, query, k=3):
        self.queries.append(query)
        return self.per_query.get(query, [])

    async def close(self):
        pass


def _source(n):
    return Source(title=f"Page {n}", url=f"https://en.wikipedia.org/wiki/Page_{n}",
                  content=f"content {n}", verified=True)


def test_grounded_run_cites_sources_and_appends_references():
    orch = StormOrchestrator(model="dummy", use_web=False)
    orch.use_web = True
    orch.retriever = _FakeRetriever({
        "First?": [_source(1), _source(2)],
        "Second?": [_source(3)],
    })

    prompts = []

    async def researcher(prompt):
        prompts.append(prompt)
        return "Fact [1]. Another [2]." if "Page 1" in prompt else "Solo fact [1]."

    writer_prompts = []

    async def writer(prompt):
        writer_prompts.append(prompt)
        return "REPORT BODY"

    orch.planner.generate = _script(["First?\nSecond?"])
    orch.researcher.generate = researcher
    orch.critic.generate = _script(["APPROVED", "APPROVED"])
    orch.writer.generate = writer

    result = asyncio.run(orch.run("some topic"))

    # Researcher was given the numbered sources and citation instructions.
    assert any("SOURCES:" in p and "[1] Page 1" in p for p in prompts)
    # References section is appended programmatically with verified links.
    assert "## References" in result
    assert "[Page 1](https://en.wikipedia.org/wiki/Page_1)" in result
    assert "[Page 3](https://en.wikipedia.org/wiki/Page_3)" in result
    # Second question's local [1] was renumbered past the first question's 2
    # sources in the research handed to the writer.
    assert "Solo fact [3]." in writer_prompts[0]
    assert "Keep the citation markers" in writer_prompts[0]
    assert orch.state["sources"][0][0].title == "Page 1"


def _grounded_orchestrator(**kwargs):
    orch = StormOrchestrator(model="dummy", use_web=False, **kwargs)
    orch.use_web = True
    orch.retriever = _FakeRetriever({"Only question?": [_source(1)]})
    orch.planner.generate = _script(["Only question?"])
    orch.researcher.generate = _script(["Fact [1]."])
    orch.critic.generate = _script(["APPROVED"])
    orch.writer.generate = _script(["Body fact [1]."])
    return orch


def test_inline_citations_are_linkified_in_final_report():
    result = asyncio.run(_grounded_orchestrator().run("some topic"))
    assert "Body fact [[1]](https://en.wikipedia.org/wiki/Page_1)." in result
    assert "## References" in result


def test_citations_checkbox_off_strips_markers_but_keeps_references():
    orch = _grounded_orchestrator(inline_citations=False)
    result = asyncio.run(orch.run("some topic"))
    assert "Body fact." in result
    assert "[[1]]" not in result
    assert "## References" in result  # traceability preserved


def test_grounded_run_with_no_sources_marks_section_unsourced():
    orch = StormOrchestrator(model="dummy", use_web=False)
    orch.use_web = True
    orch.retriever = _FakeRetriever({})

    orch.planner.generate = _script(["Only question?"])
    orch.researcher.generate = _script(["plain answer"])
    orch.critic.generate = _script(["APPROVED"])

    async def writer(prompt):
        assert "unsourced model knowledge" in prompt
        return "REPORT"

    orch.writer.generate = writer
    result = asyncio.run(orch.run("some topic"))
    assert "## References" not in result  # nothing to reference


def test_run_pipeline_end_to_end_mocked():
    orch = _make_orchestrator()
    orch.planner.generate = _script(["1. First?\n2. Second?"])
    orch.researcher.generate = _script(["ans1", "ans2"])
    orch.critic.generate = _script(["APPROVED: ans1", "APPROVED: ans2"])
    orch.writer.generate = _script(["FINAL REPORT"])

    result = asyncio.run(orch.run("some topic"))
    assert result == "FINAL REPORT"
    assert orch.state["questions"] == ["First?", "Second?"]
    assert orch.state["results"] == ["ans1", "ans2"]
