"""Which Ollama models are installed, and picking a usable stand-in when the
configured one isn't (Architecture.md §6 "Model not pulled")."""

from __future__ import annotations

import logging
from dataclasses import dataclass

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class ModelInfo:
    name: str
    size: int
    capabilities: tuple[str, ...] | None  # None = not looked up / lookup failed

    @property
    def tools(self) -> bool | None:
        return None if self.capabilities is None else "tools" in self.capabilities

    @property
    def chat(self) -> bool | None:
        return None if self.capabilities is None else can_chat(self.capabilities)


def can_chat(capabilities) -> bool:
    """Embedding (`nomic-embed-text`) and speech (`phonon`) models are
    pulled like any other but reject /api/chat."""
    return "completion" in capabilities


def normalize(name: str) -> str:
    """`llama3.2` and `llama3.2:latest` are the same model to Ollama."""
    return name if ":" in name else f"{name}:latest"


def _capabilities(client, name: str) -> tuple[str, ...] | None:
    try:
        return tuple(client.show(name).capabilities or ())
    except Exception:  # noqa: BLE001 - capability info is best-effort
        return None


def list_installed(client, with_capabilities: bool = False) -> list[ModelInfo]:
    """Installed models, largest first. Raises if Ollama can't be reached."""
    models = [
        ModelInfo(
            name=m.model,
            size=m.size or 0,
            capabilities=_capabilities(client, m.model) if with_capabilities else None,
        )
        for m in client.list().models
    ]
    return sorted(models, key=lambda m: m.size, reverse=True)


def is_installed(name: str, installed: list[ModelInfo]) -> bool:
    return normalize(name) in {normalize(m.name) for m in installed}


def resolve_model(client, wanted: str, needs_tools: bool) -> tuple[str, str | None]:
    """Return (model to use, note). The note is None when `wanted` is
    usable as-is; otherwise it explains the substitution.

    If `wanted` isn't installed -- or can't chat at all (an embedding or
    speech model), or can't call tools and the task needs them, which Ollama
    would reject outright -- falls back to the largest
    installed model that can chat (and call tools, if `needs_tools`), so a
    deleted or unsuitable model doesn't fail every run until someone
    notices. If Ollama can't be asked, or nothing suitable is installed,
    returns `wanted` unchanged and lets the real chat call report it.
    """
    try:
        installed = list_installed(client)
    except Exception:  # noqa: BLE001
        return wanted, None
    if is_installed(wanted, installed):
        caps = _capabilities(client, wanted)
        # Unknown capabilities (older Ollama) -> trust the user's choice.
        if not caps or (can_chat(caps) and (not needs_tools or "tools" in caps)):
            return wanted, None
        if not can_chat(caps):
            problem = f"model '{wanted}' can't chat ({', '.join(caps)} model)"
        else:
            problem = f"model '{wanted}' can't call tools, which this task needs"
        fix = "pick another model"
    else:
        problem = f"model '{wanted}' is not installed"
        fix = f"run `ollama pull {wanted}` to restore it, or pick another model"
    for m in installed:
        caps = _capabilities(client, m.name) or ()
        if not can_chat(caps) or (needs_tools and "tools" not in caps):
            continue
        return m.name, f"{problem}; using '{m.name}' instead ({fix})"
    return wanted, None
