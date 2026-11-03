import argparse
import asyncio
import logging
import sys
from typing import Optional

import httpx
import ollama

from .export import export_report
from .orchestrator import PRESETS, StormOrchestrator
from .retrieval import ALL_ENGINES


async def _main(
    topic: str,
    model: str,
    host: Optional[str],
    output: Optional[str],
    timeout: float,
    retries: int,
    preset: str,
    max_concurrency: Optional[int],
    backend: str,
    use_web: bool,
    sources: Optional[list[str]],
    inline_citations: bool,
    resume: bool,
) -> None:
    orchestrator = StormOrchestrator(
        model=model, host=host, timeout=timeout, retries=retries,
        max_concurrency=max_concurrency, backend=backend, use_web=use_web,
        sources=sources, inline_citations=inline_citations, resume=resume,
        **PRESETS[preset],
    )
    try:
        report = await orchestrator.run(topic)
    finally:
        await orchestrator.aclose()
    if output:
        print(export_report(report, output, title=topic), file=sys.stderr)
    else:
        print(report)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="STORM-style research report generator over Ollama, LM Studio, or Jan.ai"
    )
    parser.add_argument("topic", help="Research topic to report on")
    parser.add_argument(
        "--model",
        default="llama3.2",
        help="Model to use (default: llama3.2). Ollama cloud models work too, "
        "e.g. gpt-oss:120b-cloud (requires `ollama signin`)",
    )
    parser.add_argument(
        "--no-web",
        action="store_true",
        help="Skip web retrieval: no sources, citations, or reference links "
        "(answers come from model knowledge alone)",
    )
    parser.add_argument(
        "--no-citations",
        action="store_true",
        help="Remove inline [n] citation markers from the report body "
        "(the References section is still appended)",
    )
    parser.add_argument(
        "--sources",
        default=None,
        help="Comma-separated research engines to use (default: all). "
        f"Available: {', '.join(ALL_ENGINES)}. "
        "Example: --sources wikipedia,ddg,arxiv",
    )
    parser.add_argument(
        "--backend",
        choices=("ollama", "lmstudio", "jan"),
        default="ollama",
        help="Local LLM server to use (default: ollama)",
    )
    parser.add_argument(
        "--host",
        default=None,
        help="Server URL (default: $OLLAMA_HOST / http://localhost:11434 for ollama, "
        "$LMSTUDIO_HOST / http://localhost:1234 for lmstudio, "
        "$JAN_HOST / http://localhost:1337 for jan)",
    )
    parser.add_argument(
        "-o", "--output",
        help="Write the report to this file instead of stdout. The extension picks "
        "the format: .md (as-is), .html (built-in renderer, or pandoc when "
        "installed), .pdf (requires pandoc + a PDF engine)",
    )
    parser.add_argument(
        "--no-resume",
        action="store_true",
        help="Ignore any existing checkpoint and start the run from scratch "
        "(runs are checkpointed to .orcastrator_state.json after every step)",
    )
    parser.add_argument(
        "--preset",
        choices=sorted(PRESETS),
        default="balanced",
        help="Speed/quality trade-off: fast = 4 questions, no critic pass, short answers; "
        "balanced = 6 questions with critic review (default); "
        "thorough = up to 10 questions, critic review, unbounded answers; "
        "extended = thorough plus challenge/rebuttal per question and a "
        "draft-review-rewrite cycle",
    )
    parser.add_argument(
        "--timeout", type=float, default=300.0, help="Per-request timeout in seconds (default: 300)"
    )
    parser.add_argument(
        "--retries", type=int, default=1, help="Retries per request on connection/timeout errors (default: 1)"
    )
    parser.add_argument(
        "--max-concurrency",
        type=int,
        default=None,
        help="Research tasks to run at once (default: auto — sized from the model's "
        "memory footprint vs system RAM, so big models run one at a time)",
    )
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="Log pipeline progress to stderr"
    )
    args = parser.parse_args()

    # Parse and validate --sources (None = all engines).
    sources = None
    if args.sources:
        sources = [s.strip() for s in args.sources.split(",") if s.strip()]
        unknown = [s for s in sources if s not in ALL_ENGINES]
        if unknown:
            parser.error(
                f"unknown --sources: {', '.join(unknown)} "
                f"(available: {', '.join(ALL_ENGINES)})"
            )

    logging.basicConfig(
        level=logging.INFO if args.verbose else logging.WARNING,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        stream=sys.stderr,
    )
    # Keep --verbose focused on pipeline progress, not every underlying HTTP request.
    logging.getLogger("httpx").setLevel(logging.WARNING)

    try:
        asyncio.run(
            _main(
                args.topic, args.model, args.host, args.output,
                args.timeout, args.retries, args.preset, args.max_concurrency,
                args.backend, not args.no_web, sources, not args.no_citations,
                not args.no_resume,
            )
        )
    except (RuntimeError, ConnectionError, ollama.ResponseError, httpx.TransportError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        if isinstance(exc, (ConnectionError, httpx.ConnectError)):
            print("hint: is the Ollama daemon running? Start it with: ollama serve", file=sys.stderr)
        elif isinstance(exc, ollama.ResponseError) and "not found" in str(exc):
            print(f"hint: pull the model first with: ollama pull {args.model}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("\ninterrupted", file=sys.stderr)
        sys.exit(130)


if __name__ == "__main__":
    main()
