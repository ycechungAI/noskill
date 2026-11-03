import time
from datetime import datetime, timezone
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from schedlm import daemon, store, webapi
from schedlm.config import Config, PathsConfig
from schedlm.report import RunResult

TASK_TEXT = """---
id: sample-task
schedule: "0 8 * * *"
enabled: true
tools: []
---

Do the thing.
"""


def make_cfg(tmp_path: Path) -> Config:
    return Config(
        timezone="UTC",
        paths=PathsConfig(
            tasks=str(tmp_path / "tasks"),
            runs=str(tmp_path / "runs"),
            data=str(tmp_path / "data"),
        ),
    )


@pytest.fixture
def client(tmp_path):
    cfg = make_cfg(tmp_path)
    (tmp_path / "tasks").mkdir()
    app = webapi.create_app(cfg, seed_starters=False)
    return TestClient(app)


def test_list_empty(client):
    r = client.get("/api/tasks")
    assert r.status_code == 200
    assert r.json() == {"tasks": [], "invalid": []}


def test_create_get_and_list(client):
    r = client.post("/api/tasks/file", json={"content": TASK_TEXT})
    assert r.status_code == 201
    assert r.json() == {"filename": "sample-task.md", "id": "sample-task"}

    r = client.get("/api/tasks/file/sample-task.md")
    assert r.status_code == 200
    assert r.json()["content"] == TASK_TEXT

    r = client.get("/api/tasks")
    tasks = r.json()["tasks"]
    assert len(tasks) == 1
    assert tasks[0]["id"] == "sample-task"
    assert tasks[0]["number"] == 1
    assert tasks[0]["enabled"] is True


