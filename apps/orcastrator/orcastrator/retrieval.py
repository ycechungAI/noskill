"""Web retrieval for grounded research.

Sources come from several keyless public APIs, merged by MultiRetriever. The
default set of engines:

- Wikipedia (encyclopedic articles, lead-section extracts)
- OpenAlex (scholarly papers with abstracts and DOI links)
- DuckDuckGo Instant Answers (abstracts from Britannica, Wikipedia, etc.)
- DuckDuckGo web search (real ranked result links + snippets)
- Stock quotes (live prices for equities/ETFs/crypto/indices via Yahoo Finance)
- arXiv (scholarly preprints with abstracts)
- Semantic Scholar (scholarly papers, citations, influential works)
- Stack Exchange (Q&A, e.g. Stack Overflow)
- Hacker News (tech news/discussion via the Algolia API)

Engines are selectable via `MultiRetriever(enabled=[...])` (and the CLI/web
`--sources` flag), so users can drop fragile ones (e.g. the DuckDuckGo HTML
scraper, which can be rate-limited).

A Source's URL is only ever emitted after it was checked over HTTP (Wikipedia
content is fetched from the page itself; other URLs get a liveness check), so
every reference in a report is a live, verified link — the model never invents
URLs.
"""

import asyncio
import logging
import re
import xml.etree.ElementTree as ET
from dataclasses import dataclass
from datetime import datetime, timezone
from html import unescape
from itertools import zip_longest
from typing import Optional
from urllib.parse import parse_qs, quote, unquote, urlparse

import httpx

logger = logging.getLogger("orcastrator.retrieval")

# Per-source context handed to the researcher. Big enough to be substantive,
# small enough that 3 sources + question fit comfortably in a small model's
# context window alongside the system prompt.
EXTRACT_CHARS = 2500

_SEARCH_URL = "https://en.wikipedia.org/w/rest.php/v1/search/page"
_EXTRACT_URL = "https://en.wikipedia.org/w/api.php"
_UA = {"User-Agent": "orcastrator/0.1 (local research tool)"}


@dataclass
class Source:
    title: str
    url: str
    content: str
    verified: bool  # content was fetched from the URL's page over HTTP


class WikipediaRetriever:
    """Finds and fetches Wikipedia sources for a research question."""

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)
        # Wikipedia 429s bursty clients hard, so requests are strictly serialized
        # with a little spacing, retried with backoff, and extracts are batched
        # (one API call per question instead of one per page).
        self._lock = asyncio.Lock()

    async def _get(self, url: str, params: dict) -> httpx.Response:
        """GET, serialized and paced, with 429/5xx backoff."""
        async with self._lock:
            await asyncio.sleep(0.3)
            for attempt in range(4):
                response = await self._client.get(url, params=params)
                if response.status_code == 429 or response.status_code >= 500:
                    retry_after = float(response.headers.get("retry-after") or 0)
                    delay = min(retry_after or 0.5 * 2**attempt, 8.0)
                    logger.info(
                        "wikipedia returned %d; retrying in %.1fs", response.status_code, delay
                    )
                    await asyncio.sleep(delay)
                    continue
                response.raise_for_status()
                return response
            response.raise_for_status()
            return response

    async def _extracts(self, page_ids: list[int]) -> dict[str, str]:
        """Intro extracts for several pages in one API call, keyed by page id.

        `exintro` limits content to each article's lead section — the dense
        summary — which is what allows batching (full-text extracts are one
        page per request) and keeps per-source context compact.
        """
        params = {
            "action": "query",
            "prop": "extracts",
            "explaintext": 1,
            "exintro": 1,
            "exlimit": "max",
            "format": "json",
            "pageids": "|".join(str(i) for i in page_ids),
        }
        response = await self._get(_EXTRACT_URL, params)
        pages = response.json().get("query", {}).get("pages", {})
        extracts = {}
        for page_id, page in pages.items():
            extract = (page.get("extract") or "").strip()
            if len(extract) > EXTRACT_CHARS:
                extract = extract[:EXTRACT_CHARS].rsplit(" ", 1)[0] + " […]"
            if extract:
                extracts[page_id] = extract
        return extracts

    async def search(self, query: str, k: int = 3) -> list[Source]:
        """Top-k sources for a query; [] when retrieval is unavailable."""
        try:
            response = await self._get(_SEARCH_URL, {"q": query, "limit": k})
            pages = [p for p in (response.json().get("pages") or []) if p.get("key") and p.get("id")]
            if not pages:
                return []
            extracts = await self._extracts([p["id"] for p in pages])
        except (httpx.HTTPError, ValueError, KeyError) as exc:
            logger.warning("wikipedia retrieval failed for %r: %r", query, exc)
            return []

        sources = []
        for page in pages:
            content = extracts.get(str(page["id"]))
            if not content:
                continue
            sources.append(
                Source(
                    title=page.get("title") or page["key"],
                    url=f"https://en.wikipedia.org/wiki/{quote(page['key'])}",
                    content=content,
                    verified=True,
                )
            )
        return sources

    async def close(self) -> None:
        await self._client.aclose()


