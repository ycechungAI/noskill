"""Config dataclass and config.yaml loading. Defaults per DataModel.md §4."""

from __future__ import annotations

import dataclasses
import json
import logging
import re
from dataclasses import dataclass, field
from pathlib import Path

import yaml

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class OllamaConfig:
    host: str = "http://localhost:11434"
    model: str = "qwen3:8b"
    planner_model: str = "qwen3:8b"


@dataclass(frozen=True)
class DaemonConfig:
    tick_seconds: int = 30
    max_concurrent: int = 1
    catchup_grace_minutes: int = 60


@dataclass(frozen=True)
class AgentConfig:
    max_iterations: int = 12
    fetch_max_chars: int = 8000
    search_max_results: int = 5
    think: bool = True  # request extended "thinking" output; auto-disabled per
    # run if the model doesn't support it (see agent/executor.py)


@dataclass(frozen=True)
class SearchConfig:
    provider: str = "ddgs"


@dataclass(frozen=True)
class PathsConfig:
    tasks: str = "tasks"
    runs: str = "runs"
    data: str = "data"


@dataclass(frozen=True)
class MonitorConfig:
    port: int = 8790
    check_interval_seconds: int = 30
    heal_grace_seconds: int = 15
    services: list = field(default_factory=list)  # merged with monitor.default_services


@dataclass(frozen=True)
class Config:
    timezone: str = "local"
    ollama: OllamaConfig = field(default_factory=OllamaConfig)
    daemon: DaemonConfig = field(default_factory=DaemonConfig)
    agent: AgentConfig = field(default_factory=AgentConfig)
    search: SearchConfig = field(default_factory=SearchConfig)
    paths: PathsConfig = field(default_factory=PathsConfig)
    monitor: MonitorConfig = field(default_factory=MonitorConfig)


_TOP_LEVEL_KEYS = {"timezone", "ollama", "daemon", "agent", "search", "paths", "monitor"}


def _sub(cls: type, data: dict, section: str):
    section_data = data.get(section) or {}
    if not isinstance(section_data, dict):
        logger.warning("config.yaml: '%s' must be a mapping, ignoring", section)
        return cls()
    allowed = {f.name for f in dataclasses.fields(cls)}
    for key in section_data:
        if key not in allowed:
            logger.warning("config.yaml: unknown key '%s.%s'", section, key)
    filtered = {k: v for k, v in section_data.items() if k in allowed}
    return cls(**filtered)


def load_config(path: Path = Path("config.yaml")) -> Config:
    """Load config.yaml relative to the project root; missing file -> defaults."""
    if not path.exists():
        return Config()

    data = yaml.safe_load(path.read_text()) or {}
    if not isinstance(data, dict):
        logger.warning("%s: must be a YAML mapping, using defaults", path)
        return Config()

    for key in data:
        if key not in _TOP_LEVEL_KEYS:
            logger.warning("%s: unknown key '%s'", path, key)

    timezone = data.get("timezone", Config.timezone)
    if not isinstance(timezone, str):
        logger.warning("%s: 'timezone' must be a string, using default", path)
        timezone = Config.timezone

    return Config(
        timezone=timezone,
        ollama=_sub(OllamaConfig, data, "ollama"),
        daemon=_sub(DaemonConfig, data, "daemon"),
        agent=_sub(AgentConfig, data, "agent"),
        search=_sub(SearchConfig, data, "search"),
        paths=_sub(PathsConfig, data, "paths"),
        monitor=_sub(MonitorConfig, data, "monitor"),
    )


# Ollama model names look like `qwen3:8b`, `llama3.2`, `hf.co/org/repo:Q4_K_M`.
_MODEL_NAME_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/-]*")


def valid_model_name(name: str) -> bool:
    return bool(_MODEL_NAME_RE.fullmatch(name))


def set_default_model(path: Path, model: str) -> None:
    """Set `ollama.model` in config.yaml, creating the file if needed.

    Edits the one line in place so the rest of the file (other keys,
    comments) is preserved; the result is re-parsed before it's written.
    """
    if not valid_model_name(model):
        raise ValueError(f"invalid model name: {model!r}")
    value_line = f"model: {json.dumps(model)}"
    lines = path.read_text().splitlines() if path.exists() else []

    section = next(
        (i for i, line in enumerate(lines) if re.match(r"ollama:\s*(#.*)?$", line)),
        None,
    )
    if section is None:
        if lines and lines[-1].strip():
            lines.append("")
        lines += ["ollama:", f"  {value_line}"]
    else:
        end = section + 1
        while end < len(lines) and (not lines[end].strip() or lines[end][0] in " \t#"):
            end += 1
        existing = next(
            (i for i in range(section + 1, end) if re.match(r"\s+model:", lines[i])),
            None,
        )
        if existing is not None:
            old = re.match(r"(\s*)model:\s*(\"[^\"]*\"|'[^']*'|[^#\s]*)(\s+#.*)?$", lines[existing])
            indent, comment = (old.group(1), old.group(3) or "") if old else ("  ", "")
            lines[existing] = indent + value_line + comment
        else:
            lines.insert(section + 1, f"  {value_line}")

    text = "\n".join(lines) + "\n"
    data = yaml.safe_load(text) or {}
    if not isinstance(data, dict) or (data.get("ollama") or {}).get("model") != model:
        raise ValueError(f"{path}: could not update ollama.model safely; edit it by hand")
    path.write_text(text)


class ConfigWatcher:
    """Hands out the current Config, re-reading config.yaml when it changes,
    so a long-running daemon or web UI picks up edits (e.g. a model switched
    from the web UI) without a restart."""

    def __init__(self, path: Path, cfg: Config | None = None):
        self.path = path
        self._mtime = self._stat()
        self._cfg = cfg if cfg is not None else load_config(path)

    def _stat(self) -> float | None:
        try:
            return self.path.stat().st_mtime
        except OSError:
            return None

    def current(self) -> Config:
        mtime = self._stat()
        if mtime != self._mtime:
            self._mtime = mtime
            try:
                self._cfg = load_config(self.path)
                logger.info("config.yaml changed; reloaded (model=%s)", self._cfg.ollama.model)
            except Exception as e:  # noqa: BLE001 - keep running on the last good config
                logger.error("config.yaml changed but could not be loaded, keeping previous: %s", e)
        return self._cfg
