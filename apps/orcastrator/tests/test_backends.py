"""LMStudioBackend / JanBackend tests against a mocked OpenAI-compatible API (no server needed)."""

import asyncio
import json

import httpx

from orcastrator.backends import (
    JanBackend,
    LMStudioBackend,
    OllamaBackend,
    _strip_think,
    create_backend,
    is_cloud_model,
)


def _mounted(backend, handler):
    """Swap the backend's HTTP client for one served by a MockTransport."""
    backend._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return backend


def _completion_response(content):
    return httpx.Response(200, json={"choices": [{"message": {"content": content}}]})


def test_strip_think_removes_reasoning_blocks():
    assert _strip_think("<think>hmm, let me see</think>The answer is 4.") == "The answer is 4."
    assert _strip_think("<thinking>...</thinking>ok") == "ok"
    assert _strip_think("no tags here") == "no tags here"


def test_generate_sends_openai_payload_and_strips_think():
    seen = {}

    def handler(request):
        seen.update(json.loads(request.content))
        return _completion_response("<think>reasoning...</think>Paris is the capital.")

    backend = _mounted(LMStudioBackend(), handler)
    result = asyncio.run(
        backend.generate(model="some-model", system="You are a Researcher.", prompt="Capital of France?", num_predict=300)
    )
    assert result == "Paris is the capital."
    assert seen["model"] == "some-model"
    assert seen["max_tokens"] == 300
    assert seen["messages"][0] == {"role": "system", "content": "You are a Researcher."}
    assert seen["messages"][1] == {"role": "user", "content": "Capital of France?"}


def test_generate_retries_uncapped_when_reasoning_eats_the_budget():
    calls = []

    def handler(request):
        payload = json.loads(request.content)
        calls.append(payload)
        if "max_tokens" in payload:
            # Reasoning model burned the whole cap inside <think>: empty visible reply.
            return _completion_response("<think>endless pondering</think>")
        return _completion_response("Real answer.")

    backend = _mounted(LMStudioBackend(), handler)
    result = asyncio.run(
        backend.generate(model="m", system="s", prompt="p", num_predict=200)
    )
    assert result == "Real answer."
    assert len(calls) == 2
    assert "max_tokens" not in calls[1]


def test_list_models_excludes_embedding_models():
    def handler(request):
        assert request.url.path == "/api/v0/models"
        return httpx.Response(200, json={"data": [
            {"id": "qwen-7b", "type": "llm"},
            {"id": "phi-4", "type": "llm"},
            {"id": "nomic-embed-text", "type": "embeddings"},
        ]})

    backend = _mounted(LMStudioBackend(), handler)
    assert asyncio.run(backend.list_models()) == ["qwen-7b", "phi-4"]


def test_list_models_falls_back_to_v1_when_v0_unavailable():
    def handler(request):
        if request.url.path == "/api/v0/models":
            return httpx.Response(404)
        assert request.url.path == "/v1/models"
        return httpx.Response(200, json={"data": [{"id": "qwen-7b"}, {"id": "phi-4"}]})

    backend = _mounted(LMStudioBackend(), handler)
    assert asyncio.run(backend.list_models()) == ["qwen-7b", "phi-4"]


def test_generate_raises_readable_error_on_400():
    def handler(request):
        return httpx.Response(400, json={"error": {
            "message": "Invalid model identifier \"bad-model\". Please specify a valid downloaded model.",
        }})

    backend = _mounted(LMStudioBackend(), handler)
    try:
        asyncio.run(backend.generate(model="bad-model", system="s", prompt="p"))
        assert False, "expected RuntimeError"
    except RuntimeError as exc:
        assert "Invalid model identifier" in str(exc)
        assert "bad-model" in str(exc)
        assert "400" in str(exc)


def test_chat_stream_raises_readable_error_on_400():
    def handler(request):
        return httpx.Response(400, json={"error": {"message": "Invalid model identifier"}})

    backend = _mounted(LMStudioBackend(), handler)

    async def collect():
        return [piece async for piece in backend.chat_stream("bad-model", [{"role": "user", "content": "hi"}])]

    try:
        asyncio.run(collect())
        assert False, "expected RuntimeError"
    except RuntimeError as exc:
        assert "Invalid model identifier" in str(exc)


def test_chat_stream_parses_sse_deltas():
    sse = (
        'data: {"choices":[{"delta":{"role":"assistant"}}]}\n\n'
        'data: {"choices":[{"delta":{"content":"Hel"}}]}\n\n'
        'data: {"choices":[{"delta":{"content":"lo"}}]}\n\n'
        "data: [DONE]\n\n"
    )

    def handler(request):
        return httpx.Response(200, content=sse.encode(), headers={"content-type": "text/event-stream"})

    backend = _mounted(LMStudioBackend(), handler)

    async def collect():
        return [piece async for piece in backend.chat_stream("m", [{"role": "user", "content": "hi"}])]

    assert asyncio.run(collect()) == ["Hel", "lo"]