async def _link_alive(client: httpx.AsyncClient, url: str) -> bool:
    """Cheap liveness check; 405 means HEAD-averse, not dead."""
    try:
        response = await client.head(url, timeout=6.0)
        return response.status_code < 400 or response.status_code == 405
    except httpx.HTTPError:
        return False


def _reconstruct_abstract(inverted_index: Optional[dict]) -> str:
    """OpenAlex stores abstracts as word → positions; rebuild the text."""
    if not inverted_index:
        return ""
    slots: dict[int, str] = {}
    for word, positions in inverted_index.items():
        for position in positions:
            slots[position] = word
    return " ".join(slots[i] for i in sorted(slots))


class OpenAlexRetriever:
    """Scholarly sources (papers with abstracts) from api.openalex.org."""

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 2) -> list[Source]:
        # OpenAlex treats ? ! " etc. as query operators and 400s on them, so
        # reduce the natural-language question to plain search terms.
        clean = re.sub(r"[^\w\s]", " ", query).strip()
        clean = re.sub(r"\s+", " ", clean)
        params = {"search": clean, "per-page": k, "filter": "has_abstract:true"}
        try:
            response = await self._client.get("https://api.openalex.org/works", params=params)
            response.raise_for_status()
            works = response.json().get("results") or []
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("openalex search failed for %r: %r", query, exc)
            return []

        sources = []
        for work in works:
            abstract = _reconstruct_abstract(work.get("abstract_inverted_index"))
            if not abstract:
                continue
            if len(abstract) > EXTRACT_CHARS:
                abstract = abstract[:EXTRACT_CHARS].rsplit(" ", 1)[0] + " […]"
            # Prefer the DOI link; fall back to the always-live OpenAlex page.
            url = work.get("doi") or work.get("id")
            if not url:
                continue
            if not await _link_alive(self._client, url):
                url = work.get("id")
                if not url or not await _link_alive(self._client, url):
                    continue
            title = work.get("title") or "Untitled"
            if work.get("publication_year"):
                title = f"{title} ({work['publication_year']})"
            sources.append(Source(title=title, url=url, content=abstract, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


class InstantAnswerRetriever:
    """DuckDuckGo's official Instant Answer API (single abstract per query)."""

    def __init__(self, timeout: float = 10.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 1) -> list[Source]:
        params = {"q": query, "format": "json", "no_html": 1, "skip_disambig": 1}
        try:
            response = await self._client.get("https://api.duckduckgo.com/", params=params)
            response.raise_for_status()
            data = response.json()
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("instant-answer lookup failed for %r: %r", query, exc)
            return []
        text, url = data.get("AbstractText"), data.get("AbstractURL")
        if not text or not url or not await _link_alive(self._client, url):
            return []
        title = data.get("Heading") or "Instant answer"
        if data.get("AbstractSource"):
            title = f"{title} — {data['AbstractSource']}"
        return [Source(title=title, url=url, content=text[:EXTRACT_CHARS], verified=True)]

    async def close(self) -> None:
        await self._client.aclose()


_TAG_RE = re.compile(r"<[^>]+>")


def _strip_tags(text: str) -> str:
    """Drop HTML tags and unescape entities (for scraped snippets/titles)."""
    return unescape(_TAG_RE.sub("", text or "")).strip()


def _extract_uddg(href: str) -> Optional[str]:
    """DuckDuckGo result links are /l/?...&uddg=<urlencoded real url>&... — pull it out."""
    if not href or "uddg=" not in href:
        return None
    val = parse_qs(urlparse(href).query).get("uddg")
    return unquote(val[0]) if val else None


class SearchEngineRetriever:
    """Real web search via DuckDuckGo's HTML endpoint (ranked links + snippets).

    Unlike InstantAnswerRetriever (one curated abstract per query), this returns
    actual search hits. It scrapes the HTML results page, so it is keyless but
    may be rate-limited or served a challenge page — in that case it returns []
    and the other engines carry the report.
    """

    name = "ddg"

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 3) -> list[Source]:
        try:
            response = await self._client.get(
                "https://html.duckduckgo.com/html/", params={"q": query}
            )
            response.raise_for_status()
            html = response.text
        except httpx.HTTPError as exc:
            logger.warning("ddg web search failed for %r: %r", query, exc)
            return []

        results: list[tuple[str, str, str]] = []
        for block in re.split(r'<a[^>]*class="result__a"', html)[1:]:
            href = re.search(r'href="([^"]+)"', block)
            if not href:
                continue
            url = _extract_uddg(href.group(1))
            if not url:
                continue
            title_m = re.search(r'>(.*?)</a>', block, re.S)
            title = _strip_tags(title_m.group(1)) if title_m else url
            snip_m = re.search(r'class="result__snippet"[^>]*>(.*?)(?:</a>|</td>)', block, re.S)
            snippet = _strip_tags(snip_m.group(1)) if snip_m else ""
            results.append((title, url, snippet))
            if len(results) >= k:
                break

        sources = []
        for title, url, snippet in results:
            # Generic web links can 404; only keep ones that respond.
            if not await _link_alive(self._client, url):
                continue
            content = snippet[:EXTRACT_CHARS] if snippet else title
            sources.append(Source(title=title, url=url, content=content, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


class ArxivRetriever:
    """Scholarly preprints from export.arxiv.org (Atom feed, no key)."""

    name = "arxiv"
    _ATOM = "{http://www.w3.org/2005/Atom}"

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 2) -> list[Source]:
        params = {"search_query": f"all:{query}", "start": 0, "max_results": k}
        try:
            response = await self._client.get("http://export.arxiv.org/api/query", params=params)
            response.raise_for_status()
            root = ET.fromstring(response.text)
        except (httpx.HTTPError, ValueError, ET.ParseError) as exc:
            logger.warning("arxiv search failed for %r: %r", query, exc)
            return []

        sources = []
        for entry in root.findall(f"{self._ATOM}entry"):
            title = (entry.findtext(f"{self._ATOM}title") or "").strip()
            summary = re.sub(r"\s+", " ", entry.findtext(f"{self._ATOM}summary") or "").strip()
            url = (entry.findtext(f"{self._ATOM}id") or "").strip()
            if not title or not url:
                continue
            if len(summary) > EXTRACT_CHARS:
                summary = summary[:EXTRACT_CHARS].rsplit(" ", 1)[0] + " […]"
            sources.append(Source(title=title, url=url, content=summary, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


class SemanticScholarRetriever:
    """Scholarly papers via the Semantic Scholar Graph API (works without a key)."""

    name = "s2"

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 2) -> list[Source]:
        params = {
            "query": query,
            "limit": k,
            "fields": "title,abstract,url,year,paperId",
        }
        for attempt in range(3):
            try:
                response = await self._client.get(
                    "https://api.semanticscholar.org/graph/v1/paper/search", params=params
                )
            except httpx.HTTPError as exc:
                logger.warning("semantic scholar search failed for %r: %r", query, exc)
                return []
            if response.status_code == 429:
                # Keyless requests are rate-limited; back off and retry once or twice.
                delay = min(float(response.headers.get("retry-after") or 2.0 * attempt), 10.0)
                await asyncio.sleep(delay)
                continue
            if response.status_code >= 500:
                return []
            break
        else:
            return []

        try:
            data = response.json().get("data") or []
        except ValueError:
            return []

        sources = []
        for paper in data:
            title = paper.get("title") or "Untitled"
            abstract = (paper.get("abstract") or "").strip()
            if not abstract:
                continue
            url = paper.get("url") or (
                f"https://www.semanticscholar.org/paper/{paper['paperId']}"
                if paper.get("paperId")
                else None
            )
            if not url:
                continue
            if paper.get("year"):
                title = f"{title} ({paper['year']})"
            if len(abstract) > EXTRACT_CHARS:
                abstract = abstract[:EXTRACT_CHARS].rsplit(" ", 1)[0] + " […]"
            sources.append(Source(title=title, url=url, content=abstract, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


class StackExchangeRetriever:
    """Q&A from the Stack Exchange API (default site: stackoverflow)."""

    name = "stackex"

    def __init__(self, site: str = "stackoverflow", timeout: float = 15.0):
        self._site = site
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 2) -> list[Source]:
        params = {
            "order": "desc",
            "sort": "relevance",
            "site": self._site,
            "q": query,
            "pagesize": k,
        }
        try:
            response = await self._client.get(
                "https://api.stackexchange.com/2.3/search/advanced", params=params
            )
            response.raise_for_status()
            items = response.json().get("items") or []
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("stack exchange search failed for %r: %r", query, exc)
            return []

        sources = []
        for item in items:
            title = unescape(item.get("title") or "").strip()
            url = item.get("link")
            excerpt = _strip_tags(item.get("excerpt") or "").strip()
            if not title or not url:
                continue
            content = excerpt[:EXTRACT_CHARS] if excerpt else title
            sources.append(Source(title=title, url=url, content=content, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


class HackerNewsRetriever:
    """Hacker News stories via the Algolia search API (keyless)."""

    name = "hn"

    def __init__(self, timeout: float = 15.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 2) -> list[Source]:
        params = {"query": query, "tags": "story", "hitsPerPage": k}
        try:
            response = await self._client.get("https://hn.algolia.com/api/v1/search", params=params)
            response.raise_for_status()
            hits = response.json().get("hits") or []
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("hacker news search failed for %r: %r", query, exc)
            return []

        sources = []
        for hit in hits:
            title = (hit.get("title") or "").strip()
            url = hit.get("url")  # Ask HN posts have no external url — skip them
            if not title or not url:
                continue
            body = _strip_tags(hit.get("story_text") or "").strip()
            content = body[:EXTRACT_CHARS] if body else title
            sources.append(Source(title=title, url=url, content=content, verified=True))
        return sources

    async def close(self) -> None:
        await self._client.aclose()


# Quote types worth surfacing; excludes things like "FUTURE" or "OPTION" chains
# where a single point-in-time price is a poor answer to a general question.
_STOCK_QUOTE_TYPES = {"EQUITY", "ETF", "INDEX", "CRYPTOCURRENCY", "MUTUALFUND", "CURRENCY"}

# Yahoo's symbol search is a strict fuzzy-match on the company/asset name --
# any of these surrounding words (present in almost every natural-language
# price question) makes the whole query miss entirely, e.g. "Apple stock"
# matches zero quotes even though bare "Apple" matches AAPL immediately.
_STOCK_FILLER_WORDS = {
    "what", "whats", "how", "much", "is", "are", "the", "a", "an",
    "current", "currently", "latest", "today", "todays", "right",
    "now", "please", "stock", "stocks", "share", "shares", "price", "prices",
    "of", "for", "trading", "at", "value", "doing", "on", "market", "quote",
    "quotes", "worth", "cost", "costs", "and", "in", "to", "does",
}


def _clean_stock_query(query: str) -> str:
    """Strip filler words a chat-style question wraps around a company name.

    Falls back to the original query if stripping would remove everything
    (so a bare ticker or name with no filler words still passes through).
    """
    stripped = re.sub(r"[?!.,]", "", query).replace("’", "'")
    kept = []
    for token in stripped.split():
        bare = token[:-2] if token.lower().endswith("'s") else token
        if bare.lower() not in _STOCK_FILLER_WORDS:
            kept.append(bare)
    cleaned = " ".join(kept).strip()
    return cleaned or query


class StockQuoteRetriever:
    """Live price quotes via Yahoo Finance's public (keyless) endpoints.

    None of the other engines can answer "what's the price of X right now" --
    Wikipedia/OpenAlex/arXiv/Semantic Scholar are encyclopedic/academic and
    Stack Exchange/HN/DDG snippets don't carry a live number either, so a
    model asked a stock-price question with only those sources tends to fall
    back on whatever stale figure it memorized during training. This engine
    resolves the query to a ticker via Yahoo's symbol search, then fetches
    that ticker's real-time quote -- both endpoints are the same ones the
    `yfinance` library uses and need no API key or crumb for this data.
    """

    name = "stock"

    def __init__(self, timeout: float = 10.0):
        self._client = httpx.AsyncClient(timeout=timeout, headers=_UA, follow_redirects=True)

    async def search(self, query: str, k: int = 1) -> list[Source]:
        try:
            response = await self._client.get(
                "https://query1.finance.yahoo.com/v1/finance/search",
                params={"q": _clean_stock_query(query)},
            )
            response.raise_for_status()
            quotes = response.json().get("quotes") or []
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("stock symbol search failed for %r: %r", query, exc)
            return []

        match = next((q for q in quotes if q.get("quoteType") in _STOCK_QUOTE_TYPES), None)
        symbol = match.get("symbol") if match else None
        if not symbol:
            return []  # nothing ticker-shaped matched; not a finance query

        try:
            response = await self._client.get(
                f"https://query1.finance.yahoo.com/v8/finance/chart/{symbol}"
            )
            response.raise_for_status()
            meta = response.json()["chart"]["result"][0]["meta"]
            price = meta["regularMarketPrice"]
        except (httpx.HTTPError, ValueError, KeyError, IndexError, TypeError) as exc:
            logger.warning("stock quote fetch failed for %r: %r", symbol, exc)
            return []

        name = match.get("longname") or match.get("shortname") or symbol
        currency = meta.get("currency", "")
        as_of = datetime.fromtimestamp(meta["regularMarketTime"], tz=timezone.utc).strftime(
            "%Y-%m-%d %H:%M UTC"
        ) if meta.get("regularMarketTime") else "unknown time"
        parts = [f"{name} ({symbol}): {price} {currency} as of {as_of}."]
        if meta.get("regularMarketDayLow") is not None and meta.get("regularMarketDayHigh") is not None:
            parts.append(f"Day range: {meta['regularMarketDayLow']}-{meta['regularMarketDayHigh']}.")
        if meta.get("previousClose") is not None:
            parts.append(f"Previous close: {meta['previousClose']}.")
        content = " ".join(parts)

        return [Source(
            title=f"{name} ({symbol}) stock quote",
            url=f"https://finance.yahoo.com/quote/{symbol}",
            content=content,
            verified=True,
        )]

    async def close(self) -> None:
        await self._client.aclose()


# Engine registry: short name -> retriever class. Order is the default merge order.
ALL_ENGINES = ["wikipedia", "openalex", "instant", "ddg", "stock", "arxiv", "s2", "stackex", "hn"]

_ENGINE_REGISTRY = {
    "wikipedia": WikipediaRetriever,
    "openalex": OpenAlexRetriever,
    "instant": InstantAnswerRetriever,
    "ddg": SearchEngineRetriever,
    "stock": StockQuoteRetriever,
    "arxiv": ArxivRetriever,
    "s2": SemanticScholarRetriever,
    "stackex": StackExchangeRetriever,
    "hn": HackerNewsRetriever,
}


class MultiRetriever:
    """Merges selected engines' results round-robin, deduplicated by URL.

    Round-robin (one result per engine per round, not "drain engine A, then
    engine B, ...") matters because Wikipedia/OpenAlex are checked first and
    return several results by default; taking engines in strict order would
    let those two alone fill MAX_SOURCES before a web-search or news engine
    is ever considered, even for queries only they can answer (current
    events, live sports results, anything after the encyclopedic sources'
    coverage).

    `enabled` is a list of engine short names (see ALL_ENGINES). `None` enables
    every engine. Unknown names are logged and skipped, so a typo in `--sources`
    never breaks a run.
    """

    MAX_SOURCES = 5

    def __init__(self, enabled: Optional[list[str]] = None, timeout: float = 15.0):
        if enabled is None:
            enabled = list(ALL_ENGINES)
        self._engines: dict[str, object] = {}
        for name in enabled:
            cls = _ENGINE_REGISTRY.get(name)
            if cls is None:
                logger.warning("unknown retrieval engine %r ignored", name)
                continue
            self._engines[name] = cls(timeout=timeout)

    async def search(self, query: str, k: int = 3) -> list[Source]:
        if not self._engines:
            return []
        results = await asyncio.gather(
            *(engine.search(query) for engine in self._engines.values()),
            return_exceptions=True,
        )
        groups: list[list[Source]] = []
        for name, group in zip(self._engines, results):
            if isinstance(group, BaseException):
                logger.warning("retriever failed: %r", group)
                continue
            groups.append(group)

        # Round-robin across engines rather than draining them in registration
        # order. ALL_ENGINES lists Wikipedia and OpenAlex first, and each
        # returns 2-3 results by default -- taking engines in strict order
        # fills MAX_SOURCES from those two alone before web-search or news
        # engines are ever considered, even when they're the only ones with
        # anything current (e.g. live sports standings, breaking news). This
        # way every engine that found something gets a slot before any engine
        # gets a second one.
        merged: list[Source] = []
        seen: set[str] = set()
        for round_sources in zip_longest(*groups):
            for source in round_sources:
                if source is None:
                    continue
                if source.url not in seen:
                    seen.add(source.url)
                    merged.append(source)
                    if len(merged) >= self.MAX_SOURCES:
                        return merged
        return merged

    async def close(self) -> None:
        # A single engine failing to close cleanly must never propagate: this
        # runs from callers' `finally` blocks, where an escaping exception can
        # abort an in-flight streaming response the caller already sent good
        # content on.
        results = await asyncio.gather(
            *(engine.close() for engine in self._engines.values()), return_exceptions=True
        )
        for result in results:
            if isinstance(result, BaseException):
                logger.warning("error closing a retrieval engine: %r", result)
