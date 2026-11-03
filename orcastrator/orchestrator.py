import asyncio
import logging
import os
import re
from typing import Callable, Optional

from dataclasses import asdict

from .agents import Agent
from .backends import create_backend, is_cloud_model
from .checkpoint import Checkpoint, fingerprint
from .retrieval import MultiRetriever, Source

logger = logging.getLogger("orcastrator")

MAX_REVISIONS = 1

GIB = 2**30

# Fallback research fan-out when model memory can't be measured and the backend
# doesn't declare its own default.
DEFAULT_CONCURRENCY = 4


def _total_ram_bytes() -> Optional[int]:
    try:
        return os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE")
    except (AttributeError, ValueError, OSError):  # non-POSIX or missing key
        return None


def _concurrency_for(model_bytes: Optional[int], total_bytes: Optional[int]) -> int:
    """How many research tasks to run at once, given the model's memory footprint.

    A model that dominates RAM (e.g. a 12B model on a 16 GB machine) is served by
    Ollama with little or no request parallelism, so extra concurrent requests just
    queue server-side and can outlive the client timeout. Scale the fan-out down as
    the model's share of system memory grows.
    """
    if not model_bytes or not total_bytes:
        return DEFAULT_CONCURRENCY
    ratio = model_bytes / total_bytes
    if ratio < 0.25:
        return 4
    if ratio < 0.45:
        return 2
    return 1

# Speed/quality trade-offs. Wall-clock time scales with (questions × calls-per-question ×
# tokens-per-reply); each preset trims one or more of those. CLI and web UI expose these
# by name; the orchestrator itself takes the granular knobs.
PRESETS = {
    # ~4x fewer LLM calls than thorough: fewer questions, no critic pass, short answers.
    "fast": {"max_questions": 4, "use_critic": False, "answer_tokens": 300},
    # Critic stays in the loop, but fewer questions and capped answers.
    "balanced": {"max_questions": 6, "use_critic": True, "answer_tokens": 500},
    # Original behavior: up to 10 questions, critic validation, unbounded answers.
    "thorough": {"max_questions": 10, "use_critic": True, "answer_tokens": None},
    # Deepest pipeline ("extended thinking"): critic review plus a challenge-and-
    # rebuttal round per question, and a draft -> review -> rewrite cycle for the
    # report itself. The debate round doubles per-question calls, so answers are
    # capped at 600 tokens and questions at 8 to keep the whole run inside ~5-10
    # minutes (an uncapped 9-question run measured ~14 min on llama3.2).
    "extended": {"max_questions": 8, "use_critic": True, "answer_tokens": 600, "extended": True},
}

# Rough character budget for the combined research handed to the Writer, chosen to stay
# well inside a small local model's context window (~1 token ≈ 4 chars). Sections beyond
# this are truncated so the final prompt doesn't silently overflow and get dropped.
WRITER_CHAR_BUDGET = 12_000


def _linkify_citations(text: str, references: list[Source]) -> str:
    """Turn plain [n] markers into clickable [[n]](url) citation links."""
    def link(match):
        k = int(match.group(1))
        if 1 <= k <= len(references):
            return f"[[{k}]]({references[k - 1].url})"
        return match.group(0)

    return re.sub(r"\[(\d+)\]", link, text)


def _strip_citations(text: str) -> str:
    """Remove [n] markers (for readers who don't want inline citations)."""
    return re.sub(r"\s*\[\d+\]", "", text)


def _renumber_citations(text: str, offset: int, n_sources: int) -> str:
    """Shift a section's local citations [1..n] into the report-wide numbering."""
    def shift(match):
        k = int(match.group(1))
        return f"[{k + offset}]" if 1 <= k <= n_sources else match.group(0)

    return re.sub(r"\[(\d+)\]", shift, text)


def _sources_block(sources: list[Source]) -> str:
    return "\n\n".join(
        f"[{i}] {s.title} ({s.url}):\n{s.content}" for i, s in enumerate(sources, 1)
    )


