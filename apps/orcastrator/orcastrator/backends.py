"""LLM server backends.

The orchestrator, CLI, and web UI talk to a Backend rather than to Ollama
directly, so the same pipeline runs against:

- Ollama (native API, default host http://localhost:11434)
- LM Studio (OpenAI-compatible API, default host http://localhost:1234)
- Jan.ai (OpenAI-compatible API, default host http://localhost:1337)

A backend knows how to generate text, stream a chat reply, list models, and
report what the pipeline needs for tuning (thinking capability, loaded-model
memory) — returning empty/None where a server simply doesn't expose that.
"""

import json
import logging
import os
import re
from typing import AsyncIterator, Optional

import httpx
import ollama

logger = logging.getLogger("orcastrator.backends")

_THINK_TAGS = re.compile(r"(?s)<think(?:ing)?>.*?</think(?:ing)?>\s*")

# Ollama cloud models run on ollama.com but are served through the local daemon
# (after `ollama signin`); they're addressed by a -cloud/:cloud tag. Curated list
# for the UI picker — any other cloud tag typed manually works the same way.
OLLAMA_CLOUD_MODELS = [
    "gpt-oss:20b-cloud",
    "gpt-oss:120b-cloud",
    "deepseek-v3.1:671b-cloud",
    "qwen3-coder:480b-cloud",
    "glm-4.6:cloud",
    "minimax-m2:cloud",
    "kimi-k2:1t-cloud",
]


def is_cloud_model(model: str) -> bool:
    return model.endswith("-cloud") or model.endswith(":cloud")


def _strip_think(text: str) -> str:
    """Drop inline <think>…</think> blocks that reasoning models emit over
    OpenAI-compatible APIs (which have no server-side switch to disable them)."""
    if not text:
        return ""
    return _THINK_TAGS.sub("", text).strip()


def _openai_error_detail(host: str, model: Optional[str], status_code: int, raw: bytes) -> str:
    """Turn an OpenAI-compatible error body into a readable message.

    Without this, a 4xx surfaces as httpx's generic
    "Client error '400 Bad Request' for url '...'" — which says nothing about
    *why*. Servers like LM Studio put the real reason in `error.message` (e.g.
    "Invalid model identifier ... specify a valid downloaded model"), which is
    almost always the actionable part (wrong/unloaded model, bad request shape).
    """
    message = None
    try:
        payload = json.loads(raw or b"{}")
        err = payload.get("error")
        if isinstance(err, dict):
            message = err.get("message")
        elif isinstance(err, str):
            message = err
        message = message or payload.get("message")
    except ValueError:
        pass
    detail = message or (raw.decode(errors="replace")[:300] if raw else f"HTTP {status_code}")
    return f"{host} rejected the request for model {model!r} ({status_code}): {detail}"


