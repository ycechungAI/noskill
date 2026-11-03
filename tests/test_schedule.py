from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

from schedlm.config import Config
from schedlm.schedule import (
    catchup_decision,
    is_due,
    is_one_time_due,
    next_run,
    resolve_tz,
)


def test_next_run_weekday_schedule_friday_to_monday():
    tz = ZoneInfo("America/New_York")
    friday_morning = datetime(2026, 7, 17, 8, 0, tzinfo=tz)  # Friday
    nxt = next_run("0 7 * * 1-5", friday_morning, tz)
    assert nxt == datetime(2026, 7, 20, 7, 0, tzinfo=tz)  # Monday 07:00


def test_next_run_dst_spring_forward_monotonic():
    tz = ZoneInfo("America/New_York")
    cursor = datetime(2026, 3, 6, 12, 0, tzinfo=tz)
    times = []
    for _ in range(5):
        cursor = next_run("30 2 * * *", cursor, tz)
        times.append(cursor)
    assert times == sorted(times)
    assert all(t.tzinfo is not None for t in times)


def test_next_run_dst_fall_back_monotonic():
    tz = ZoneInfo("America/New_York")
    cursor = datetime(2026, 10, 30, 12, 0, tzinfo=tz)
    times = []
    for _ in range(5):
        cursor = next_run("30 2 * * *", cursor, tz)
        times.append(cursor)
    assert times == sorted(times)
    assert all(t.tzinfo is not None for t in times)


def test_is_due_boundary():
    tz = ZoneInfo("UTC")
    next_run_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    assert is_due(next_run_at, datetime(2026, 7, 18, 7, 0, tzinfo=tz)) is True
    assert is_due(next_run_at, datetime(2026, 7, 18, 7, 0, 1, tzinfo=tz)) is True
    assert is_due(next_run_at, datetime(2026, 7, 18, 6, 59, 59, tzinfo=tz)) is False


def test_is_one_time_due():
    tz = ZoneInfo("UTC")
    fire_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    assert is_one_time_due(fire_at, datetime(2026, 7, 19, tzinfo=tz)) is True
    assert is_one_time_due(fire_at, datetime(2026, 7, 17, tzinfo=tz)) is False


def test_catchup_decision_within_grace():
    tz = ZoneInfo("UTC")
    next_run_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    now = next_run_at + timedelta(minutes=10)
    assert catchup_decision(next_run_at, now, catchup=True, grace_minutes=60) is True


def test_catchup_decision_beyond_grace():
    tz = ZoneInfo("UTC")
    next_run_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    now = next_run_at + timedelta(minutes=90)
    assert catchup_decision(next_run_at, now, catchup=True, grace_minutes=60) is False


def test_catchup_decision_flag_off():
    tz = ZoneInfo("UTC")
    next_run_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    now = next_run_at + timedelta(minutes=5)
    assert catchup_decision(next_run_at, now, catchup=False, grace_minutes=60) is False


def test_catchup_decision_not_yet_due():
    tz = ZoneInfo("UTC")
    next_run_at = datetime(2026, 7, 18, 7, 0, tzinfo=tz)
    now = next_run_at - timedelta(minutes=5)
    assert catchup_decision(next_run_at, now, catchup=True, grace_minutes=60) is False


def test_resolve_tz_named_zone():
    cfg = Config(timezone="America/New_York")
    tz = resolve_tz(cfg)
    assert str(tz) == "America/New_York"


def test_resolve_tz_local_is_usable():
    cfg = Config(timezone="local")
    tz = resolve_tz(cfg)
    # Must be usable for schedule math without raising, regardless of
    # whether it resolved to a real IANA zone or the fixed-offset fallback.
    result = next_run("0 7 * * *", datetime.now(tz), tz)
    assert result.tzinfo is not None
