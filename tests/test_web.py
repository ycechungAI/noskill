"""Web UI tests: exercised with a stubbed orchestrator so no Ollama is needed."""

import json

from fastapi.testclient import TestClient

import orcastrator.web as web
from orcastrator.retrieval import Source


class FakeOrchestrator:
    """Mimics StormOrchestrator: emits two progress events, then a report."""

    def __init__(self, *, on_progress=None, **kwargs):
        self.kwargs = kwargs
        self.progress = on_progress or (lambda m: None)

    async def run(self, topic):
        self.progress({"kind": "stage", "stage": "planning", "message": "planning"})
        self.progress({"kind": "stage", "stage": "researching", "message": "researching",
                       "questions": ["Q1", "Q2"], "use_critic": True})
        self.progress({"kind": "question", "index": 0, "status": "approved", "message": "approved Q1"})
        return f"# Report on {topic}\n\nBody."

    async def aclose(self):
        pass


class FailingOrchestrator(FakeOrchestrator):
    async def run(self, topic):
        raise RuntimeError("Planner returned no research questions")


def _events(body: str) -> list[tuple[str, str]]:
    """Parse an SSE body into (event, decoded-data) pairs."""
    events = []
    for block in body.strip().split("\n\n"):
        kind = data = None
        for line in block.splitlines():
            if line.startswith("event: "):
                kind = line[len("event: "):]
            elif line.startswith("data: "):
                data = json.loads(line[len("data: "):])
        if kind:
            events.append((kind, data))
    return events


def test_index_serves_ui_page():
    client = TestClient(web.app)
    response = client.get("/")
    assert response.status_code == 200
    assert "Orcastrator" in response.text
    assert "<form" in response.text


def test_research_streams_progress_then_report(monkeypatch):
    monkeypatch.setattr(web, "StormOrchestrator", FakeOrchestrator)
    client = TestClient(web.app)
    response = client.get("/api/research", params={"topic": "tides"})
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/event-stream")
    events = _events(response.text)
    progress = [data for kind, data in events if kind == "progress"]
    stages = [p["stage"] for p in progress if p["kind"] == "stage"]
    assert stages == ["planning", "researching"]
    # The researching stage carries the question list so the UI can lay out status rows.
    researching = next(p for p in progress if p.get("stage") == "researching")
    assert researching["questions"] == ["Q1", "Q2"]
    # Per-question status events drive the live status rows.
    assert any(p["kind"] == "question" and p["status"] == "approved" for p in progress)
    assert events[-1] == ("report", "# Report on tides\n\nBody.")


def test_research_applies_speed_preset(monkeypatch):
    created = {}

    class CapturingOrchestrator(FakeOrchestrator):
        def __init__(self, **kwargs):
            super().__init__(**kwargs)
            created.update(self.kwargs)

    monkeypatch.setattr(web, "StormOrchestrator", CapturingOrchestrator)
    client = TestClient(web.app)
    client.get("/api/research", params={"topic": "tides", "preset": "fast"})
    assert created["max_questions"] == 4
    assert created["use_critic"] is False
    assert created["answer_tokens"] == 300

    client.get("/api/research", params={"topic": "tides", "preset": "extended"})
    assert created["extended"] is True
    assert created["use_critic"] is True


def test_research_reports_pipeline_failure_as_error_event(monkeypatch):
    monkeypatch.setattr(web, "StormOrchestrator", FailingOrchestrator)
    client = TestClient(web.app)
    response = client.get("/api/research", params={"topic": "tides"})
    events = _events(response.text)
    assert events == [("error", "Planner returned no research questions")]


class FakeChatBackend:
    """Mimics a Backend for the chat endpoint: streams two tokens."""

    last_request = {}

    def __init__(self, name, host=None, timeout=None):
        FakeChatBackend.last_request.clear()
        FakeChatBackend.last_request.update({"backend": name, "host": host})

    async def chat_stream(self, model, messages):
        FakeChatBackend.last_request.update({"model": model, "messages": messages})
        for part in ("Hello", " there"):
            yield part

    async def close(self):
        pass


class FlakyCloseChatBackend(FakeChatBackend):
    """A backend whose close() blows up after a successful reply.

    Reproduces the "Failed to fetch" regression: a cleanup failure in the
    handler's `finally` block must never be allowed to kill an already-good
    streaming response.
    """

    async def close(self):
        raise RuntimeError("connection pool teardown exploded")


def test_close_failure_after_reply_does_not_break_the_response(monkeypatch):
    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FlakyCloseChatBackend(name, host, timeout),
    )
    client = TestClient(web.app)
    response = client.post(
        "/api/chat", json={"messages": [{"role": "user", "content": "hi"}], "model": "llama3.2"}
    )
    # The reply must arrive intact even though cleanup afterward failed.
    assert response.status_code == 200
    assert response.text == "Hello there"


