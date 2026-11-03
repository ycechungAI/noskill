"""Local web UI for the STORM orchestrator.

Run with `orcastrator-web` (or `python -m orcastrator.web`) and open
http://127.0.0.1:8400 in a browser. Two modes:

- Research (default): submits a topic and streams pipeline progress plus the
  final report over Server-Sent Events, so the user sees what the agents are
  doing instead of a silent multi-minute wait.
- Chat: a plain chatbot against the selected Ollama model (`/api/chat`),
  streaming tokens as they generate.
"""

import argparse
import asyncio
import json
import logging
from importlib import resources
from typing import Optional

import httpx
from fastapi import FastAPI
from fastapi.responses import HTMLResponse, StreamingResponse
from pydantic import BaseModel

from .backends import OLLAMA_CLOUD_MODELS, create_backend
from .orchestrator import PRESETS, StormOrchestrator, _linkify_citations, _sources_block
from .retrieval import MultiRetriever

logger = logging.getLogger("orcastrator.web")

app = FastAPI(title="Orcastrator")

DEFAULT_PORT = 8400


def _parse_sources(raw: str) -> Optional[list[str]]:
    """Comma-separated engine list from the query string; '' -> None (all)."""
    if not raw:
        return None
    return [s.strip() for s in raw.split(",") if s.strip()]


async def _safe_close(coro, what: str) -> None:
    """Await a cleanup coroutine without letting it kill an in-flight response.

    A `finally` block that awaits close()/aclose() directly lets any exception
    there (a flaky client teardown, a half-broken connection) replace whatever
    the handler already produced — for a StreamingResponse that can mean a
    perfectly good reply gets thrown away and the client sees a bare
    connection failure ("Failed to fetch") instead. Cleanup failures are
    logged, never raised. Pass the awaitable itself, e.g. `_safe_close(be.close(), "backend")`.
    """
    try:
        await asyncio.shield(coro)
    except Exception:
        logger.warning("error closing %s (ignored, response already sent)", what, exc_info=True)


@app.get("/", response_class=HTMLResponse)
async def index() -> str:
    return resources.files("orcastrator").joinpath("static/index.html").read_text()


@app.get("/api/models")
async def models(host: Optional[str] = None, backend: str = "ollama") -> dict:
    """List models available on the selected server, so the UI can offer a picker.

    Returns {"models": [...], "error": null} — a failure is reported rather than
    raised so the page still loads when the server is down.
    """
    be = create_backend(backend, host=host, timeout=5.0)
    try:
        installed = await be.list_models()
        # Ollama can also serve cloud-hosted models (after `ollama signin`);
        # offer the known ones that aren't already registered locally.
        cloud = [m for m in OLLAMA_CLOUD_MODELS if m not in installed] if backend == "ollama" else []
        return {"models": installed, "cloud_models": cloud, "error": None}
    except (httpx.HTTPError, KeyError, ValueError) as exc:
        logger.warning("could not list %s models from %s: %s", backend, be.host, exc)
        return {"models": [], "cloud_models": [], "error": f"Could not reach {backend} at {be.host}: {exc}"}
    finally:
        await be.close()


class ChatRequest(BaseModel):
    """One chat turn: full history in, streamed assistant reply out."""

    messages: list[dict]  # [{"role": "user"|"assistant", "content": "..."}]
    model: str = "llama3.2"
    host: Optional[str] = None
    timeout: float = 300.0
    backend: str = "ollama"
    use_web: bool = False
    sources: str = ""


