"""Retriever tests against mocked API responses (no network)."""

import asyncio

import httpx

from orcastrator.retrieval import (
    ArxivRetriever,
    HackerNewsRetriever,
    MultiRetriever,
    SearchEngineRetriever,
    SemanticScholarRetriever,
    Source,
    StackExchangeRetriever,
    StockQuoteRetriever,
    WikipediaRetriever,
    _clean_stock_query,
    _reconstruct_abstract,
)


def _mount(retriever, handler):
    """Swap any retriever's HTTP client for a MockTransport-backed one."""
    retriever._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return retriever


def _mounted(handler):
    retriever = WikipediaRetriever()
    retriever._client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return retriever


def _handler(search_pages, extracts, request_log=None):
    def handler(request):
        if request_log is not None:
            request_log.append(str(request.url))
        if "rest.php" in request.url.path:
            return httpx.Response(200, json={"pages": search_pages})
        requested = request.url.params["pageids"].split("|")
        pages = {pid: {"extract": extracts.get(pid)} for pid in requested}
        return httpx.Response(200, json={"query": {"pages": pages}})

    return handler


def test_search_returns_verified_sources_with_content():
    requests = []
    retriever = _mounted(_handler(
        search_pages=[
            {"id": 1, "key": "Roman_aqueduct", "title": "Roman aqueduct"},
            {"id": 2, "key": "Aqua_Appia", "title": "Aqua Appia"},
        ],
        extracts={"1": "Aqueducts carried water.", "2": "The Aqua Appia was the first."},
        request_log=requests,
    ))
    sources = asyncio.run(retriever.search("aqueducts", k=2))
    assert [s.title for s in sources] == ["Roman aqueduct", "Aqua Appia"]
    assert sources[0].url == "https://en.wikipedia.org/wiki/Roman_aqueduct"
    assert sources[0].content == "Aqueducts carried water."
    assert all(s.verified for s in sources)
    # One search + ONE batched extract call — not one call per page.
    assert len(requests) == 2
    assert "pageids=1%7C2" in requests[1] or "pageids=1|2" in requests[1]


def test_pages_without_extracts_are_dropped():
    retriever = _mounted(_handler(
        search_pages=[
            {"id": 1, "key": "Good_page", "title": "Good page"},
            {"id": 2, "key": "Empty_page", "title": "Empty page"},
        ],
        extracts={"1": "Real content.", "2": None},
    ))
    sources = asyncio.run(retriever.search("query"))
    assert [s.title for s in sources] == ["Good page"]


def test_search_failure_returns_empty_list():
    def handler(request):
        return httpx.Response(404)

    retriever = _mounted(handler)
    assert asyncio.run(retriever.search("query")) == []


def test_429_is_retried_then_succeeds():
    state = {"calls": 0}

    def handler(request):
        state["calls"] += 1
        if state["calls"] == 1:
            return httpx.Response(429, headers={"retry-after": "0"})
        if "rest.php" in request.url.path:
            return httpx.Response(200, json={"pages": [{"id": 1, "key": "P", "title": "P"}]})
        return httpx.Response(200, json={"query": {"pages": {"1": {"extract": "text"}}}})

    retriever = _mounted(handler)
    sources = asyncio.run(retriever.search("query"))
    assert [s.title for s in sources] == ["P"]
    assert state["calls"] == 3  # 429 + retried search + extract


def test_reconstruct_abstract_orders_words_by_position():
    inverted = {"water": [2], "Aqueducts": [0], "carried": [1], "far": [3, 4]}
    assert _reconstruct_abstract(inverted) == "Aqueducts carried water far far"
    assert _reconstruct_abstract(None) == ""


class _StubSub:
    def __init__(self, sources):
        self.sources = sources

    async def search(self, query, k=3):
        return self.sources

    async def close(self):
        pass


def _src(url, title="t"):
    return Source(title=title, url=url, content="c", verified=True)


def test_multi_retriever_merges_and_dedupes_across_engines():
    multi = MultiRetriever(enabled=["wikipedia", "openalex", "instant"])
    multi._engines["wikipedia"] = _StubSub([_src("https://en.wikipedia.org/wiki/A", "Wiki A")])
    multi._engines["openalex"] = _StubSub([_src("https://doi.org/10.1/x", "Paper X")])
    # Instant answer duplicates the Wikipedia URL — must be dropped.
    multi._engines["instant"] = _StubSub([_src("https://en.wikipedia.org/wiki/A", "IA dupe")])

    sources = asyncio.run(multi.search("query"))
    assert [s.title for s in sources] == ["Wiki A", "Paper X"]


