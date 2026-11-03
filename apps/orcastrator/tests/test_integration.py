import asyncio
import os

import pytest

from orcastrator.orchestrator import StormOrchestrator

RUN = os.getenv("RUN_OLLAMA_TESTS") == "1"
MODEL = os.getenv("OLLAMA_TEST_MODEL", "llama3.2")


@pytest.mark.skipif(not RUN, reason="set RUN_OLLAMA_TESTS=1 to run against a live Ollama daemon")
def test_full_run_against_live_ollama():
    """End-to-end smoke test — requires a running Ollama daemon with MODEL pulled."""
    orch = StormOrchestrator(model=MODEL)
    report = asyncio.run(orch.run("The water cycle"))

    assert isinstance(report, str) and report.strip()
    assert orch.state["questions"], "planner produced no questions"
    assert len(orch.state["results"]) == len(orch.state["questions"])