class _OpenAICompatibleBackend:
    """Shared implementation for local OpenAI-compatible chat servers.

    LM Studio and Jan.ai both expose the same `/v1/chat/completions` /
    `/v1/models` shape, so their generate/chat_stream/list_models/close bodies
    are identical; only the default host and how (or whether) the server can
    be auto-verified/started differ, which subclasses override.
    """

    default_concurrency = 2  # neither server reports memory; keep the fan-out modest

    def __init__(self, host: Optional[str] = None, timeout: float = 300.0):
        self.host = (host or self._default_host()).rstrip("/")
        self._client = httpx.AsyncClient(timeout=timeout)

    def _default_host(self) -> str:
        raise NotImplementedError

    async def _completion(self, payload: dict) -> str:
        response = await self._client.post(self.host + "/v1/chat/completions", json=payload)
        if response.status_code >= 400:
            raise RuntimeError(
                _openai_error_detail(self.host, payload.get("model"), response.status_code, response.content)
            )
        # Some OpenAI-compatible servers (e.g. Jan.ai / LM Studio when the model
        # isn't loaded) return content: null rather than a 4xx. Treat that as an
        # empty reply instead of crashing on _strip_think(None).
        content = response.json()["choices"][0]["message"].get("content")
        if content is None:
            logger.warning(
                "%s returned a null completion (model may not be loaded); treating as empty",
                self.host,
            )
            content = ""
        return _strip_think(content)

    async def generate(
        self,
        model: str,
        system: str,
        prompt: str,
        num_predict: Optional[int] = None,
        think: Optional[bool] = None,  # no OpenAI-API switch; <think> blocks are stripped
    ) -> str:
        payload = {
            "model": model,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": prompt},
            ],
            "stream": False,
        }
        if num_predict:
            payload["max_tokens"] = num_predict
        content = await self._completion(payload)
        if not content and num_predict:
            # A reasoning model can spend the whole max_tokens budget inside its
            # <think> block (same failure mode as Ollama thinking models, but the
            # OpenAI API has no off switch) — retry once without the cap.
            logger.warning(
                "%s returned an empty reply with max_tokens=%d (reasoning model?); retrying uncapped",
                model, num_predict,
            )
            payload.pop("max_tokens")
            content = await self._completion(payload)
        return content

    async def chat_stream(self, model: str, messages: list[dict]) -> AsyncIterator[str]:
        payload = {"model": model, "messages": messages, "stream": True}
        async with self._client.stream(
            "POST", self.host + "/v1/chat/completions", json=payload
        ) as response:
            if response.status_code >= 400:
                body = await response.aread()
                raise RuntimeError(_openai_error_detail(self.host, model, response.status_code, body))
            async for line in response.aiter_lines():
                if not line.startswith("data:"):
                    continue
                data = line[len("data:"):].strip()
                if data == "[DONE]":
                    break
                choices = json.loads(data).get("choices") or []
                piece = choices[0].get("delta", {}).get("content") if choices else None
                if piece:
                    yield piece

    async def list_models(self) -> list[str]:
        response = await self._client.get(self.host + "/v1/models")
        response.raise_for_status()
        return [m["id"] for m in response.json().get("data", [])]

    async def ensure_model(self, model: str) -> Optional[str]:
        return None  # these servers load models themselves (JIT or via their UI)

    async def capabilities(self, model: str) -> list[str]:
        return []  # not exposed by the OpenAI-compatible API

    async def loaded_model_bytes(self, model: str) -> Optional[int]:
        return None  # not exposed by the OpenAI-compatible API

    async def close(self) -> None:
        await self._client.aclose()


class OllamaBackend:
    """Ollama's native API via the official client."""

    name = "ollama"
    # Matches Ollama's own request parallelism when memory allows.
    default_concurrency = 4

    def __init__(self, host: Optional[str] = None, timeout: float = 300.0):
        self.host = (host or os.environ.get("OLLAMA_HOST") or "http://localhost:11434").rstrip("/")
        self._client = ollama.AsyncClient(host=host, timeout=timeout)
        # Separate client for the daemon's management endpoints (tags/show/ps/pull).
        self._http = httpx.AsyncClient(timeout=10.0)

    async def generate(
        self,
        model: str,
        system: str,
        prompt: str,
        num_predict: Optional[int] = None,
        think: Optional[bool] = None,
    ) -> str:
        extra = {}
        if num_predict:
            extra["options"] = {"num_predict": num_predict}
        if think is not None:
            extra["think"] = think
        response = await self._client.generate(model=model, system=system, prompt=prompt, **extra)
        return response["response"]

    async def chat_stream(self, model: str, messages: list[dict]) -> AsyncIterator[str]:
        stream = await self._client.chat(model=model, messages=messages, stream=True)
        async for chunk in stream:
            yield chunk["message"]["content"]

    async def list_models(self) -> list[str]:
        response = await self._http.get(self.host + "/api/tags")
        response.raise_for_status()
        return [m["name"] for m in response.json().get("models", [])]

    async def ensure_server(self) -> Optional[str]:
        return None  # the Ollama daemon is started by the user (`ollama serve`)

    async def ensure_model(self, model: str) -> Optional[str]:
        """Register a cloud model with the daemon if it isn't already.

        Cloud models only need a tiny manifest pull (the weights stay on
        ollama.com), but the daemon must be signed in. Returns a note for the
        UI when a pull happened, None otherwise.
        """
        if not is_cloud_model(model):
            return None
        try:
            if model in await self.list_models():
                return None
        except (httpx.HTTPError, ValueError):
            return None  # daemon unreachable; the generate call will surface it
        logger.info("registering cloud model %s with the daemon", model)
        response = await self._http.post(
            self.host + "/api/pull",
            json={"model": model, "stream": False},
            timeout=120.0,
        )
        if response.status_code != 200:
            raise RuntimeError(
                f"could not register cloud model {model}: {response.text[:200]} "
                "— cloud models need `ollama signin` first"
            )
        return f"Registered cloud model {model} with the local daemon"

    async def capabilities(self, model: str) -> list[str]:
        try:
            response = await self._http.post(self.host + "/api/show", json={"model": model})
            response.raise_for_status()
            return response.json().get("capabilities") or []
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("could not read model capabilities (%s)", exc)
            return []

    async def loaded_model_bytes(self, model: str) -> Optional[int]:
        try:
            response = await self._http.get(self.host + "/api/ps")
            response.raise_for_status()
            for m in response.json().get("models", []):
                # Match bare names against full tags (llama3.2 vs llama3.2:latest).
                if m.get("name", "").split(":")[0] == model.split(":")[0]:
                    return m.get("size")
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("could not measure model memory (%s)", exc)
        return None

    async def close(self) -> None:
        await self._client.close()
        await self._http.aclose()