def test_multi_retriever_round_robins_instead_of_draining_first_engine():
    # Reproduces the "can't find current info" bug: an early engine
    # (wikipedia) alone returns enough results to fill MAX_SOURCES, which
    # would silently starve out a later engine (ddg) even though ddg found
    # something none of the encyclopedic/academic engines could -- e.g. live
    # sports standings or breaking news that only a web search surfaces.
    multi = MultiRetriever(enabled=["wikipedia", "openalex", "instant", "ddg"])
    multi.MAX_SOURCES = 3
    multi._engines["wikipedia"] = _StubSub([_src(f"https://en.wikipedia.org/wiki/{i}") for i in range(5)])
    multi._engines["openalex"] = _StubSub([])
    multi._engines["instant"] = _StubSub([])
    multi._engines["ddg"] = _StubSub([_src("https://fifa.com/standings", "Live standings")])

    sources = asyncio.run(multi.search("query"))
    assert "Live standings" in [s.title for s in sources]
    assert len(sources) == 3


def test_multi_retriever_round_robin_order_takes_one_per_engine_per_round():
    multi = MultiRetriever(enabled=["wikipedia", "openalex"])
    multi._engines["wikipedia"] = _StubSub([_src("https://a/1", "wiki1"), _src("https://a/2", "wiki2")])
    multi._engines["openalex"] = _StubSub([_src("https://b/1", "oa1"), _src("https://b/2", "oa2")])

    sources = asyncio.run(multi.search("query"))
    # Round 1: wiki1, oa1. Round 2: wiki2, oa2. Not wiki1, wiki2, oa1, oa2.
    assert [s.title for s in sources] == ["wiki1", "oa1", "wiki2", "oa2"]


def test_multi_retriever_survives_a_failing_engine():
    class _Boom:
        async def search(self, query, k=3):
            raise RuntimeError("engine down")

        async def close(self):
            pass

    multi = MultiRetriever(enabled=["wikipedia", "openalex", "instant"])
    multi._engines["wikipedia"] = _Boom()
    multi._engines["openalex"] = _StubSub([_src("https://doi.org/10.1/x", "Paper X")])
    multi._engines["instant"] = _StubSub([])

    sources = asyncio.run(multi.search("query"))
    assert [s.title for s in sources] == ["Paper X"]


def test_multi_retriever_close_survives_a_failing_engine():
    class _BoomOnClose:
        async def close(self):
            raise RuntimeError("teardown exploded")

    multi = MultiRetriever(enabled=["wikipedia", "openalex"])
    multi._engines["wikipedia"] = _BoomOnClose()
    closed = {"openalex": False}

    class _Fine:
        async def close(self):
            closed["openalex"] = True

    multi._engines["openalex"] = _Fine()

    asyncio.run(multi.close())  # must not raise despite wikipedia's close() failing
    assert closed["openalex"] is True  # the other engine still got closed


def test_long_extracts_are_truncated():
    retriever = _mounted(_handler(
        search_pages=[{"id": 1, "key": "Long_page", "title": "Long page"}],
        extracts={"1": "word " * 2000},
    ))
    sources = asyncio.run(retriever.search("query"))
    assert len(sources[0].content) <= 2600
    assert sources[0].content.endswith("[…]")


# --- DuckDuckGo web search (SearchEngineRetriever) ---

_DDG_HTML = """
<div class="result">
  <a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fa&rut=x">First <b>Result</b></a>
  <a class="result__snippet" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fa">Snippet <b>one</b> here.</a>
</div>
<div class="result">
  <a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fdead">Dead link</a>
  <a class="result__snippet">Snippet two.</a>
</div>
"""


