import asyncio

import httpx
import pytest

from orcastrator.agents import Agent


class _StubBackend:
    """Stand-in backend; tests replace .generate directly."""

    async def generate(self, **kwargs):
        raise NotImplementedError


def _make_agent(retries=1):
    return Agent("Tester", "test role", backend=_StubBackend(), model="dummy", retries=retries)


def _raise_then_succeed(exc, n_failures, result="ok"):
    calls = {"n": 0}

    async def _fake(**kwargs):
        calls["n"] += 1
        if calls["n"] <= n_failures:
            raise exc
        return result

    return _fake, calls


def test_retries_on_connect_timeout():
    agent = _make_agent(retries=2)
    fake, calls = _raise_then_succeed(httpx.ConnectTimeout("boom"), n_failures=2)
    agent.backend.generate = fake

    result = asyncio.run(agent.generate("prompt"))
    assert result == "ok"
    assert calls["n"] == 3  # 2 failures + 1 success


def test_retries_on_read_error():
    # httpx.ReadError is a TransportError but NOT a TimeoutException/ConnectError; the
    # retry tuple must cover it via httpx.TransportError, not just ConnectionError/TimeoutException.
    agent = _make_agent(retries=1)
    fake, calls = _raise_then_succeed(httpx.ReadError("dropped"), n_failures=1)
    agent.backend.generate = fake

    result = asyncio.run(agent.generate("prompt"))
    assert result == "ok"
    assert calls["n"] == 2


def test_gives_up_after_exhausting_retries():
    agent = _make_agent(retries=1)

    async def _always_fail(**kwargs):
        raise httpx.ConnectTimeout("still down")

    agent.backend.generate = _always_fail

    with pytest.raises(httpx.ConnectTimeout):
        asyncio.run(agent.generate("prompt"))


def test_non_retryable_error_propagates_immediately():
    agent = _make_agent(retries=3)
    calls = {"n": 0}

    async def _fake(**kwargs):
        calls["n"] += 1
        raise ValueError("bad model name")

    agent.backend.generate = _fake

    with pytest.raises(ValueError):
        asyncio.run(agent.generate("prompt"))
    assert calls["n"] == 1  # no retries for a non-retryable error