class LMStudioBackend(_OpenAICompatibleBackend):
    """LM Studio's OpenAI-compatible API (plain httpx, no extra dependency)."""

    name = "lmstudio"

    def _default_host(self) -> str:
        return os.environ.get("LMSTUDIO_HOST") or "http://localhost:1234"

    async def ensure_server(self) -> Optional[str]:
        return None  # LM Studio loads models itself (JIT or via its UI)

    async def list_models(self) -> list[str]:
        """Chat-capable models only.

        LM Studio's `/api/v0/models` reports a `type` per model (llm vs
        embeddings); the plain OpenAI-compatible `/v1/models` doesn't, and
        offering an embedding model in the picker means the very next chat
        request 400s with a confusing "invalid model" error. Falls back to
        `/v1/models` if the LM-Studio-specific endpoint isn't available (e.g.
        an older version, or another server pointed at via --host).
        """
        try:
            response = await self._client.get(self.host + "/api/v0/models")
            response.raise_for_status()
            models = response.json().get("data") or []
            return [m["id"] for m in models if m.get("type") != "embeddings"]
        except (httpx.HTTPError, ValueError, KeyError) as exc:
            logger.info("falling back to /v1/models (no /api/v0/models: %s)", exc)
            return await super().list_models()


class JanBackend(_OpenAICompatibleBackend):
    """Jan.ai's local OpenAI-compatible API (no auth required).

    Jan ships an OpenAI-compatible endpoint at http://localhost:1337 (or
    $JAN_HOST) once you enable "Local API Server" in its settings.
    """

    name = "jan"

    def _default_host(self) -> str:
        return os.environ.get("JAN_HOST") or "http://localhost:1337"

    async def ensure_server(self) -> Optional[str]:
        """Probe Jan.ai's API endpoint and raise a clear message if down.

        Jan has no CLI to start its server, so rather than failing later with a
        confusing connection timeout we verify reachability up front and point
        the user at the setting that turns the server on. Returns None when up.
        """
        try:
            response = await self._client.get(self.host + "/v1/models")
            response.raise_for_status()
        except httpx.HTTPError as exc:
            raise RuntimeError(
                f"Jan.ai isn't reachable at {self.host}. Open Jan, go to "
                "Settings → Local API Server, and enable it (no CLI to start "
                f"the server). Underlying error: {exc}"
            ) from exc
        return None


BACKENDS = {b.name: b for b in (OllamaBackend, LMStudioBackend, JanBackend)}


def create_backend(name: str, host: Optional[str] = None, timeout: float = 300.0):
    try:
        cls = BACKENDS[name]
    except KeyError:
        raise ValueError(f"unknown backend {name!r}; expected one of {sorted(BACKENDS)}")
    return cls(host=host, timeout=timeout)