def test_chat_streams_reply_text(monkeypatch):
    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FakeChatBackend(name, host, timeout),
    )
    client = TestClient(web.app)
    history = [{"role": "user", "content": "hi"}]
    response = client.post(
        "/api/chat", json={"messages": history, "model": "llama3.2", "backend": "lmstudio"}
    )
    assert response.status_code == 200
    assert response.text == "Hello there"
    assert FakeChatBackend.last_request["model"] == "llama3.2"
    assert FakeChatBackend.last_request["messages"] == history
    assert FakeChatBackend.last_request["backend"] == "lmstudio"


class FakeChatRetriever:
    """Mimics MultiRetriever for grounded-chat tests: returns a fixed source list."""

    def __init__(self, sources, enabled=None):
        self.sources = sources
        self.enabled = enabled

    async def search(self, query, k=3):
        return self.sources

    async def close(self):
        pass


class FakeCitingChatBackend(FakeChatBackend):
    """Like FakeChatBackend, but the reply cites source [1] — for linkify tests."""

    async def chat_stream(self, model, messages):
        FakeCitingChatBackend.last_request.update({"model": model, "messages": messages})
        for part in ("Reefs form over centuries", " [1]."):
            yield part


def test_chat_with_web_search_grounds_reply_and_appends_sources(monkeypatch):
    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FakeCitingChatBackend(name, host, timeout),
    )
    source = Source(title="Example Page", url="https://example.com/a", content="fact", verified=True)
    monkeypatch.setattr(web, "MultiRetriever", lambda enabled=None: FakeChatRetriever([source], enabled))

    client = TestClient(web.app)
    history = [{"role": "user", "content": "tell me about coral reefs"}]
    response = client.post(
        "/api/chat",
        json={"messages": history, "model": "llama3.2", "use_web": True},
    )
    assert response.status_code == 200
    # The grounding system message was inserted directly before the final user turn.
    sent = FakeCitingChatBackend.last_request["messages"]
    assert sent[-1] == history[0]
    assert sent[-2]["role"] == "system"
    assert "SOURCES" in sent[-2]["content"]
    assert "https://example.com/a" in sent[-2]["content"]
    # Bare [1] from the model's reply becomes a clickable citation link, plus a
    # verified Sources footer built from the actual fetched Source, not the model.
    assert "[[1]](https://example.com/a)" in response.text
    assert "[Example Page](https://example.com/a)" in response.text


def test_grounded_chat_survives_retriever_close_failure(monkeypatch):
    class FlakyCloseRetriever(FakeChatRetriever):
        async def close(self):
            raise RuntimeError("retriever teardown exploded")

    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FakeCitingChatBackend(name, host, timeout),
    )
    source = Source(title="Example Page", url="https://example.com/a", content="fact", verified=True)
    monkeypatch.setattr(web, "MultiRetriever", lambda enabled=None: FlakyCloseRetriever([source], enabled))

    client = TestClient(web.app)
    response = client.post(
        "/api/chat",
        json={"messages": [{"role": "user", "content": "tell me about coral reefs"}], "model": "llama3.2", "use_web": True},
    )
    assert response.status_code == 200
    assert "[[1]](https://example.com/a)" in response.text


def test_chat_with_web_search_falls_back_to_plain_reply_when_no_sources(monkeypatch):
    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FakeChatBackend(name, host, timeout),
    )
    monkeypatch.setattr(web, "MultiRetriever", lambda enabled=None: FakeChatRetriever([], enabled))

    client = TestClient(web.app)
    response = client.post(
        "/api/chat",
        json={"messages": [{"role": "user", "content": "hi"}], "model": "llama3.2", "use_web": True},
    )
    assert response.status_code == 200
    assert response.text == "Hello there"  # ungrounded FakeChatBackend reply, unmodified


def test_chat_without_web_search_never_touches_retriever(monkeypatch):
    def boom(enabled=None):
        raise AssertionError("MultiRetriever should not be constructed when use_web is False")

    monkeypatch.setattr(
        web, "create_backend",
        lambda name, host=None, timeout=None: FakeChatBackend(name, host, timeout),
    )
    monkeypatch.setattr(web, "MultiRetriever", boom)

    client = TestClient(web.app)
    response = client.post(
        "/api/chat", json={"messages": [{"role": "user", "content": "hi"}], "model": "llama3.2"}
    )
    assert response.status_code == 200
    assert response.text == "Hello there"


def test_models_endpoint_degrades_gracefully_when_ollama_is_down():
    client = TestClient(web.app)
    response = client.get("/api/models", params={"host": "http://localhost:1"})
    assert response.status_code == 200
    body = response.json()
    assert body["models"] == []
    assert "Could not reach ollama" in body["error"]