def _parse_questions(raw: str) -> list[str]:
    """Turn the Planner's free-text reply into a clean list of questions."""
    questions = []
    for line in raw.splitlines():
        line = line.strip()
        if not line:
            continue
        line = re.sub(r"^[\d]+[.)]\s*", "", line)
        line = re.sub(r"^[-*]\s*", "", line)
        if line:
            questions.append(line)
    return questions


def _budget_sections(sections: list[str], budget: int = WRITER_CHAR_BUDGET) -> str:
    """Join research sections, truncating proportionally if they exceed the char budget.

    Each section keeps at least a small floor so no topic disappears entirely; the join is
    the string actually passed to the Writer.
    """
    combined = "\n\n".join(sections)
    if len(combined) <= budget or not sections:
        return combined

    separators = 2 * (len(sections) - 1)
    per_section = max(200, (budget - separators) // len(sections))
    logger.info(
        "research (%d chars) exceeds writer budget (%d); truncating each section to ~%d chars",
        len(combined), budget, per_section,
    )
    trimmed = []
    for sec in sections:
        if len(sec) > per_section:
            sec = sec[:per_section].rstrip() + " […]"
        trimmed.append(sec)
    return "\n\n".join(trimmed)


class StormOrchestrator:
    def __init__(
        self,
        model: str = "llama3.2",
        host: Optional[str] = None,
        timeout: float = 300.0,
        retries: int = 1,
        on_progress: Optional[Callable[[dict], None]] = None,
        max_questions: int = 10,
        use_critic: bool = True,
        answer_tokens: Optional[int] = None,
        extended: bool = False,
        max_concurrency: Optional[int] = None,
        backend: str = "ollama",
        use_web: bool = True,
        sources: Optional[list[str]] = None,
        inline_citations: bool = True,
        resume: bool = True,
        checkpoint_path: Optional[str] = ".orcastrator_state.json",
    ):
        # Optional hook for UIs (e.g. the web frontend) that want live pipeline
        # status without scraping the logging output.
        self._progress = on_progress or (lambda message: None)
        self.max_questions = max_questions
        self.use_critic = use_critic
        self.extended = extended
        # None = decide at run time from the model's memory footprint.
        self.max_concurrency = max_concurrency
        self.model = model
        # One backend (and connection pool) shared by every agent.
        self.backend = create_backend(backend, host=host, timeout=timeout)
        # Grounded research: fetch verified sources per question and require
        # citations, so every fact in the report is traceable to a live link.
        self.use_web = use_web
        self.retriever = MultiRetriever(enabled=sources) if use_web else None
        # False keeps the References section but removes [n] markers from the body.
        self.inline_citations = inline_citations
        self.answer_tokens = answer_tokens
        # Resumable runs: state is checkpointed after planning, each completed
        # question, and the extended draft; cleared on success. `resume=False`
        # still writes checkpoints but ignores any existing one.
        self.resume = resume
        self._checkpoint = Checkpoint(checkpoint_path) if checkpoint_path else None
        agent_kwargs = {"model": model, "backend": self.backend, "retries": retries}
        self.planner = Agent(
            "Planner",
            f"Break down a topic into at most {max_questions} specific, non-overlapping "
            "research questions (fewer is fine). Reply with exactly one question per line, "
            "plain text, no numbering, no bullets, and no extra commentary before or after "
            "the list.",
            # A question list is short; capping it guards against a rambling planner.
            num_predict=400,
            **agent_kwargs,
        )
        concise = (
            " Be concise and information-dense: a few short paragraphs at most."
            if answer_tokens
            else ""
        )
        self.researcher = Agent(
            "Researcher",
            "Provide detailed, factual information on a given sub-topic. "
            f"Cite specifics where possible.{concise}",
            num_predict=answer_tokens,
            **agent_kwargs,
        )
        source_check = (
            "When numbered sources are provided, verify every claim against them: "
            "treat unsupported claims, missing citations, or citations of nonexistent "
            "source numbers as errors. "
            if use_web
            else ""
        )
        self.critic = Agent(
            "Critic",
            "Review a research answer for hallucinations, missing data, or logical gaps. "
            f"{source_check}"
            "Reply with 'APPROVED: <refined answer>' if it holds up, or "
            "'REVISE: <reason>' if the researcher should try again.",
            # The critic echoes a refined answer, so it needs the same room as the researcher.
            num_predict=answer_tokens,
            **agent_kwargs,
        )
        self.writer = Agent(
            "Writer",
            "Synthesize validated research into a cohesive, structured, "
            "Wikipedia-style article with clear headings.",
            **agent_kwargs,
        )
        # Extended-pipeline agents. The challenger cross-examines each answer (a
        # different job than the approve/revise critic, hence a separate system
        # prompt); the reviewer critiques the whole draft before the rewrite.
        self.challenger = Agent(
            "Critic",
            "Cross-examine a research answer: name its 2-3 weakest claims, missing "
            "perspectives, or likely errors, briefly and specifically. Do not rewrite "
            "the answer yourself.",
            num_predict=answer_tokens,
            **agent_kwargs,
        )
        self.reviewer = Agent(
            "Reviewing Editor",
            "Review a draft report for factual gaps, contradictions, redundancy, and "
            "unclear structure. Reply with a short numbered list of concrete "
            "improvements. Do not rewrite the report yourself.",
            **agent_kwargs,
        )
        self._agents = (
            self.planner, self.researcher, self.critic, self.writer,
            self.challenger, self.reviewer,
        )
        self.state = {"query": "", "questions": [], "results": [], "sources": []}

    async def aclose(self) -> None:
        """Close the shared backend client. Call once after `run()` completes."""
        await self.backend.close()
        if self.retriever:
            await self.retriever.close()

    async def _configure_thinking(self) -> None:
        """Disable hidden chain-of-thought on thinking-capable models.

        Thinking tokens count against num_predict, so a capped agent call can spend
        its entire budget reasoning and return an empty visible reply (observed with
        gemma4:12b: 400 thinking tokens, empty response, done_reason=length). The
        pipeline's agents don't need a reasoning preamble, so turn it off outright.
        Backends that don't expose capabilities return [] and are left alone.
        """
        capabilities = await self.backend.capabilities(self.model)
        if "thinking" in capabilities:
            logger.info("%s is a thinking model; disabling hidden reasoning for agents", self.model)
            self._emit(
                "info",
                f"{self.model} is a thinking model — disabling hidden reasoning so replies aren't empty",
            )
            for agent in self._agents:
                agent.think = False

    async def _pick_concurrency(self) -> tuple[int, str]:
        """Decide the research fan-out from the model's measured memory footprint."""
        if self.max_concurrency:
            return self.max_concurrency, f"concurrency set manually to {self.max_concurrency}"
        if is_cloud_model(self.model):
            return DEFAULT_CONCURRENCY, (
                f"{self.model} runs in Ollama's cloud — no local memory limit; "
                f"running {DEFAULT_CONCURRENCY} research tasks at a time"
            )
        total = _total_ram_bytes()
        model_bytes = await self.backend.loaded_model_bytes(self.model)
        if model_bytes and total:
            concurrency = _concurrency_for(model_bytes, total)
            note = (
                f"{self.model} uses {model_bytes / GIB:.1f} of {total / GIB:.0f} GiB RAM — "
                f"running {concurrency} research task{'s' if concurrency > 1 else ''} at a time"
            )
        else:
            concurrency = getattr(self.backend, "default_concurrency", DEFAULT_CONCURRENCY)
            note = (
                f"{self.backend.name} does not report model memory — "
                f"running {concurrency} research tasks at a time"
            )
        return concurrency, note

    def _emit(self, kind: str, message: str, **fields) -> None:
        """Send a structured progress event to the on_progress callback.

        `kind` drives the UI (stage tracker, per-question rows); `message` is the
        human-readable line for the text log. Extra fields carry structured data
        such as the question index/status. Always JSON-serializable.
        """
        self._progress({"kind": kind, "message": message, **fields})

    async def _research_and_validate(self, index: int, question: str) -> str:
        sources: list[Source] = []
        if self.retriever:
            self._emit("question", f"Searching the web: {question}", index=index, status="searching")
            sources = await self.retriever.search(question)
            if sources:
                logger.info("found %d verified sources: %s", len(sources), question)
                self._emit("info", f"Found {len(sources)} verified sources for: {question}")
            else:
                logger.warning("no sources found: %s", question)
                self._emit(
                    "info",
                    f"No sources found for: {question} — answering from model knowledge (unsourced)",
                )
        # Self-sizing so _research_and_validate also works when called directly.
        source_slots = self.state.setdefault("sources", [])
        while len(source_slots) <= index:
            source_slots.append([])
        source_slots[index] = sources

        # Re-attached to every prompt in the chain (each call is stateless).
        grounding = (
            "\n\nAnswer using ONLY the numbered sources below. Cite the supporting source "
            "after each factual claim, like [1] or [2]. If the sources don't cover part of "
            "the question, say what's missing instead of guessing.\n\nSOURCES:\n"
            + _sources_block(sources)
            if sources
            else ""
        )

        self._emit("question", f"Researching: {question}", index=index, status="researching")
        answer = await self.researcher.generate(f"{question}{grounding}")
        if self.extended:
            # Debate round: the challenger picks the answer apart, then the
            # researcher writes a strengthened rebuttal before the critic verdict.
            logger.info("challenging: %s", question)
            self._emit("question", f"Critic challenging: {question}", index=index, status="challenging")
            challenge = await self.challenger.generate(f"Question: {question}\nAnswer: {answer}")
            logger.info("rebutting: %s", question)
            self._emit("question", f"Researcher rebutting: {question}", index=index, status="rebutting")
            answer = await self.researcher.generate(
                f"{question}\n\nYour answer was:\n{answer}\n\nA critic raised these "
                f"challenges:\n{challenge}\n\nWrite an improved answer that addresses "
                "each challenge — correct anything wrong, fill the gaps, and keep what holds up."
                f"{grounding}"
            )
        if not self.use_critic:
            logger.info("researched (critic disabled): %s", question)
            self._emit("question", f"Researched: {question}", index=index, status="done")
            return answer
        self._emit("question", f"Critic reviewing: {question}", index=index, status="reviewing")
        critic_context = f"\nSOURCES:\n{_sources_block(sources)}" if sources else ""
        for _ in range(MAX_REVISIONS + 1):
            verdict = await self.critic.generate(
                f"Question: {question}{critic_context}\nAnswer: {answer}"
            )
            if verdict.strip().startswith("APPROVED"):
                logger.info("critic approved: %s", question)
                self._emit("question", f"Critic approved: {question}", index=index, status="approved")
                # Expected format is "APPROVED: <refined answer>"; if the critic omits
                # the colon/refined-answer part, fall back to the researcher's answer
                # instead of raising IndexError on a malformed reply.
                _, _, refined = verdict.partition(":")
                return refined.strip() or answer
            logger.info("critic requested revision: %s", question)
            self._emit("question", f"Critic requested a revision: {question}", index=index, status="revising")
            answer = await self.researcher.generate(
                f"{question}\n\nYour previous answer was rejected for this reason: "
                f"{verdict}\nProvide a corrected, more thorough answer.{grounding}"
            )
        logger.warning("answer never approved after %d revision(s): %s", MAX_REVISIONS, question)
        self._emit(
            "question",
            f"Never approved after {MAX_REVISIONS} revision(s), keeping as [unverified]: {question}",
            index=index, status="unverified",
        )
        return f"[unverified] {answer}"

    def _run_fingerprint(self, topic: str) -> str:
        """Everything that shapes a run; a checkpoint only resumes on exact match."""
        return fingerprint(
            topic=topic,
            model=self.model,
            backend=self.backend.name,
            max_questions=self.max_questions,
            use_critic=self.use_critic,
            extended=self.extended,
            answer_tokens=self.answer_tokens,
            use_web=self.use_web,
        )

    def _save_checkpoint(self, run_fp: str, topic: str, results: list, draft: Optional[str] = None) -> None:
        if not self._checkpoint:
            return
        completed = {
            str(i): {
                "answer": results[i],
                "sources": [asdict(s) for s in self.state["sources"][i]],
            }
            for i in range(len(results))
            if results[i] is not None
        }
        self._checkpoint.save({
            "fingerprint": run_fp,
            "topic": topic,
            "questions": self.state["questions"],
            "results": completed,
            "draft": draft,
        })

    async def run(self, topic: str) -> str:
        self.state["query"] = topic

        # Verify the backend server is reachable up front. Jan.ai has no CLI to
        # start its server, so ensure_server() raises a clear error there instead
        # of a confusing connection timeout later; other backends treat it as a
        # no-op.
        server_note = await self.backend.ensure_server()
        if server_note:
            self._emit("info", server_note)

        # Cloud models (name ending in -cloud/:cloud) may need a one-time manifest
        # pull to register with the daemon; raises a clear error if not signed in.
        pull_note = await self.backend.ensure_model(self.model)
        if pull_note:
            self._emit("info", pull_note)

        await self._configure_thinking()

        run_fp = self._run_fingerprint(topic)
        saved = self._checkpoint.load(run_fp) if (self._checkpoint and self.resume) else None

        logger.info("planning: %s", topic)
        # Announce the full stage list up front so the UI can size its tracker;
        # the extended pipeline has five stages instead of three.
        stage_plan = [("planning", "Plan"), ("researching", "Research")]
        if self.extended:
            stage_plan += [("drafting", "Draft"), ("reviewing", "Review"), ("rewriting", "Rewrite")]
        else:
            stage_plan += [("writing", "Write")]
        self._emit(
            "stage",
            f"Breaking '{topic}' into research questions…",
            stage="planning",
            stages=[{"key": key, "label": label} for key, label in stage_plan],
        )

        saved_results = {}
        saved_draft = None

        if saved and saved.get("questions"):
            # Reuse the checkpointed plan — the planner already ran last time.
            questions = saved["questions"]
            saved_results = saved.get("results", {})
            saved_draft = saved.get("draft")
            logger.info("resuming from checkpoint: %d questions restored", len(questions))
        else:
            # Run the planner from scratch
            plan_raw = await self.planner.generate(f"Break down the topic: {topic}")
            questions = _parse_questions(plan_raw)
            if not questions:
                raise RuntimeError("Planner returned no research questions")
            if len(questions) > self.max_questions:
                logger.info(
                    "planner produced %d questions; keeping the first %d",
                    len(questions), self.max_questions,
                )
                questions = questions[: self.max_questions]
        self.state["questions"] = questions
        self.state["sources"] = [[] for _ in questions]
        logger.info("planner produced %d questions", len(self.state["questions"]))

        # The planner call above loaded the model, so its memory footprint is
        # measurable now — size the research fan-out to what the daemon can serve.
        concurrency, memory_note = await self._pick_concurrency()
        logger.info(memory_note)

        logger.info("researching %d questions, %d at a time", len(self.state["questions"]), concurrency)
        critic_note = " (each is answered, then reviewed by the critic)" if self.use_critic else ""
        # The `questions` payload lets the UI lay out one status row per question up front.
        self._emit(
            "stage",
            f"Researching {len(self.state['questions'])} questions{critic_note}…",
            stage="researching",
            questions=self.state["questions"],
            use_critic=self.use_critic,
        )
        self._emit("info", memory_note)

        # Restore checkpointed answers, then research only what's missing.
        results: list[Optional[str]] = [None] * len(questions)
        saved_draft: Optional[str] = None
        if saved:
            saved_draft = saved.get("draft")
            for key, entry in (saved.get("results") or {}).items():
                index = int(key)
                if index >= len(questions):
                    continue
                results[index] = entry["answer"]
                self.state["sources"][index] = [Source(**s) for s in entry.get("sources", [])]
                self._emit(
                    "question",
                    f"Restored from checkpoint: {questions[index]}",
                    index=index, status="restored",
                )
            restored = sum(1 for r in results if r is not None)
            if restored:
                self._emit(
                    "info",
                    f"Resuming previous run — {restored}/{len(questions)} questions "
                    "already researched (delete .orcastrator_state.json to start fresh)",
                )

        self._save_checkpoint(run_fp, topic, results, draft=saved_draft)

        semaphore = asyncio.Semaphore(concurrency)

        async def bounded(index: int, question: str) -> str:
            async with semaphore:
                answer = await self._research_and_validate(index, question)
                results[index] = answer
                # Checkpoint after every finished question, so a crash mid-run
                # only loses the questions still in flight.
                self._save_checkpoint(run_fp, topic, results, draft=saved_draft)
                return answer

        pending = [(i, q) for i, q in enumerate(questions) if results[i] is None]
        await asyncio.gather(*(bounded(i, q) for i, q in pending))
        self.state["results"] = results

        # Merge per-question citations into one report-wide numbering, and build
        # the References list programmatically — the model never invents URLs.
        sections = []
        references: list[Source] = []
        for question, result, sources in zip(
            self.state["questions"], self.state["results"], self.state["sources"]
        ):
            if sources:
                result = _renumber_citations(result, offset=len(references), n_sources=len(sources))
                references.extend(sources)
            elif self.use_web:
                result += "\n\n*(No sources found — this section is unsourced model knowledge.)*"
            sections.append(f"## {question}\n{result}")
        combined = _budget_sections(sections)

        references_md = ""
        if references:
            lines = "\n".join(
                f"- [{i}] [{s.title}]({s.url})" for i, s in enumerate(references, 1)
            )
            references_md = (
                "\n\n## References\n"
                "*All links were fetched and verified while researching this report.*\n\n"
                f"{lines}"
            )

        def finalize(body: str) -> str:
            """Inline-link or strip citation markers, then append References."""
            if references:
                body = (
                    _linkify_citations(body, references)
                    if self.inline_citations
                    else _strip_citations(body)
                )
            return body + references_md

        done_note = "All research validated" if self.use_critic else "Research complete"
        cite_note = (
            " Keep the citation markers like [1] exactly where they support each claim. "
            "Do NOT write your own references, sources, or bibliography section — a "
            "verified References list is appended automatically."
            if references
            else ""
        )
        draft_prompt = (
            f"Write a structured report on '{topic}' using this validated research."
            f"{cite_note}\n\n{combined}"
        )

        def finish(report: str) -> str:
            """A run that produced a report no longer needs its checkpoint."""
            if self._checkpoint:
                self._checkpoint.clear()
            return report

        if not self.extended:
            logger.info("writing final report")
            self._emit("stage", f"{done_note} — writing the final report…", stage="writing")
            return finish(finalize(await self.writer.generate(draft_prompt)))

        if saved_draft:
            logger.info("resuming from checkpointed draft")
            self._emit("info", "Restored the first draft from the checkpoint")
            self._emit("stage", "Draft restored from checkpoint…", stage="drafting")
            draft = saved_draft
        else:
            logger.info("writing first draft")
            self._emit("stage", f"{done_note} — writing the first draft…", stage="drafting")
            draft = await self.writer.generate(draft_prompt)
            self._save_checkpoint(run_fp, topic, results, draft=draft)

        logger.info("reviewing draft")
        self._emit(
            "stage",
            "Draft complete — reviewing editor checking for gaps and contradictions…",
            stage="reviewing",
        )
        feedback = await self.reviewer.generate(
            f"Review this draft report on '{topic}':\n\n{draft}"
        )

        logger.info("rewriting final report")
        self._emit("stage", "Review complete — rewriting the final report…", stage="rewriting")
        final = await self.writer.generate(
            f"Improve this draft report on '{topic}' by applying the reviewer's feedback. "
            f"Keep clear headings and output only the final report.{cite_note}\n\n"
            f"Draft:\n{draft}\n\nReviewer feedback:\n{feedback}"
        )
        return finish(finalize(final))
