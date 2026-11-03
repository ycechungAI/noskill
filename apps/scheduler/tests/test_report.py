import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

from schedlm.report import (
    RunResult,
    format_duration,
    prune_reports,
    resolve_output_path,
    write_report,
)
from schedlm.tasks import OutputSpec

TZ = ZoneInfo("UTC")


def make_result(text: str = "hello") -> RunResult:
    return RunResult(
        report=f"# Title\n\n{text}\n", iterations=2, tool_calls=3, model="qwen3:8b"
    )


def test_resolve_output_path_default(tmp_path):
    output = OutputSpec()
    when = datetime(2026, 7, 18, 7, 0, tzinfo=TZ)
    path = resolve_output_path(
        output, "mytask", when, default_runs_dir=tmp_path / "runs"
    )
    assert path == tmp_path / "runs" / "mytask" / "2026-07-18T07-00.md"


def test_resolve_output_path_templated_dir_and_filename(tmp_path):
    output = OutputSpec(
        dir=str(tmp_path / "{year}" / "{month_name}"), filename="day{day}.md"
    )
    when = datetime(2026, 7, 18, 7, 0, tzinfo=TZ)
    path = resolve_output_path(output, "daily-briefing", when)
    assert path == tmp_path / "2026" / "July" / "day18.md"


def test_write_report_default_has_frontmatter_and_footer(tmp_path):
    output = OutputSpec(keep=30)
    started = datetime(2026, 7, 18, 7, 0, tzinfo=TZ)
    finished = started + timedelta(minutes=3, seconds=12)
    path = write_report(
        "mytask",
        1,
        "schedule",
        started,
        finished,
        "success",
        make_result(),
        output,
        default_runs_dir=tmp_path / "runs",
    )
    text = path.read_text()
    assert text.startswith("---\ntask: mytask\n")
    assert "status: success" in text
    assert "# Title" in text
    assert "duration: 3m12s" in text
    assert "iterations: 2" in text


def test_write_report_raw_mode_no_wrapper(tmp_path):
    output = OutputSpec(
        dir=str(tmp_path / "briefing" / "{year}"), filename="day{day}.md", raw=True
    )
    started = datetime(2026, 7, 18, 7, 0, tzinfo=TZ)
    finished = started + timedelta(minutes=1)
    result = RunResult(
        report="# Daily Briefing\n\ncontent", iterations=1, tool_calls=0, model="m"
    )
    path = write_report(
        "daily-briefing", 1, "schedule", started, finished, "success", result, output
    )
    assert path.read_text() == "# Daily Briefing\n\ncontent\n"
    assert path == tmp_path / "briefing" / "2026" / "day18.md"


def test_write_report_creates_missing_dirs_and_overwrites_on_rerun(tmp_path):
    output = OutputSpec(
        dir=str(tmp_path / "briefing" / "{year}"), filename="day{day}.md", raw=True
    )
    started = datetime(2026, 7, 18, 7, 0, tzinfo=TZ)
    first = RunResult(report="first", iterations=1, tool_calls=0, model="m")
    second = RunResult(report="second", iterations=1, tool_calls=0, model="m")
    write_report("t", 1, "manual", started, started, "success", first, output)
    path = write_report("t", 2, "manual", started, started, "success", second, output)
    assert path.read_text() == "second\n"


def test_write_report_prunes_default_dir_only(tmp_path):
    output = OutputSpec(keep=2)
    base = tmp_path / "runs"
    for minute in range(4):
        finished = datetime(2026, 7, 18, 7, minute, tzinfo=TZ)
        write_report(
            "mytask",
            minute,
            "schedule",
            finished,
            finished,
            "success",
            make_result(f"run {minute}"),
            output,
            default_runs_dir=base,
        )
    files = sorted((base / "mytask").glob("*.md"))
    assert len(files) == 2


def test_write_report_custom_dir_never_pruned(tmp_path):
    output = OutputSpec(
        keep=1, dir=str(tmp_path / "briefing"), filename="{iso_datetime}.md"
    )
    for minute in range(4):
        finished = datetime(2026, 7, 18, 7, minute, tzinfo=TZ)
        write_report(
            "t", minute, "schedule", finished, finished, "success", make_result(), output
        )
    files = list((tmp_path / "briefing").glob("*.md"))
    assert len(files) == 4  # never pruned, per DataModel.md §1


def test_prune_reports_keeps_newest_by_mtime(tmp_path):
    d = tmp_path / "x"
    d.mkdir()
    for i in range(5):
        (d / f"{i}.md").write_text("x")
        time.sleep(0.01)
    prune_reports(d, keep=2)
    assert len(list(d.glob("*.md"))) == 2


def test_format_duration():
    assert format_duration(timedelta(seconds=45)) == "45s"
    assert format_duration(timedelta(minutes=3, seconds=12)) == "3m12s"
    assert format_duration(timedelta(hours=1, minutes=2)) == "1h02m"
