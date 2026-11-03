"""Run report writing, templated destinations, and pruning. See DataModel.md §1, §3."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta
from pathlib import Path

from schedlm.tasks import OutputSpec

DEFAULT_FILENAME_TEMPLATE = "{iso_datetime}.md"


@dataclass
class RunResult:
    report: str
    iterations: int
    tool_calls: int
    model: str
    # Deterministic verification verdict (agent/verify.py): success / partial
    # / failed. RunResults built by hand (tests) default to success.
    status: str = "success"
    check_failures: list[str] = field(default_factory=list)


def _template_vars(when: datetime) -> dict:
    return {
        "year": when.year,
        "month02": f"{when.month:02d}",
        "month_name": when.strftime("%B"),
        "day": when.day,
        "day02": f"{when.day:02d}",
        "iso_date": when.strftime("%Y-%m-%d"),
        "iso_datetime": when.strftime("%Y-%m-%dT%H-%M"),
    }


def resolve_output_path(
    output: OutputSpec,
    task_id: str,
    when: datetime,
    default_runs_dir: Path = Path("runs"),
) -> Path:
    """Expand template vars ({year}, {month_name}, {day}, ...) and ~ in output.dir/filename."""
    vars_ = _template_vars(when)
    if output.dir:
        dir_path = Path(output.dir.format(**vars_)).expanduser()
    else:
        dir_path = default_runs_dir / task_id
    filename = (output.filename or DEFAULT_FILENAME_TEMPLATE).format(**vars_)
    return dir_path / filename


def format_duration(delta: timedelta) -> str:
    total = int(delta.total_seconds())
    hours, rem = divmod(total, 3600)
    minutes, seconds = divmod(rem, 60)
    if hours:
        return f"{hours}h{minutes:02d}m"
    if minutes:
        return f"{minutes}m{seconds:02d}s"
    return f"{seconds}s"


def write_report(
    task_id: str,
    run_id: int,
    trigger: str,
    started: datetime,
    finished: datetime,
    status: str,
    result: RunResult,
    output: OutputSpec,
    default_runs_dir: Path = Path("runs"),
) -> Path:
    path = resolve_output_path(output, task_id, finished, default_runs_dir)
    path.parent.mkdir(parents=True, exist_ok=True)

    if output.raw:
        text = result.report.rstrip() + "\n"
    else:
        duration_str = format_duration(finished - started)
        checks = ""
        if result.check_failures:
            checks = (
                "checks_failed: "
                + " | ".join(result.check_failures)
                + "\n"
            )
        frontmatter = (
            "---\n"
            f"task: {task_id}\n"
            f"run_id: {run_id}\n"
            f"trigger: {trigger}\n"
            f"started: {started.isoformat()}\n"
            f"status: {status}\n"
            f"{checks}"
            "---\n\n"
        )
        footer = (
            "\n\n---\n"
            f"_model: {result.model} · duration: {duration_str} · "
            f"iterations: {result.iterations} · tool calls: {result.tool_calls}_\n"
        )
        text = frontmatter + result.report.rstrip() + footer

    path.write_text(text)
    if not output.dir:
        prune_reports(path.parent, output.keep)
    return path


def prune_reports(dir_path: Path, keep: int) -> None:
    if not dir_path.exists():
        return
    files = sorted(dir_path.glob("*.md"), key=lambda p: p.stat().st_mtime, reverse=True)
    for stale in files[keep:]:
        stale.unlink()
