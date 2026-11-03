import json
from dataclasses import dataclass, field
from pathlib import Path

import httpx
import ollama

import schedlm.agent.executor as executor_module
from schedlm.agent import tools as tools_module
from schedlm.agent.executor import run_task
from schedlm.config import AgentConfig, Config, PathsConfig
from schedlm.tasks import ChecksSpec, OutputSpec, Task


@dataclass
class FakeFunction:
    name: str
    arguments: dict


@dataclass
class FakeToolCall:
    function: FakeFunction


@dataclass
class FakeMessage:
    content: str | None = None
    tool_calls: list | None = None
    thinking: str | None = None


@dataclass
class FakeResponse:
    message: FakeMessage


class FakeOllamaClient:
    def __init__(self, responses):
        self._responses = list(responses)
        self.calls: list = []

    def chat(self, model, messages, tools=None, think=None):
        self.calls.append(
            {"model": model, "messages": list(messages), "tools": tools, "think": think}
        )
        return self._responses.pop(0)


def make_task(tmp_path: Path, tools=None) -> Task:
    path = tmp_path / "t.md"
    path.write_text('---\nid: t\nschedule: "0 7 * * *"\n---\n\nDo it.\n')
    return Task(
        id="t",
        name="t",
        schedule="0 7 * * *",
        fire_at=None,
        enabled=True,
        model="fake-model",
        tools=tools or ["web_search", "fetch_page"],
        timeout_minutes=10,
        catchup=False,
        output=OutputSpec(),
        prompt="Do it.",
        path=path,
    )


def make_cfg(tmp_path: Path, **overrides) -> Config:
    paths = PathsConfig(
        tasks=str(tmp_path / "tasks"),
        runs=str(tmp_path / "runs"),
        data=str(tmp_path / "data"),
    )
    return Config(paths=paths, **overrides)


def test_run_task_scripted_conversation(tmp_path, monkeypatch):
    monkeypatch.setattr(
        executor_module, "execute", lambda name, args, ctx: json.dumps({"tool": name})
    )
    task = make_task(tmp_path)
    cfg = make_cfg(tmp_path)

    search_call = FakeToolCall(FakeFunction("web_search", {"query": "x"}))
    fetch_call = FakeToolCall(FakeFunction("fetch_page", {"url": "https://example.com"}))
    responses = [
        FakeResponse(FakeMessage(tool_calls=[search_call])),
        FakeResponse(FakeMessage(tool_calls=[fetch_call])),
        FakeResponse(FakeMessage(content="# Report\n\ndone.")),
    ]
    client = FakeOllamaClient(responses)

    result = run_task(task, cfg, client=client)

    assert result.report == "# Report\n\ndone."
    assert result.iterations == 3
    assert result.tool_calls == 2
    assert result.model == "fake-model"
    # first two calls carry tools; nothing here forces a tool-less finalize call
    assert client.calls[0]["tools"]
    assert client.calls[1]["tools"]


def test_run_task_iteration_cap_forces_finalization_without_tools(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path)
    cfg = make_cfg(tmp_path, agent=AgentConfig(max_iterations=3))

    call = FakeToolCall(FakeFunction("web_search", {"query": "x"}))
    responses = [FakeResponse(FakeMessage(tool_calls=[call])) for _ in range(3)]
    responses.append(FakeResponse(FakeMessage(content="# Forced final report")))
    client = FakeOllamaClient(responses)

    result = run_task(task, cfg, client=client)

    assert result.report == "# Forced final report"
    assert result.iterations == 3
    assert result.tool_calls == 3
    assert len(client.calls) == 4
    assert client.calls[-1]["tools"] is None
    assert client.calls[-1]["messages"][-1]["content"] == executor_module.FINALIZE_PROMPT


def test_run_task_tool_result_truncated_within_loop(tmp_path, monkeypatch):
    huge_snippet = "y" * 20000

    class FakeDDGS:
        def text(self, query, max_results=5):
            return [{"title": "t", "href": "https://x", "body": huge_snippet}]

    monkeypatch.setattr(tools_module, "DDGS", lambda: FakeDDGS())

    task = make_task(tmp_path)
    cfg = make_cfg(tmp_path)
    call = FakeToolCall(FakeFunction("web_search", {"query": "x"}))
    responses = [
        FakeResponse(FakeMessage(tool_calls=[call])),
        FakeResponse(FakeMessage(content="done")),
    ]
    client = FakeOllamaClient(responses)

    run_task(task, cfg, client=client)

    tool_message = client.calls[-1]["messages"][-1]
    assert tool_message["role"] == "tool"
    assert len(tool_message["content"]) <= tools_module.MAX_TOOL_RESULT_CHARS


def test_run_task_no_tool_calls_returns_immediately(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "unused")
    task = make_task(tmp_path, tools=["current_datetime"])  # no research tools
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient([FakeResponse(FakeMessage(content="# Quick answer"))])

    result = run_task(task, cfg, client=client)

    assert result.report == "# Quick answer"
    assert result.iterations == 1
    assert result.tool_calls == 0