def test_ddg_web_search_parses_links_and_snippets_and_drops_dead():
    def handler(request):
        if request.method == "HEAD":
            # example.com/a is alive; /dead is not.
            return httpx.Response(200 if request.url.path == "/a" else 404)
        assert "html.duckduckgo.com" in request.url.host
        return httpx.Response(200, text=_DDG_HTML)

    retriever = _mount(SearchEngineRetriever(), handler)
    sources = asyncio.run(retriever.search("query", k=5))
    assert [s.title for s in sources] == ["First Result"]
    assert sources[0].url == "https://example.com/a"
    assert sources[0].content == "Snippet one here."


def test_ddg_web_search_returns_empty_on_error():
    def handler(request):
        return httpx.Response(429)

    retriever = _mount(SearchEngineRetriever(), handler)
    assert asyncio.run(retriever.search("query")) == []


# --- arXiv ---

_ARXIV_ATOM = """<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>http://arxiv.org/abs/1234.5678v1</id>
    <title>Attention Is All You Need</title>
    <summary>We propose   a new
    architecture.</summary>
  </entry>
</feed>
"""


def test_arxiv_parses_atom_entries():
    def handler(request):
        assert "export.arxiv.org" in request.url.host
        return httpx.Response(200, text=_ARXIV_ATOM)

    retriever = _mount(ArxivRetriever(), handler)
    sources = asyncio.run(retriever.search("transformers"))
    assert len(sources) == 1
    assert sources[0].title == "Attention Is All You Need"
    assert sources[0].url == "http://arxiv.org/abs/1234.5678v1"
    assert sources[0].content == "We propose a new architecture."


# --- Semantic Scholar ---


def test_semantic_scholar_parses_papers():
    def handler(request):
        return httpx.Response(200, json={"data": [
            {"title": "Deep Learning", "abstract": "A review.", "url": "https://s2.org/p/1", "year": 2015},
            {"title": "No Abstract", "abstract": None, "url": "https://s2.org/p/2"},
        ]})

    retriever = _mount(SemanticScholarRetriever(), handler)
    sources = asyncio.run(retriever.search("deep learning"))
    assert [s.title for s in sources] == ["Deep Learning (2015)"]
    assert sources[0].url == "https://s2.org/p/1"


def test_semantic_scholar_retries_on_429_then_succeeds():
    state = {"calls": 0}

    def handler(request):
        state["calls"] += 1
        if state["calls"] == 1:
            return httpx.Response(429, headers={"retry-after": "0"})
        return httpx.Response(200, json={"data": [
            {"title": "P", "abstract": "abs", "paperId": "abc"},
        ]})

    retriever = _mount(SemanticScholarRetriever(), handler)
    sources = asyncio.run(retriever.search("q"))
    assert state["calls"] == 2
    assert sources[0].url == "https://www.semanticscholar.org/paper/abc"


# --- Stack Exchange ---


def test_stack_exchange_parses_items():
    def handler(request):
        assert "api.stackexchange.com" in request.url.host
        return httpx.Response(200, json={"items": [
            {"title": "How to &amp; why", "link": "https://stackoverflow.com/q/1", "excerpt": "Do <b>this</b>."},
            {"title": "No link", "excerpt": "x"},
        ]})

    retriever = _mount(StackExchangeRetriever(), handler)
    sources = asyncio.run(retriever.search("q"))
    assert [s.title for s in sources] == ["How to & why"]
    assert sources[0].url == "https://stackoverflow.com/q/1"
    assert sources[0].content == "Do this."


# --- Hacker News ---


def test_hacker_news_parses_hits_and_skips_urlless():
    def handler(request):
        assert "hn.algolia.com" in request.url.host
        return httpx.Response(200, json={"hits": [
            {"title": "Show HN: Thing", "url": "https://thing.dev", "story_text": "<p>Body.</p>"},
            {"title": "Ask HN: no url", "url": None},
        ]})

    retriever = _mount(HackerNewsRetriever(), handler)
    sources = asyncio.run(retriever.search("q"))
    assert [s.title for s in sources] == ["Show HN: Thing"]
    assert sources[0].url == "https://thing.dev"
    assert sources[0].content == "Body."


# --- Stock quotes (StockQuoteRetriever) ---


