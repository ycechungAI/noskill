import json

import httpx
import respx

from schedlm.agent import tools


class _FakeDDGS:
    def __init__(self, hits=None, error=None):
        self._hits = hits
        self._error = error

    def text(self, query, max_results=5):
        if self._error is not None:
            raise self._error
        return self._hits


def test_web_search_success(monkeypatch):
    hits = [
        {"title": "Result 1", "href": "https://a.example", "body": "snippet a"},
        {"title": "Result 2", "href": "https://b.example", "body": "snippet b"},
    ]
    monkeypatch.setattr(tools, "DDGS", lambda: _FakeDDGS(hits=hits))
    result = json.loads(tools.web_search("python", max_results=2))
    assert result == [
        {"title": "Result 1", "url": "https://a.example", "snippet": "snippet a"},
        {"title": "Result 2", "url": "https://b.example", "snippet": "snippet b"},
    ]


def test_web_search_empty_results(monkeypatch):
    monkeypatch.setattr(tools, "DDGS", lambda: _FakeDDGS(hits=[]))
    result = json.loads(tools.web_search("nothing"))
    assert "error" in result


def test_web_search_raises_returns_error_string_not_exception(monkeypatch):
    monkeypatch.setattr(tools, "DDGS", lambda: _FakeDDGS(error=RuntimeError("network down")))
    result = json.loads(tools.web_search("python"))
    assert "search unavailable" in result["error"]


def test_web_search_empty_query():
    result = json.loads(tools.web_search(""))
    assert "error" in result


@respx.mock
def test_fetch_page_extracts_text_and_drops_script_style_nav_footer():
    html = (
        "<html><head><style>body{color:red}</style></head>"
        "<body><script>evil()</script><nav>Menu</nav>"
        "<p>Hello world</p><footer>Copyright</footer></body></html>"
    )
    respx.get("https://example.com/page").mock(
        return_value=httpx.Response(200, text=html, headers={"content-type": "text/html"})
    )
    result = tools.fetch_page("https://example.com/page")
    assert result.startswith('<fetched_content url="https://example.com/page">')
    assert "Hello world" in result
    assert "evil()" not in result
    assert "Menu" not in result
    assert "Copyright" not in result


@respx.mock
def test_fetch_page_http_error_status():
    respx.get("https://example.com/missing").mock(return_value=httpx.Response(404))
    result = tools.fetch_page("https://example.com/missing")
    assert "error: HTTP 404" in result


@respx.mock
def test_fetch_page_timeout():
    respx.get("https://example.com/slow").mock(
        side_effect=httpx.TimeoutException("timed out")
    )
    result = tools.fetch_page("https://example.com/slow")
    assert "error: request timed out" in result


def test_fetch_page_empty_url():
    result = tools.fetch_page("")
    assert "error: empty url" in result


@respx.mock
def test_fetch_page_compresses_when_over_budget():
    long_text = "word " * 5000
    html = f"<html><body><p>{long_text}</p></body></html>"
    respx.get("https://example.com/long").mock(
        return_value=httpx.Response(200, text=html, headers={"content-type": "text/html"})
    )
    calls = []

    def compress(text, url):
        calls.append(url)
        return "short summary"

    result = tools.fetch_page("https://example.com/long", max_chars=100, compress=compress)
    assert "short summary" in result
    assert calls == ["https://example.com/long"]


@respx.mock
def test_fetch_page_compress_failure_falls_back_to_hard_truncate():
    long_text = "word " * 5000
    html = f"<html><body><p>{long_text}</p></body></html>"
    respx.get("https://example.com/long").mock(
        return_value=httpx.Response(200, text=html, headers={"content-type": "text/html"})
    )

    def broken_compress(text, url):
        raise RuntimeError("compressor down")

    result = tools.fetch_page(
        "https://example.com/long", max_chars=50, compress=broken_compress
    )
    inner = result.split(">", 1)[1].rsplit("<", 1)[0]
    assert len(inner) <= 50


def test_current_datetime_is_iso_with_tz():
    from datetime import datetime

    value = tools.current_datetime()
    parsed = datetime.fromisoformat(value)
    assert parsed.tzinfo is not None


def test_read_write_note_round_trip(tmp_path):
    notes_dir = tmp_path / "notes"
    assert "error" in json.loads(tools.read_note("missing.txt", notes_dir))
    assert json.loads(tools.write_note("seen.txt", "hello", notes_dir)) == {"ok": True}
    assert tools.read_note("seen.txt", notes_dir) == "hello"


def test_note_path_traversal_rejected(tmp_path):
    notes_dir = tmp_path / "notes"
    assert "error" in json.loads(tools.write_note("../evil.txt", "x", notes_dir))
    assert "error" in json.loads(tools.write_note("a/b.txt", "x", notes_dir))
    assert "error" in json.loads(tools.read_note("../evil.txt", notes_dir))
    assert "error" in json.loads(tools.read_note("a/b.txt", notes_dir))


def test_execute_dispatch_and_unknown_tool(tmp_path):
    ctx = tools.ToolContext(notes_dir=tmp_path / "notes")
    result = tools.execute("current_datetime", {}, ctx)
    assert result
    result = json.loads(tools.execute("not_a_tool", {}, ctx))
    assert "error" in result


def test_execute_truncates_to_max_result_chars(tmp_path, monkeypatch):
    ctx = tools.ToolContext(notes_dir=tmp_path / "notes")
    huge = "x" * (tools.MAX_TOOL_RESULT_CHARS + 5000)
    monkeypatch.setattr(tools, "read_note", lambda name, notes_dir: huge)
    result = tools.execute("read_note", {"name": "big.txt"}, ctx)
    assert len(result) == tools.MAX_TOOL_RESULT_CHARS


def test_execute_never_raises_on_tool_exception(tmp_path, monkeypatch):
    ctx = tools.ToolContext(notes_dir=tmp_path / "notes")

    def boom(name, notes_dir):
        raise RuntimeError("kaboom")

    monkeypatch.setattr(tools, "read_note", boom)
    result = json.loads(tools.execute("read_note", {"name": "x"}, ctx))
    assert "kaboom" in result["error"]