class ThinkUnsupportedClient(FakeOllamaClient):
    """Rejects think=True like a real model that doesn't support it."""

    def chat(self, model, messages, tools=None, think=None):
        if think:
            raise ollama.ResponseError(f'"{model}" does not support thinking', 400)
        return super().chat(model, messages, tools=tools, think=think)


def test_run_task_falls_back_when_thinking_unsupported(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    cfg = make_cfg(tmp_path)
    client = ThinkUnsupportedClient([FakeResponse(FakeMessage(content="# Done"))])

    events: list = []
    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.report == "# Done"
    assert any("does not support extended thinking" in e for e in events)
    assert len(client.calls) == 1  # the think=True attempt never got recorded
    assert client.calls[0]["think"] is None


def test_run_task_can_disable_thinking_via_config(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    cfg = make_cfg(tmp_path, agent=AgentConfig(think=False))
    client = FakeOllamaClient([FakeResponse(FakeMessage(content="# Done"))])

    run_task(task, cfg, client=client)

    assert client.calls[0]["think"] is None


def test_run_task_emits_events_for_tool_calls_and_thinking(tmp_path, monkeypatch):
    monkeypatch.setattr(
        executor_module, "execute", lambda name, args, ctx: json.dumps({"ok": True})
    )
    task = make_task(tmp_path)
    cfg = make_cfg(tmp_path)

    call = FakeToolCall(FakeFunction("web_search", {"query": "x"}))
    responses = [
        FakeResponse(FakeMessage(tool_calls=[call], thinking="considering the query...")),
        FakeResponse(FakeMessage(content="# Report")),
    ]
    client = FakeOllamaClient(responses)

    events: list = []
    run_task(task, cfg, client=client, on_event=events.append)

    joined = "\n".join(events)
    assert "thinking: considering the query" in joined
    assert "calling web_search" in joined
    assert "web_search ->" in joined
    assert "final report ready" in joined


def test_run_task_nudges_once_when_research_task_skips_tools(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "[]")
    task = make_task(tmp_path)  # web_search + fetch_page
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient(
        [
            FakeResponse(FakeMessage(content="# Invented briefing")),
            FakeResponse(
                FakeMessage(
                    tool_calls=[FakeToolCall(FakeFunction("web_search", {"query": "news"}))]
                )
            ),
            FakeResponse(FakeMessage(content="# Sourced briefing")),
        ]
    )
    events: list = []

    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.report == "# Sourced briefing"
    assert result.tool_calls == 1
    assert client.calls[1]["messages"][-1] == {
        "role": "user",
        "content": executor_module.NO_TOOLS_NUDGE,
    }
    assert any("asking it to research first" in e for e in events)


def test_run_task_flags_report_written_without_any_tools(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "unused")
    task = make_task(tmp_path)
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient(
        [
            FakeResponse(FakeMessage(content="# Briefing\n\nMade up.")),
            FakeResponse(FakeMessage(content="# Briefing\n\nStill made up.")),
        ]
    )

    result = run_task(task, cfg, client=client)

    assert result.tool_calls == 0
    assert result.report == (
        f"# Briefing\n\n{executor_module.NO_SOURCES_WARNING}\n\nStill made up."
    )


class FakeModelInfo:
    def __init__(self, model, size):
        self.model, self.size = model, size


class FakeListResponse:
    def __init__(self, models):
        self.models = models


class FakeShowResponse:
    def __init__(self, capabilities):
        self.capabilities = capabilities


class InstalledModelsClient(FakeOllamaClient):
    """Fake client that also answers list()/show() like a real Ollama."""

    def __init__(self, responses, installed):
        super().__init__(responses)
        self.installed = installed  # {name: (size, capabilities)}

    def list(self):
        return FakeListResponse(
            [FakeModelInfo(n, size) for n, (size, _) in self.installed.items()]
        )

    def show(self, name):
        if name not in self.installed:
            raise ollama.ResponseError(f"model '{name}' not found", 404)
        return FakeShowResponse(self.installed[name][1])


def test_run_task_falls_back_to_installed_model_when_configured_one_missing(
    tmp_path, monkeypatch
):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])  # task.model = fake-model
    cfg = make_cfg(tmp_path)
    client = InstalledModelsClient(
        [FakeResponse(FakeMessage(content="# Done"))],
        installed={
            "big-no-tools:7b": (9_000, ["completion"]),
            "granite4:3b": (2_000, ["completion", "tools"]),
            "tiny:1b": (800, ["completion", "tools"]),
        },
    )
    events: list = []

    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.model == "granite4:3b"  # largest that can call tools
    assert client.calls[0]["model"] == "granite4:3b"
    assert any("'fake-model' is not installed" in e for e in events)
    assert "using model granite4:3b" in events