def test_clean_stock_query_strips_filler_words():
    # Real failure mode: Yahoo's fuzzy symbol search matches bare "Tesla" but
    # misses "Tesla stock" or "stock price of Tesla" entirely (zero results),
    # so filler words wrapped around the company name must be stripped first.
    assert _clean_stock_query("what is the stock price of Tesla") == "Tesla"
    assert _clean_stock_query("how much is Apple stock trading at") == "Apple"
    assert _clean_stock_query("what's Microsoft's stock price right now") == "Microsoft"
    assert _clean_stock_query("AAPL") == "AAPL"
    # Stripping everything falls back to the original query rather than "".
    assert _clean_stock_query("what is the price of the") == "what is the price of the"


def _quote_search_response(quotes):
    return httpx.Response(200, json={"quotes": quotes})


def _chart_response(meta):
    return httpx.Response(200, json={"chart": {"result": [{"meta": meta}]}})


def test_stock_quote_resolves_symbol_and_formats_price():
    def handler(request):
        if "finance/search" in request.url.path:
            assert request.url.params["q"] == "Tesla"  # filler words stripped
            return _quote_search_response([
                {"symbol": "TSLA", "quoteType": "EQUITY", "longname": "Tesla, Inc."},
            ])
        assert request.url.path.endswith("/chart/TSLA")
        return _chart_response({
            "regularMarketPrice": 390.05,
            "currency": "USD",
            "regularMarketTime": 1784224307,
            "regularMarketDayLow": 385.32,
            "regularMarketDayHigh": 395.31,
            "previousClose": 394.46,
        })

    retriever = _mount(StockQuoteRetriever(), handler)
    sources = asyncio.run(retriever.search("what is the stock price of Tesla"))
    assert len(sources) == 1
    assert sources[0].title == "Tesla, Inc. (TSLA) stock quote"
    assert sources[0].url == "https://finance.yahoo.com/quote/TSLA"
    assert "390.05 USD" in sources[0].content
    assert "Day range: 385.32-395.31" in sources[0].content
    assert "Previous close: 394.46" in sources[0].content
    assert sources[0].verified is True


def test_stock_quote_returns_empty_when_no_ticker_matches():
    def handler(request):
        assert "finance/search" in request.url.path
        return _quote_search_response([])  # non-financial query: nothing ticker-shaped

    retriever = _mount(StockQuoteRetriever(), handler)
    assert asyncio.run(retriever.search("roman aqueducts")) == []


def test_stock_quote_skips_non_price_quote_types():
    def handler(request):
        # Only a FUTURE-type match exists; not one of the accepted quote types.
        return _quote_search_response([{"symbol": "ESZ26", "quoteType": "FUTURE"}])

    retriever = _mount(StockQuoteRetriever(), handler)
    assert asyncio.run(retriever.search("s&p futures")) == []


def test_stock_quote_search_failure_returns_empty_list():
    def handler(request):
        return httpx.Response(500)

    retriever = _mount(StockQuoteRetriever(), handler)
    assert asyncio.run(retriever.search("Tesla")) == []


def test_stock_quote_chart_failure_returns_empty_list():
    def handler(request):
        if "finance/search" in request.url.path:
            return _quote_search_response([{"symbol": "TSLA", "quoteType": "EQUITY"}])
        return httpx.Response(500)  # chart endpoint down

    retriever = _mount(StockQuoteRetriever(), handler)
    assert asyncio.run(retriever.search("Tesla")) == []


# --- MultiRetriever engine selection ---


def test_multi_retriever_uses_only_enabled_engines():
    multi = MultiRetriever(enabled=["ddg", "arxiv"])
    assert sorted(multi._engines) == ["arxiv", "ddg"]
    # Stub them out so no network is touched.
    multi._engines["ddg"] = _StubSub([_src("https://a.com", "A")])
    multi._engines["arxiv"] = _StubSub([_src("https://b.com", "B")])
    sources = asyncio.run(multi.search("q"))
    assert sorted(s.title for s in sources) == ["A", "B"]


def test_multi_retriever_ignores_unknown_engine_names():
    multi = MultiRetriever(enabled=["wikipedia", "bogus"])
    assert list(multi._engines) == ["wikipedia"]


def test_multi_retriever_default_enables_all_engines():
    from orcastrator.retrieval import ALL_ENGINES

    multi = MultiRetriever()
    assert list(multi._engines) == ALL_ENGINES


def test_multi_retriever_empty_selection_returns_no_sources():
    multi = MultiRetriever(enabled=[])
    assert asyncio.run(multi.search("q")) == []
