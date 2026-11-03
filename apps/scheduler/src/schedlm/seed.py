"""Soft-persistent starter tasks: seeded once on first run, user-deletable.

The project ships ready-made tasks under `starter_tasks/`. On first run, if
the tasks directory has no task files, they are copied in and the tasks
directory is marked as seeded (`.starters-seeded`). Deleting them afterwards
— or deleting every task — is permanent: the marker is never removed, so the
starters never reappear.

An existing, non-empty tasks directory is left untouched (marker is still
written, so this install never gets auto-seeded later either).
"""

from __future__ import annotations

import logging
import shutil
from datetime import datetime, timezone
from pathlib import Path

logger = logging.getLogger(__name__)

STARTER_TASKS_DIR = Path(__file__).resolve().parents[2] / "starter_tasks"
MARKER = ".starters-seeded"


def ensure_starter_tasks(
    tasks_dir: Path, starter_dir: Path | None = None
) -> list[str]:
    """Copy starter tasks into an untouched tasks dir. Returns the filenames
    copied (empty when nothing to do). Idempotent; safe to call on every
    startup."""
    starter_dir = Path(starter_dir) if starter_dir is not None else STARTER_TASKS_DIR
    marker = tasks_dir / MARKER
    if marker.exists():
        return []
    tasks_dir.mkdir(parents=True, exist_ok=True)
    seeded: list[str] = []
    if not list(tasks_dir.glob("*.md")) and starter_dir.is_dir():
        for src in sorted(starter_dir.glob("*.md")):
            dest = tasks_dir / src.name
            if dest.exists():
                continue
            shutil.copy2(src, dest)
            seeded.append(src.name)
    marker.write_text(
        "starter tasks were handled at "
        f"{datetime.now(timezone.utc).isoformat()}: "
        + (", ".join(seeded) if seeded else "(existing tasks left untouched)")
        + "\n"
    )
    if seeded:
        logger.info("seeded %d starter task(s): %s", len(seeded), ", ".join(seeded))
    return seeded
