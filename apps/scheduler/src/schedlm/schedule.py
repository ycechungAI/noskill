"""Cron/schedule math. See spec.md FR-4..FR-8 and Testing.md test_schedule.py."""

from __future__ import annotations

from datetime import datetime, timedelta, tzinfo
from pathlib import Path
from zoneinfo import ZoneInfo

from croniter import croniter

from schedlm.config import Config


def _system_tz_name() -> str | None:
    """Best-effort IANA zone name from /etc/localtime (macOS/Linux)."""
    try:
        localtime = Path("/etc/localtime")
        if localtime.is_symlink():
            parts = localtime.resolve().parts
            if "zoneinfo" in parts:
                idx = parts.index("zoneinfo")
                return "/".join(parts[idx + 1 :])
    except OSError:
        pass
    return None


def resolve_tz(cfg: Config) -> tzinfo:
    """Resolve cfg.timezone to a tzinfo.

    'local' prefers a real IANA zone (via /etc/localtime) so DST transitions
    are handled correctly (spec FR-4); it falls back to the current fixed
    UTC offset only if the zone name can't be determined, since a fixed
    offset does not shift across DST changes.
    """
    if cfg.timezone != "local":
        return ZoneInfo(cfg.timezone)
    name = _system_tz_name()
    if name:
        try:
            return ZoneInfo(name)
        except Exception:
            pass
    return datetime.now().astimezone().tzinfo


def next_run(cron: str, after: datetime, tz: tzinfo) -> datetime:
    """Next cron fire time strictly after `after`, evaluated in `tz`."""
    if after.tzinfo is None:
        after = after.replace(tzinfo=tz)
    after_local = after.astimezone(tz)
    itr = croniter(cron, after_local)
    nxt = itr.get_next(datetime)
    if nxt.tzinfo is None:
        nxt = nxt.replace(tzinfo=tz)
    return nxt


def is_due(next_run_at: datetime, now: datetime) -> bool:
    """A task fires when now >= next_run_at (spec FR-5)."""
    return now >= next_run_at


def is_one_time_due(fire_at: datetime, now: datetime) -> bool:
    return now >= fire_at


def catchup_decision(
    next_run_at: datetime, now: datetime, catchup: bool, grace_minutes: int
) -> bool:
    """Return True if a missed run should fire once now (spec FR-7).

    Callers are responsible for advancing next_run_at to the next future
    slot regardless of this result (Architecture.md §4 Startup/catch-up).
    """
    if not catchup:
        return False
    if now < next_run_at:
        return False
    lateness = now - next_run_at
    return lateness <= timedelta(minutes=grace_minutes)