def test_create_duplicate_id_rejected(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    r = client.post(
        "/api/tasks/file", json={"content": TASK_TEXT, "filename": "other.md"}
    )
    assert r.status_code == 409


def test_create_invalid_content_rejected(client):
    r = client.post("/api/tasks/file", json={"content": "not a task file"})
    assert r.status_code == 400
    assert "frontmatter" in r.json()["detail"]


def test_update_task_file(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    new_text = TASK_TEXT.replace("Do the thing.", "Do the other thing.")
    r = client.put("/api/tasks/file/sample-task.md", json={"content": new_text})
    assert r.status_code == 200
    r = client.get("/api/tasks/file/sample-task.md")
    assert "other thing" in r.json()["content"]


def test_update_nonexistent_file_404(client):
    r = client.put("/api/tasks/file/nope.md", json={"content": TASK_TEXT})
    assert r.status_code == 404


def test_update_id_clash_rejected(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    other_text = TASK_TEXT.replace("id: sample-task", "id: other-task")
    client.post("/api/tasks/file", json={"content": other_text, "filename": "other.md"})

    clashing = other_text.replace("id: other-task", "id: sample-task")
    r = client.put("/api/tasks/file/other.md", json={"content": clashing})
    assert r.status_code == 409


def test_delete_task_file(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    r = client.delete("/api/tasks/file/sample-task.md")
    assert r.status_code == 200
    r = client.get("/api/tasks")
    assert r.json() == {"tasks": [], "invalid": []}


def test_delete_invalid_file_still_works(client, tmp_path):
    (tmp_path / "tasks" / "broken.md").write_text("garbage")
    r = client.get("/api/tasks")
    assert r.json()["invalid"][0]["filename"] == "broken.md"
    r = client.delete("/api/tasks/file/broken.md")
    assert r.status_code == 200
    r = client.get("/api/tasks")
    assert r.json()["invalid"] == []


def test_enable_disable(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    r = client.post("/api/tasks/sample-task/disable")
    assert r.status_code == 200
    assert r.json() == {"id": "sample-task", "enabled": False}
    content = client.get("/api/tasks/file/sample-task.md").json()["content"]
    assert "enabled: false" in content

    r = client.post("/api/tasks/sample-task/enable")
    assert r.json()["enabled"] is True


def test_enable_after_disable_reschedules(client, tmp_path):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    client.post("/api/tasks/sample-task/disable")
    conn = store.init_db(tmp_path / "data" / "sched.db")
    assert store.get_task_state(conn, "sample-task")["next_run_at"] is None

    client.post("/api/tasks/sample-task/enable")

    row = store.get_task_state(conn, "sample-task")
    assert datetime.fromisoformat(row["next_run_at"]) > datetime.now(timezone.utc)
    conn.close()


def test_enable_unknown_task_404(client):
    r = client.post("/api/tasks/nope/enable")
    assert r.status_code == 404


def test_run_task_streams_events_and_report(client, monkeypatch, tmp_path):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})

    def fake_run_task(task, cfg, client=None, on_event=None):
        if on_event:
            on_event("calling nothing")
            on_event("final report ready")
        return RunResult(report="# Sample Report\n\nhello\n", iterations=1, tool_calls=0, model="x")

    monkeypatch.setattr(webapi, "agent_run_task", fake_run_task)
    monkeypatch.setattr(daemon, "ensure_ollama_running", lambda cfg: True)

    r = client.post("/api/tasks/sample-task/run")
    assert r.status_code == 200
    token = r.json()["run_token"]

    deadline = time.monotonic() + 5
    body = ""
    while time.monotonic() < deadline:
        r = client.get(f"/api/runs/stream/{token}")
        if r.status_code == 200:
            body = r.text
            break
        time.sleep(0.05)

    assert '"done": true' in body
    assert '"status": "success"' in body
    assert "final report ready" in body

    runs = client.get("/api/tasks/sample-task/runs").json()
    assert len(runs) == 1
    assert runs[0]["status"] == "success"

    report_text = client.get(f"/api/runs/{runs[0]['run_id']}/report").text
    assert "Sample Report" in report_text


def test_run_unknown_task_404(client):
    r = client.post("/api/tasks/nope/run")
    assert r.status_code == 404


def test_run_ollama_unreachable_503(client, monkeypatch):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    monkeypatch.setattr(daemon, "ensure_ollama_running", lambda cfg: False)
    r = client.post("/api/tasks/sample-task/run")
    assert r.status_code == 503


class _FakeModel:
    def __init__(self, model, size):
        self.model, self.size = model, size


class _FakeShow:
    def __init__(self, capabilities):
        self.capabilities = capabilities


class _FakeOllama:
    INSTALLED = {
        "granite4:3b": (2_000, ["completion", "tools"]),
        "gemma3:1b": (800, ["completion"]),
        "nomic-embed-text:latest": (274, ["embedding"]),
    }

    def __init__(self, host=None, timeout=None):
        pass

    def list(self):
        class R:
            models = [_FakeModel(n, size) for n, (size, _) in _FakeOllama.INSTALLED.items()]

        return R()

    def show(self, name):
        return _FakeShow(_FakeOllama.INSTALLED[name][1])


@pytest.fixture
def config_client(tmp_path, monkeypatch):
    monkeypatch.setattr(webapi.ollama, "Client", _FakeOllama)
    (tmp_path / "tasks").mkdir()
    config_path = tmp_path / "config.yaml"
    config_path.write_text("ollama:\n  model: qwen3:8b\n")
    app = webapi.create_app(make_cfg(tmp_path), config_path=config_path, seed_starters=False)
    return TestClient(app), config_path


def test_models_reports_fallback_for_missing_default(config_client):
    client, _ = config_client

    data = client.get("/api/models").json()

    assert data["ollama_up"] is True
    assert data["default"] == "qwen3:8b"
    assert data["effective_default"] == "granite4:3b"
    assert "not installed" in data["note"]
    # The embedding model can't chat, so it isn't offered.
    assert [m["name"] for m in data["installed"]] == ["granite4:3b", "gemma3:1b"]
    assert data["installed"][1]["tools"] is False


def test_models_when_ollama_down(tmp_path, monkeypatch):
    class Down(_FakeOllama):
        def list(self):
            raise ConnectionError("refused")

    monkeypatch.setattr(webapi.ollama, "Client", Down)
    (tmp_path / "tasks").mkdir()
    client = TestClient(webapi.create_app(make_cfg(tmp_path), seed_starters=False))

    data = client.get("/api/models").json()

    assert data["ollama_up"] is False
    assert data["installed"] == []
    assert data["can_change_default"] is False


def test_switch_default_model_persists_and_applies(config_client):
    client, config_path = config_client

    r = client.put("/api/models/default", json={"model": "granite4:3b"})

    assert r.status_code == 200
    assert 'model: "granite4:3b"' in config_path.read_text()
    data = client.get("/api/models").json()
    assert data["default"] == "granite4:3b"
    assert data["note"] is None


def test_switch_default_model_rejects_bad_name(config_client):
    client, _ = config_client
    assert client.put("/api/models/default", json={"model": "x\ny"}).status_code == 400
    assert client.put("/api/models/default", json={"model": None}).status_code == 400


def test_set_and_clear_task_model(client):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})

    r = client.put("/api/tasks/sample-task/model", json={"model": "gemma3:1b"})

    assert r.json() == {"id": "sample-task", "model": "gemma3:1b"}
    task = client.get("/api/tasks").json()["tasks"][0]
    assert task["model"] == "gemma3:1b"

    client.put("/api/tasks/sample-task/model", json={"model": None})
    assert client.get("/api/tasks").json()["tasks"][0]["model"] is None
    assert "model:" not in client.get("/api/tasks/file/sample-task.md").json()["content"]


def test_set_task_model_unknown_task_404(client):
    assert client.put("/api/tasks/nope/model", json={"model": "a:1b"}).status_code == 404


# --- Describe a task: POST /api/tasks/draft ---------------------------------


def _fake_planner(monkeypatch, task_id="news-digest", seen=None):
    def fake(sentence, cfg, client=None):
        if seen is not None:
            seen.append(sentence)
        text = TASK_TEXT.replace("sample-task", task_id)
        return webapi.tasks_mod.parse_task_text(text, Path("/tmp/x.md"))

    monkeypatch.setattr(webapi.planner_mod, "nl_to_task", fake)


def test_draft_fills_in_a_task_without_saving(client, tmp_path, monkeypatch):
    _fake_planner(monkeypatch)
    r = client.post("/api/tasks/draft", json={"text": "every day at 8, the news"})
    assert r.status_code == 200
    assert r.json()["id"] == "news-digest"
    assert "id: news-digest" in r.json()["content"]
    assert list((tmp_path / "tasks").iterdir()) == []


def test_draft_picks_a_free_id(client, monkeypatch):
    client.post("/api/tasks/file", json={"content": TASK_TEXT})
    _fake_planner(monkeypatch, task_id="sample-task")
    r = client.post("/api/tasks/draft", json={"text": "do the thing"})
    assert r.json()["id"] == "sample-task-2"


def test_a_follow_up_sends_the_current_draft(client, monkeypatch):
    seen: list = []
    _fake_planner(monkeypatch, seen=seen)
    client.post("/api/tasks/draft", json={"text": "make it 9am", "draft": TASK_TEXT})
    assert "make it 9am" in seen[0] and "id: sample-task" in seen[0]
    assert webapi.draft_request("hi", None) == "hi"
    assert webapi.draft_request("hi", "  ") == "hi"


def test_draft_errors_are_readable(client, monkeypatch):
    assert client.post("/api/tasks/draft", json={"text": "  "}).status_code == 400

    def fails(sentence, cfg, client=None):
        raise webapi.planner_mod.PlannerError("no valid task after 3 attempts")

    monkeypatch.setattr(webapi.planner_mod, "nl_to_task", fails)
    r = client.post("/api/tasks/draft", json={"text": "x"})
    assert r.status_code == 422 and "3 attempts" in r.json()["detail"]

    def down(sentence, cfg, client=None):
        raise ConnectionError("connection refused")

    monkeypatch.setattr(webapi.planner_mod, "nl_to_task", down)
    assert client.post("/api/tasks/draft", json={"text": "x"}).status_code == 503


def test_the_page_has_the_describe_box(client):
    page = client.get("/").text
    assert 'id="describe-text"' in page and "/api/tasks/draft" in page