def test_is_cloud_model_matches_ollama_cloud_tags():
    assert is_cloud_model("gpt-oss:120b-cloud")
    assert is_cloud_model("glm-4.6:cloud")
    assert not is_cloud_model("llama3.2")
    assert not is_cloud_model("gemma4:12b")


def test_ensure_model_pulls_unregistered_cloud_model():
    calls = []

    def handler(request):
        calls.append(request.url.path)
        if request.url.path == "/api/tags":
            return httpx.Response(200, json={"models": [{"name": "llama3.2:latest"}]})
        if request.url.path == "/api/pull":
            assert json.loads(request.content)["model"] == "gpt-oss:120b-cloud"
            return httpx.Response(200, json={"status": "success"})
        return httpx.Response(404)

    backend = OllamaBackend()
    backend._http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    note = asyncio.run(backend.ensure_model("gpt-oss:120b-cloud"))
    assert "gpt-oss:120b-cloud" in note
    assert calls == ["/api/tags", "/api/pull"]

    # Local models never trigger a pull.
    calls.clear()
    assert asyncio.run(backend.ensure_model("llama3.2")) is None
    assert calls == []


def test_create_backend_rejects_unknown_names():
    try:
        create_backend("copilot")
        assert False, "expected ValueError"
    except ValueError as exc:
        assert "copilot" in str(exc)


# --- JanBackend (OpenAI-compatible, no auth, default host :1337) ---


def test_jan_default_host_and_env_override():
    assert JanBackend().host == "http://localhost:1337"
    import os

    os.environ["JAN_HOST"] = "http://jan.local:9000"
    try:
        assert JanBackend().host == "http://jan.local:9000"
    finally:
        del os.environ["JAN_HOST"]


def test_jan_generate_sends_openai_payload_and_strips_think():
    seen = {}

    def handler(request):
        seen.update(json.loads(request.content))
        return _completion_response("<think>reasoning...</think>Paris is the capital.")

    backend = _mounted(JanBackend(), handler)
    result = asyncio.run(
        backend.generate(model="some-model", system="You are a Researcher.", prompt="Capital of France?", num_predict=300)
    )
    assert result == "Paris is the capital."
    assert seen["model"] == "some-model"
    assert seen["max_tokens"] == 300
    assert seen["messages"][1] == {"role": "user", "content": "Capital of France?"}


def test_jan_list_models_parses_openai_format():
    def handler(request):
        assert request.url.path == "/v1/models"
        return httpx.Response(200, json={"data": [{"id": "qwen-7b"}, {"id": "phi-4"}]})

    backend = _mounted(JanBackend(), handler)
    assert asyncio.run(backend.list_models()) == ["qwen-7b", "phi-4"]


def test_jan_chat_stream_parses_sse_deltas():
    sse = (
        'data: {"choices":[{"delta":{"role":"assistant"}}]}\n\n'
        'data: {"choices":[{"delta":{"content":"Hel"}}]}\n\n'
        'data: {"choices":[{"delta":{"content":"lo"}}]}\n\n'
        "data: [DONE]\n\n"
    )

    def handler(request):
        return httpx.Response(200, content=sse.encode(), headers={"content-type": "text/event-stream"})

    backend = _mounted(JanBackend(), handler)

    async def collect():
        return [piece async for piece in backend.chat_stream("m", [{"role": "user", "content": "hi"}])]

    assert asyncio.run(collect()) == ["Hel", "lo"]


def test_jan_ensure_model_is_a_noop():
    backend = JanBackend()
    assert asyncio.run(backend.ensure_model("anything")) is None


def test_jan_ensure_server_returns_none_when_reachable():
    def handler(request):
        assert request.url.path == "/v1/models"
        return httpx.Response(200, json={"data": [{"id": "llama-7b"}]})

    backend = _mounted(JanBackend(), handler)
    assert asyncio.run(backend.ensure_server()) is None


def test_jan_ensure_server_raises_clear_error_when_down():
    def handler(request):
        return httpx.Response(503)

    backend = _mounted(JanBackend(host="http://localhost:1337"), handler)
    try:
        asyncio.run(backend.ensure_server())
        assert False, "expected RuntimeError"
    except RuntimeError as exc:
        assert "Jan.ai isn't reachable at http://localhost:1337" in str(exc)
        assert "Local API Server" in str(exc)
