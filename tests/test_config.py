import os

import pytest

from schedlm.config import ConfigWatcher, load_config, set_default_model, valid_model_name


def test_set_default_model_creates_config_file(tmp_path):
    path = tmp_path / "config.yaml"

    set_default_model(path, "granite4:3b")

    assert load_config(path).ollama.model == "granite4:3b"


def test_set_default_model_preserves_other_keys_and_comments(tmp_path):
    path = tmp_path / "config.yaml"
    path.write_text(
        "# my settings\n"
        "timezone: UTC\n"
        "ollama:\n"
        "    host: http://localhost:11434   # local\n"
        "    model: qwen3:8b   # executor default\n"
        "    planner_model: qwen3:4b\n"
        "agent:\n"
        "  max_iterations: 5\n"
    )

    set_default_model(path, "granite4:3b")

    text = path.read_text()
    assert "# my settings" in text and "# local" in text
    assert '    model: "granite4:3b"   # executor default\n' in text
    cfg = load_config(path)
    assert cfg.ollama.model == "granite4:3b"
    assert cfg.ollama.planner_model == "qwen3:4b"
    assert cfg.agent.max_iterations == 5


def test_set_default_model_adds_key_to_existing_section(tmp_path):
    path = tmp_path / "config.yaml"
    path.write_text("ollama:\n  host: http://localhost:11434\ndaemon:\n  tick_seconds: 10\n")

    set_default_model(path, "llama3.2")

    cfg = load_config(path)
    assert cfg.ollama.model == "llama3.2"
    assert cfg.ollama.host == "http://localhost:11434"
    assert cfg.daemon.tick_seconds == 10


@pytest.mark.parametrize("name", ["", "a b", "x\ny", "model: evil", "-rf"])
def test_set_default_model_rejects_bad_names(tmp_path, name):
    assert not valid_model_name(name)
    with pytest.raises(ValueError):
        set_default_model(tmp_path / "config.yaml", name)


def test_config_watcher_reloads_on_change(tmp_path):
    path = tmp_path / "config.yaml"
    path.write_text("ollama:\n  model: a:1b\n")
    watcher = ConfigWatcher(path)
    assert watcher.current().ollama.model == "a:1b"

    path.write_text("ollama:\n  model: b:2b\n")
    st = path.stat()
    os.utime(path, (st.st_atime, st.st_mtime + 5))  # mtime granularity

    assert watcher.current().ollama.model == "b:2b"


def test_config_watcher_keeps_last_good_config_on_bad_yaml(tmp_path):
    path = tmp_path / "config.yaml"
    path.write_text("ollama:\n  model: a:1b\n")
    watcher = ConfigWatcher(path)

    path.write_text("ollama: [unclosed\n")
    st = path.stat()
    os.utime(path, (st.st_atime, st.st_mtime + 5))

    assert watcher.current().ollama.model == "a:1b"
