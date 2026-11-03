"""Starter tasks: seeded once on first run into an untouched tasks dir,
user-deletable forever after (soft-persistent)."""

from __future__ import annotations

from pathlib import Path

from schedlm import seed as seed_mod

FOUR_STARTERS = {
    "daily-briefing.md",
    "daily-game-deals.md",
    "github-trending.md",
    "weekly-market-summary.md",
}


def test_first_run_seeds_the_four_starters(tmp_path: Path) -> None:
    tasks_dir = tmp_path / "tasks"
    seeded = seed_mod.ensure_starter_tasks(tasks_dir)
    assert set(seeded) == FOUR_STARTERS
    assert {p.name for p in tasks_dir.glob("*.md")} == FOUR_STARTERS
    assert (tasks_dir / seed_mod.MARKER).exists()


def test_seed_is_permanent_not_resurrecting(tmp_path: Path) -> None:
    """User deletes the starters: they must not come back on later runs."""
    tasks_dir = tmp_path / "tasks"
    seed_mod.ensure_starter_tasks(tasks_dir)
    for f in tasks_dir.glob("*.md"):
        f.unlink()
    again = seed_mod.ensure_starter_tasks(tasks_dir)
    assert again == []
    assert list(tasks_dir.glob("*.md")) == []


def test_non_empty_tasks_dir_is_untouched(tmp_path: Path) -> None:
    """Existing installs (the user's own tasks) are never seeded over."""
    tasks_dir = tmp_path / "tasks"
    tasks_dir.mkdir()
    mine = tasks_dir / "mine.md"
    mine.write_text("---\nid: mine\n---\n\nmy task\n")
    seeded = seed_mod.ensure_starter_tasks(tasks_dir)
    assert seeded == []
    assert mine.exists()
    assert not (tasks_dir / "daily-briefing.md").exists()
    # ...but the marker is written so this install never auto-seeds later.
    assert (tasks_dir / seed_mod.MARKER).exists()


def test_seeded_files_parse_as_valid_tasks(tmp_path: Path) -> None:
    from schedlm import tasks as tasks_mod

    tasks_dir = tmp_path / "tasks"
    seed_mod.ensure_starter_tasks(tasks_dir)
    valid, invalid = tasks_mod.load_tasks_dir(tasks_dir)
    assert invalid == []
    assert {t.id for t in valid} == {p[: -len(".md")] for p in FOUR_STARTERS}


def test_second_call_is_a_noop(tmp_path: Path) -> None:
    tasks_dir = tmp_path / "tasks"
    seed_mod.ensure_starter_tasks(tasks_dir)
    marker_mtime = (tasks_dir / seed_mod.MARKER).stat().st_mtime_ns
    assert seed_mod.ensure_starter_tasks(tasks_dir) == []
    assert (tasks_dir / seed_mod.MARKER).stat().st_mtime_ns == marker_mtime
