from orcastrator.orchestrator import WRITER_CHAR_BUDGET, _budget_sections, _parse_questions


def test_parse_numbered_list():
    raw = "1. What is X?\n2) How does Y work?\n3.  Why Z?"
    assert _parse_questions(raw) == ["What is X?", "How does Y work?", "Why Z?"]


def test_parse_bulleted_list():
    raw = "- First question\n* Second question"
    assert _parse_questions(raw) == ["First question", "Second question"]


def test_parse_skips_blank_lines():
    raw = "\n\nQuestion one\n\n   \nQuestion two\n"
    assert _parse_questions(raw) == ["Question one", "Question two"]


def test_parse_plain_lines_unchanged():
    raw = "What causes tides?\nHow do neap tides differ?"
    assert _parse_questions(raw) == ["What causes tides?", "How do neap tides differ?"]


def test_parse_empty_input():
    assert _parse_questions("") == []
    assert _parse_questions("\n  \n") == []


def test_budget_under_limit_is_unchanged():
    sections = ["## Q1\nshort answer", "## Q2\nanother short answer"]
    assert _budget_sections(sections) == "\n\n".join(sections)


def test_budget_truncates_oversized_sections():
    big = "## Q1\n" + ("x" * (WRITER_CHAR_BUDGET * 2))
    small = "## Q2\ntiny"
    out = _budget_sections([big, small])
    assert len(out) <= WRITER_CHAR_BUDGET + 100  # separators + ellipsis slack
    assert "[…]" in out
    assert "## Q2" in out  # smaller section survives


def test_budget_empty_sections():
    assert _budget_sections([]) == ""