@app.post("/api/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    """Plain chatbot mode: stream the model's reply as text.

    The client keeps the conversation history and sends it whole each turn;
    the server is stateless. Errors mid-stream are appended as a readable
    line since the HTTP status is already committed by then.

    When `use_web` is set, the latest user message is used as a search query;
    any sources found are handed to the model with instructions to cite them,
    and a verified References list is appended once generation finishes. This
    requires buffering the full reply before sending it (citations are only
    correct once we can see everything the model wrote), so grounded replies
    arrive in one piece rather than streaming token-by-token like ungrounded
    chat does.
    """
    be = create_backend(req.backend, host=req.host or None, timeout=req.timeout)

    async def tokens():
        retriever = None
        try:
            if req.use_web and req.messages and req.messages[-1].get("role") == "user":
                query = req.messages[-1]["content"]
                retriever = MultiRetriever(enabled=_parse_sources(req.sources))
                sources = await retriever.search(query)
                if sources:
                    grounded = list(req.messages)
                    grounded.insert(-1, {
                        "role": "system",
                        "content": (
                            "Verified web sources for the user's latest message follow. "
                            "If they're relevant, cite each supporting claim like [1] or "
                            "[2]. Use your own knowledge for anything the sources don't "
                            "cover; don't write your own reference list — one is appended "
                            "automatically.\n\nSOURCES:\n" + _sources_block(sources)
                        ),
                    })
                    full = "".join([p async for p in be.chat_stream(req.model, grounded)])
                    full = _linkify_citations(full, sources)
                    refs = "\n".join(f"- [{i}] [{s.title}]({s.url})" for i, s in enumerate(sources, 1))
                    yield full + "\n\n**Sources:**\n" + refs
                    return
                # No sources found: fall through to a normal, live-streamed reply.
            async for piece in be.chat_stream(req.model, req.messages):
                yield piece
        except Exception as exc:
            logger.exception("chat request failed")
            yield f"\n\n[error: {exc}]"
        finally:
            await _safe_close(be.close(), "chat backend")
            if retriever:
                await _safe_close(retriever.close(), "chat retriever")

    return StreamingResponse(tokens(), media_type="text/plain; charset=utf-8")


@app.get("/api/research")
async def research(
    topic: str,
    model: str = "llama3.2",
    host: Optional[str] = None,
    timeout: float = 300.0,
    retries: int = 1,
    preset: str = "fast",
    backend: str = "ollama",
    use_web: bool = True,
    citations: bool = True,
    sources: str = "",
    resume: bool = True,
) -> StreamingResponse:
    """Run the pipeline, streaming `progress` events and then a final `report`.

    Uses GET + SSE (rather than POST) because the browser's EventSource API
    only supports GET; the server binds to localhost by default.
    """
    queue: asyncio.Queue = asyncio.Queue()

    orchestrator = StormOrchestrator(
        model=model,
        host=host or None,
        timeout=timeout,
        retries=retries,
        backend=backend,
        use_web=use_web,
        sources=_parse_sources(sources),
        inline_citations=citations,
        resume=resume,
        on_progress=lambda message: queue.put_nowait(("progress", message)),
        **PRESETS.get(preset, PRESETS["fast"]),
    )

    async def pipeline() -> None:
        try:
            report = await orchestrator.run(topic)
            await queue.put(("report", report))
        except Exception as exc:  # surfaced to the page as an `error` event
            logger.exception("pipeline failed for topic %r", topic)
            await queue.put(("error", str(exc)))
        finally:
            await _safe_close(orchestrator.aclose(), "orchestrator")
            await queue.put(("close", ""))

    task = asyncio.create_task(pipeline())

    async def events():
        try:
            while True:
                kind, payload = await queue.get()
                if kind == "close":
                    break
                yield f"event: {kind}\ndata: {json.dumps(payload)}\n\n"
        finally:
            task.cancel()

    return StreamingResponse(events(), media_type="text/event-stream")


def main() -> None:
    import uvicorn

    parser = argparse.ArgumentParser(description="Web UI for the STORM research orchestrator")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help=f"Port to serve on (default: {DEFAULT_PORT})")
    parser.add_argument(
        "--bind",
        default="127.0.0.1",
        help="Address to bind (default: 127.0.0.1; use 0.0.0.0 to expose on your network)",
    )
    args = parser.parse_args()

    print(f"Orcastrator web UI: http://{args.bind}:{args.port}")
    uvicorn.run(app, host=args.bind, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