def test_run_task_announces_model_in_use(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    cfg = make_cfg(tmp_path)
    client = InstalledModelsClient(
        [FakeResponse(FakeMessage(content="# Done"))],
        installed={"fake-model:latest": (1, ["completion", "tools"])},
    )
    events: list = []

    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.model == "fake-model"
    assert events[0] == "using model fake-model"


class DropsFirstCallClient(FakeOllamaClient):
    def chat(self, model, messages, tools=None, think=None):
        if not getattr(self, "dropped", False):
            self.dropped = True
            raise httpx.RemoteProtocolError("Server disconnected without sending a response.")
        return super().chat(model, messages, tools=tools, think=think)


def test_run_task_retries_once_when_ollama_drops_connection(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    monkeypatch.setattr(executor_module.time, "sleep", lambda s: None)
    ensured = []
    monkeypatch.setattr(
        executor_module.daemon_mod, "ensure_ollama_running", lambda cfg: ensured.append(cfg)
    )
    task = make_task(tmp_path, tools=["current_datetime"])
    cfg = make_cfg(tmp_path, agent=AgentConfig(think=False))
    client = DropsFirstCallClient([FakeResponse(FakeMessage(content="# Done"))])

    result = run_task(task, cfg, client=client)

    assert result.report == "# Done"
    assert len(ensured) == 1


def test_run_task_swaps_out_installed_model_that_cannot_call_tools(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    task.model = "gemma3:1b"
    cfg = make_cfg(tmp_path)
    client = InstalledModelsClient(
        [FakeResponse(FakeMessage(content="# Done"))],
        installed={
            "gemma3:1b": (800, ["completion"]),
            "granite4:3b": (2_000, ["completion", "tools"]),
        },
    )
    events: list = []

    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.model == "granite4:3b"
    assert any("'gemma3:1b' can't call tools" in e for e in events)


def test_run_task_keeps_tool_less_model_for_task_without_tools(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    task.model = "gemma3:1b"
    task.tools = []
    cfg = make_cfg(tmp_path)
    client = InstalledModelsClient(
        [FakeResponse(FakeMessage(content="# Done"))],
        installed={"gemma3:1b": (800, ["completion"]), "granite4:3b": (2_000, ["completion", "tools"])},
    )

    assert run_task(task, cfg, client=client).model == "gemma3:1b"


def test_run_task_swaps_out_installed_model_that_cannot_chat(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: "ok")
    task = make_task(tmp_path, tools=["current_datetime"])
    task.model = "nomic-embed-text:latest"
    task.tools = []
    cfg = make_cfg(tmp_path)
    client = InstalledModelsClient(
        [FakeResponse(FakeMessage(content="# Done"))],
        installed={
            "nomic-embed-text:latest": (9_000, ["embedding"]),
            "phonon:latest": (5_000, ["speech"]),
            "gemma3:1b": (800, ["completion"]),
        },
    )
    events: list = []

    result = run_task(task, cfg, client=client, on_event=events.append)

    assert result.model == "gemma3:1b"
    assert any("'nomic-embed-text:latest' can't chat" in e for e in events)


def test_run_task_retries_then_succeeds_when_report_unbacked(tmp_path, monkeypatch):
    monkeypatch.setattr(
        executor_module,
        "execute",
        lambda name, args, ctx: (
            json.dumps([{"title": "t", "url": args.get("url", "https://example.com"), "snippet": "s"}])
            if name == "web_search"
            else f'<fetched_content url="{args.get("url")}">deals text</fetched_content>'
        ),
    )
    task = make_task(tmp_path)
    task.sources = ["https://example.com/deals"]
    task.checks = ChecksSpec(require_cited_sources=True, min_report_links=1)
    cfg = make_cfg(tmp_path, agent=AgentConfig(max_iterations=6))

    fetch_ok = FakeToolCall(FakeFunction("fetch_page", {"url": "https://example.com/deals"}))
    responses = [
        FakeResponse(FakeMessage(tool_calls=[fetch_ok])),
        # unbacked: the "report" cites no URL the run really got
        FakeResponse(FakeMessage(content="# Deals\n\n| Made Up | $1 | ...")),
        # fixed after the CHECKS_FAILED retry
        FakeResponse(FakeMessage(content="# Deals\n\n| Real | $1 |\n\nhttps://example.com/deals")),
    ]
    result = run_task(task, cfg, client=FakeOllamaClient(responses))

    assert result.status == "success"
    assert result.check_failures == []


def test_run_task_records_failed_when_no_web_evidence(tmp_path, monkeypatch):
    monkeypatch.setattr(executor_module, "execute", lambda name, args, ctx: '{"error": "search unavailable"}')
    task = make_task(tmp_path)
    task.sources = ["https://example.com/deals"]
    task.checks = ChecksSpec(require_sources=True, require_cited_sources=True)
    cfg = make_cfg(tmp_path, agent=AgentConfig(max_iterations=6))

    responses = [
        FakeResponse(FakeMessage(tool_calls=[FakeToolCall(FakeFunction("web_search", {"query": "x"}))])),
        FakeResponse(FakeMessage(content="# Deals\n\nnothing here")),
        FakeResponse(FakeMessage(content="# Deals\n\nstill nothing")),
    ]
    result = run_task(task, cfg, client=FakeOllamaClient(responses))

    assert result.status == "failed"
    assert result.check_failures
