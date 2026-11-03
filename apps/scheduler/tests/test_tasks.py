import dataclasses
import logging
from pathlib import Path

import pytest

from schedlm.tasks import (
    OutputSpec,
    Task,
    TaskValidationError,
    content_hash,
    load_tasks_dir,
    parse_task_file,
    set_enabled,
    set_model,
    write_task_file,
)

VALID_FILE = """\
---
id: my-task
schedule: "0 7 * * *"
tools: [web_search, fetch_page]
---

Do the thing.
"""


def write(tmp_path: Path, name: str, text: str) -> Path:
    path = tmp_path / name
    path.write_text(text)
    return path


def test_round_trip(tmp_path):
    src = write(tmp_path, "my-task.md", VALID_FILE)
    task = parse_task_file(src)

    dest = tmp_path / "rewritten.md"
    rewritten = dataclasses.replace(task, path=dest)
    write_task_file(rewritten)

    reparsed = parse_task_file(dest)
    assert reparsed.id == task.id
    assert reparsed.schedule == task.schedule
    assert reparsed.fire_at == task.fire_at
    assert reparsed.enabled == task.enabled
    assert set(reparsed.tools) == set(task.tools)
    assert reparsed.timeout_minutes == task.timeout_minutes
    assert reparsed.catchup == task.catchup
    assert reparsed.output.keep == task.output.keep
    assert reparsed.prompt.strip() == task.prompt.strip()


def test_missing_id(tmp_path):
    path = write(
        tmp_path,
        "bad.md",
        "---\nschedule: \"0 7 * * *\"\n---\n\nDo it.\n",
    )
    with pytest.raises(TaskValidationError, match="id"):
        parse_task_file(path)


def test_both_schedule_and_fire_at(tmp_path):
    path = write(
        tmp_path,
        "bad.md",
        '---\nid: x\nschedule: "0 7 * * *"\nfire_at: "2026-07-19T15:00:00"\n---\n\nDo it.\n',
    )
    with pytest.raises(TaskValidationError, match="schedule/fire_at"):
        parse_task_file(path)


def test_neither_schedule_nor_fire_at(tmp_path):
    path = write(tmp_path, "bad.md", "---\nid: x\n---\n\nDo it.\n")
    with pytest.raises(TaskValidationError, match="schedule/fire_at"):
        parse_task_file(path)


def test_bad_cron(tmp_path):
    path = write(
        tmp_path,
        "bad.md",
        '---\nid: x\nschedule: "not a cron"\n---\n\nDo it.\n',
    )
    with pytest.raises(TaskValidationError, match="schedule"):
        parse_task_file(path)


def test_unknown_tool(tmp_path):
    path = write(
        tmp_path,
        "bad.md",
        '---\nid: x\nschedule: "0 7 * * *"\ntools: [not_a_real_tool]\n---\n\nDo it.\n',
    )
    with pytest.raises(TaskValidationError, match="tools"):
        parse_task_file(path)


def test_empty_prompt_body(tmp_path):
    path = write(tmp_path, "bad.md", '---\nid: x\nschedule: "0 7 * * *"\n---\n\n   \n')
    with pytest.raises(TaskValidationError, match="prompt"):
        parse_task_file(path)


def test_unknown_frontmatter_key_warns(tmp_path, caplog):
    path = write(
        tmp_path,
        "ok.md",
        '---\nid: x\nschedule: "0 7 * * *"\nsurprise: 1\n---\n\nDo it.\n',
    )
    with caplog.at_level(logging.WARNING):
        task = parse_task_file(path)
    assert task.id == "x"
    assert any("unknown frontmatter key" in m for m in caplog.messages)


def test_duplicate_ids_first_wins(tmp_path):
    write(tmp_path, "a.md", '---\nid: dup\nschedule: "0 7 * * *"\n---\n\nA.\n')
    write(tmp_path, "b.md", '---\nid: dup\nschedule: "0 8 * * *"\n---\n\nB.\n')
    valid, invalid = load_tasks_dir(tmp_path)
    assert len(valid) == 1
    assert valid[0].path.name == "a.md"
    assert len(invalid) == 1
    assert "duplicate id" in invalid[0][1]


def test_set_enabled_preserves_comments(tmp_path):
    text = (
        "---\n"
        "id: my-task\n"
        "schedule: \"0 8 * * *\"   # ASSUMED daily 08:00\n"
        "enabled: true\n"
        "---\n\n"
        "Do the thing.\n"
    )
    path = write(tmp_path, "my-task.md", text)
    set_enabled(path, False)
    new_text = path.read_text()
    assert "# ASSUMED daily 08:00" in new_text
    assert "enabled: false" in new_text
    task = parse_task_file(path)
    assert task.enabled is False


def test_content_hash_changes_with_content(tmp_path):
    path = write(tmp_path, "x.md", VALID_FILE)
    h1 = content_hash(path)
    path.write_text(VALID_FILE + "\nmore\n")
    h2 = content_hash(path)
    assert h1 != h2


MODEL_TASK = (
    "---\n"
    "id: my-task\n"
    "name: My Task\n"
    "schedule: \"0 8 * * *\"   # ASSUMED daily 08:00\n"
    "---\n\n"
    "Do the thing.\n"
)


def test_set_model_inserts_after_name_and_preserves_comments(tmp_path):
    path = write(tmp_path, "my-task.md", MODEL_TASK)

    set_model(path, "granite4:3b")

    text = path.read_text()
    assert 'name: My Task\nmodel: "granite4:3b"\n' in text
    assert "# ASSUMED daily 08:00" in text
    assert parse_task_file(path).model == "granite4:3b"


def test_set_model_replaces_then_clears(tmp_path):
    path = write(tmp_path, "my-task.md", MODEL_TASK)
    set_model(path, "a:1b")
    set_model(path, "b:2b")
    assert parse_task_file(path).model == "b:2b"
    assert path.read_text().count("model:") == 1

    set_model(path, None)

    assert parse_task_file(path).model is None
    assert path.read_text() == MODEL_TASK
