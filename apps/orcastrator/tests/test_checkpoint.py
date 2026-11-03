"""Checkpoint unit tests and end-to-end resume behavior with scripted agents."""

import asyncio
import json

import pytest

from orcastrator.checkpoint import Checkpoint, fingerprint
from orcastrator.orchestrator import StormOrchestrator


def _script(responses):
    it = iter(responses)

    async def _fake(prompt):
        return next(it)

    return _fake


# --- Checkpoint class ---


def test_fingerprint_is_stable_and_config_sensitive():
    a = fingerprint(topic="t", model="m", extended=False)
    b = fingerprint(model="m", topic="t", extended=False)  # order-independent
    c = fingerprint(topic="t", model="m", extended=True)
    assert a == b
    assert a != c


def test_save_load_roundtrip_and_fingerprint_gate(tmp_path):
    cp = Checkpoint(str(tmp_path / "state.json"))
    cp.save({"fingerprint": "abc", "questions": ["Q?"], "results": {}})
    assert cp.load("abc")["questions"] == ["Q?"]
    assert cp.load("other") is None  # different run config: ignored


def test_corrupted_checkpoint_is_ignored(tmp_path):
    path = tmp_path / "state.json"
    path.write_text("{not json")
    assert Checkpoint(str(path)).load("abc") is None


def test_clear_removes_file_and_tolerates_absence(tmp_path):
    cp = Checkpoint(str(tmp_path / "state.json"))
    cp.save({"fingerprint": "abc"})
    cp.clear()
    assert cp.load("abc") is None
    cp.clear()  # already gone: no error


# --- End-to-end resume through the orchestrator ---


def _orchestrator(tmp_path, **kwargs):
    defaults = dict(
        model="dummy",
        use_web=False,
        use_critic=False,
        max_concurrency=1,  # deterministic question order
        checkpoint_path=str(tmp_path / "state.json"),
    )
    defaults.update(kwargs)
    return StormOrchestrator(**defaults)


def test_crashed_run_resumes_without_redoing_finished_work(tmp_path):
    state_file = tmp_path / "state.json"

    # First run: question 1 succeeds, question 2 crashes the run.
    orch = _orchestrator(tmp_path)
    orch.planner.generate = _script(["First?\nSecond?"])

    async def failing_researcher(prompt):
        if "First?" in prompt:
            return "answer one"
        raise RuntimeError("daemon died")

    orch.researcher.generate = failing_researcher
    with pytest.raises(RuntimeError):
        asyncio.run(orch.run("some topic"))

    saved = json.loads(state_file.read_text())
    assert saved["questions"] == ["First?", "Second?"]
    assert saved["results"]["0"]["answer"] == "answer one"
    assert "1" not in saved["results"]

    # Second run, same config: planner must NOT run again (empty script would
    # raise StopIteration), and only the unfinished question is researched.
    orch2 = _orchestrator(tmp_path)
    orch2.planner.generate = _script([])
    orch2.researcher.generate = _script(["answer two"])
    orch2.writer.generate = _script(["FINAL REPORT"])

    result = asyncio.run(orch2.run("some topic"))
    assert result == "FINAL REPORT"
    assert orch2.state["results"] == ["answer one", "answer two"]
    assert not state_file.exists()  # cleared after success


def test_checkpoint_for_different_topic_is_ignored(tmp_path):
    orch = _orchestrator(tmp_path)
    orch.planner.generate = _script(["Only?"])

    async def boom(prompt):
        raise RuntimeError("crash")

    orch.researcher.generate = boom
    with pytest.raises(RuntimeError):
        asyncio.run(orch.run("topic A"))

    # New topic: the old checkpoint must not be resumed — planner runs again.
    orch2 = _orchestrator(tmp_path)
    orch2.planner.generate = _script(["Other?"])
    orch2.researcher.generate = _script(["fresh answer"])
    orch2.writer.generate = _script(["REPORT B"])
    assert asyncio.run(orch2.run("topic B")) == "REPORT B"


def test_no_resume_ignores_existing_checkpoint(tmp_path):
    orch = _orchestrator(tmp_path)
    orch.planner.generate = _script(["Only?"])

    async def boom(prompt):
        raise RuntimeError("crash")

    orch.researcher.generate = boom
    with pytest.raises(RuntimeError):
        asyncio.run(orch.run("some topic"))

    orch2 = _orchestrator(tmp_path, resume=False)
    orch2.planner.generate = _script(["Only?"])  # planner runs again
    orch2.researcher.generate = _script(["fresh answer"])
    orch2.writer.generate = _script(["FRESH REPORT"])
    result = asyncio.run(orch2.run("some topic"))
    assert result == "FRESH REPORT"
    assert orch2.state["results"] == ["fresh answer"]


def test_extended_run_checkpoints_and_restores_the_draft(tmp_path):
    state_file = tmp_path / "state.json"

    # First run: research + draft succeed, the review step crashes.
    orch = _orchestrator(tmp_path, extended=True, use_critic=True)
    orch.planner.generate = _script(["Only?"])
    orch.researcher.generate = _script(["answer", "rebuttal answer"])
    orch.challenger.generate = _script(["weak points"])
    orch.critic.generate = _script(["APPROVED: validated answer"])
    orch.writer.generate = _script(["THE DRAFT"])

    async def failing_reviewer(prompt):
        raise RuntimeError("crash during review")

    orch.reviewer.generate = failing_reviewer
    with pytest.raises(RuntimeError):
        asyncio.run(orch.run("some topic"))
    assert json.loads(state_file.read_text())["draft"] == "THE DRAFT"

    # Resume: no planner/researcher/draft-writer calls; review + rewrite only.
    orch2 = _orchestrator(tmp_path, extended=True, use_critic=True)
    orch2.planner.generate = _script([])
    orch2.researcher.generate = _script([])
    orch2.reviewer.generate = _script(["feedback"])
    orch2.writer.generate = _script(["FINAL EXTENDED REPORT"])

    result = asyncio.run(orch2.run("some topic"))
    assert result == "FINAL EXTENDED REPORT"
    assert not state_file.exists()
