from dataclasses import dataclass

import pytest

from schedlm.agent.planner import PlannerError, nl_to_task
from schedlm.config import Config, PathsConfig


@dataclass
class FakeMessage:
    content: str


@dataclass
class FakeResponse:
    message: FakeMessage


class FakeOllamaClient:
    def __init__(self, replies):
        self._replies = list(replies)
        self.calls: list = []

    def chat(self, model, messages):
        self.calls.append({"model": model, "messages": list(messages)})
        return FakeResponse(FakeMessage(content=self._replies.pop(0)))


def make_cfg(tmp_path) -> Config:
    return Config(
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        )
    )


VALID_REPLY = """
{"id": "ollama-watch", "name": "Ollama Watch",
 "schedule": "0 9 * * 1", "fire_at": null,
 "tools": ["web_search"],
 "prompt": "Check for new Ollama model releases this week and summarize with links."}
"""


class _Model:
    def __init__(self, model, size):
        self.model, self.size = model, size


class _Show:
    def __init__(self, capabilities):
        self.capabilities = capabilities


class InstalledModelsClient(FakeOllamaClient):
    def __init__(self, replies, installed):
        super().__init__(replies)
        self.installed = installed  # {name: (size, capabilities)}

    def list(self):
        class R:
            models = [_Model(n, size) for n, (size, _) in self.installed.items()]

        return R()

    def show(self, name):
        return _Show(self.installed[name][1])


def test_nl_to_task_falls_back_when_planner_model_not_installed(tmp_path):
    cfg = make_cfg(tmp_path)  # planner_model defaults to qwen3:8b
    client = InstalledModelsClient(
        [VALID_REPLY],
        installed={
            "nomic-embed-text:latest": (9_000, ["embedding"]),
            "granite4.2:3b": (2_000, ["completion", "tools"]),
        },
    )

    task = nl_to_task("every monday at 9am check for new ollama releases", cfg, client=client)

    assert task.id == "ollama-watch"
    assert client.calls[0]["model"] == "granite4.2:3b"


def test_nl_to_task_valid_json(tmp_path):
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient([VALID_REPLY])

    task = nl_to_task("every monday at 9am check for new ollama releases", cfg, client=client)

    assert task.id == "ollama-watch"
    assert task.schedule == "0 9 * * 1"
    assert task.fire_at is None
    assert task.tools == ["web_search"]
    assert "Ollama" in task.prompt
    assert len(client.calls) == 1


def test_nl_to_task_retries_after_invalid_json_then_succeeds(tmp_path):
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient(["not json at all", VALID_REPLY])

    task = nl_to_task("every monday at 9am check for new ollama releases", cfg, client=client)

    assert task.id == "ollama-watch"
    assert len(client.calls) == 2
    # the retry must carry corrective feedback back to the model
    retry_messages = client.calls[1]["messages"]
    assert retry_messages[-1]["role"] == "user"
    assert "invalid" in retry_messages[-1]["content"].lower()


def test_nl_to_task_retries_after_validation_error_then_succeeds(tmp_path):
    cfg = make_cfg(tmp_path)
    bad_cron_reply = VALID_REPLY.replace('"0 9 * * 1"', '"not a cron"')
    client = FakeOllamaClient([bad_cron_reply, VALID_REPLY])

    task = nl_to_task("every monday at 9am", cfg, client=client)

    assert task.schedule == "0 9 * * 1"
    assert len(client.calls) == 2


def test_nl_to_task_both_attempts_invalid_raises_planner_error(tmp_path):
    cfg = make_cfg(tmp_path)
    client = FakeOllamaClient(["garbage", "still garbage"])

    with pytest.raises(PlannerError):
        nl_to_task("do something", cfg, client=client)

    assert len(client.calls) == 2


def test_nl_to_task_strips_markdown_fences(tmp_path):
    cfg = make_cfg(tmp_path)
    fenced = "```json\n" + VALID_REPLY.strip() + "\n```"
    client = FakeOllamaClient([fenced])

    task = nl_to_task("every monday at 9am", cfg, client=client)

    assert task.id == "ollama-watch"


def test_nl_to_task_defaults_missing_fields(tmp_path):
    cfg = make_cfg(tmp_path)
    reply = (
        '{"id": "bare-task", "schedule": "0 8 * * *", "fire_at": null, '
        '"prompt": "Summarize something useful."}'
    )
    client = FakeOllamaClient([reply])

    task = nl_to_task("do a daily thing", cfg, client=client)

    assert task.id == "bare-task"
    assert task.enabled is True
    assert task.timeout_minutes == 10
    assert task.catchup is False
    assert set(task.tools) == {
        "web_search",
        "fetch_page",
        "current_datetime",
        "read_note",
        "write_note",
    }
