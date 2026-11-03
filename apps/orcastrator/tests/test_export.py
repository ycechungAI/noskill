"""Export tests: built-in HTML renderer, extension routing, pandoc fallbacks."""

import subprocess

import pytest

import orcastrator.export as export
from orcastrator.export import export_report, markdown_to_html, render_html_document


def test_markdown_to_html_covers_report_constructs():
    md = (
        "# Title\n\n"
        "Some **bold** and *italic* and `code`.\n\n"
        "## Section\n\n"
        "- item one\n"
        "- item two\n\n"
        "1. first\n"
        "2. second\n\n"
        "> a quote\n\n"
        "Fact [[1]](https://example.com/a).\n\n"
        "- [1] [Example](https://example.com/a)\n"
    )
    html = markdown_to_html(md)
    assert "<h1>Title</h1>" in html
    assert "<strong>bold</strong>" in html and "<em>italic</em>" in html
    assert "<code>code</code>" in html
    assert "<ul>" in html and "<ol>" in html
    assert "<blockquote>" in html
    assert '<a class="cite" href="https://example.com/a">[1]</a>' in html
    assert '<a href="https://example.com/a">Example</a>' in html


def test_markdown_to_html_escapes_raw_html():
    html = markdown_to_html("evil <script>alert(1)</script> text")
    assert "<script>" not in html
    assert "&lt;script&gt;" in html


def test_render_html_document_is_standalone():
    doc = render_html_document("# Hi", title="My <Topic>")
    assert doc.startswith("<!doctype html>")
    assert "<title>My &lt;Topic&gt;</title>" in doc
    assert "<h1>Hi</h1>" in doc


def test_export_md_writes_text_verbatim(tmp_path):
    out = tmp_path / "report.md"
    note = export_report("# Report\n\nBody.", str(out))
    assert out.read_text() == "# Report\n\nBody."
    assert "Markdown" in note


def test_export_html_falls_back_to_builtin_renderer(tmp_path, monkeypatch):
    monkeypatch.setattr(export.shutil, "which", lambda name: None)
    out = tmp_path / "report.html"
    note = export_report("# Report\n\nBody.", str(out), title="Report")
    text = out.read_text()
    assert text.startswith("<!doctype html>")
    assert "<h1>Report</h1>" in text
    assert "built-in renderer" in note


def test_export_html_prefers_pandoc_when_available(tmp_path, monkeypatch):
    calls = {}

    def fake_run(cmd, **kwargs):
        calls["cmd"] = cmd
        return subprocess.CompletedProcess(cmd, 0, stdout=b"", stderr=b"")

    monkeypatch.setattr(export.shutil, "which", lambda name: "/usr/bin/pandoc")
    monkeypatch.setattr(export.subprocess, "run", fake_run)
    note = export_report("# R", str(tmp_path / "report.html"))
    assert calls["cmd"][0] == "/usr/bin/pandoc"
    assert "via pandoc" in note


def test_export_pdf_without_pandoc_raises_helpful_error(tmp_path, monkeypatch):
    monkeypatch.setattr(export.shutil, "which", lambda name: None)
    with pytest.raises(RuntimeError) as exc:
        export_report("# R", str(tmp_path / "report.pdf"))
    assert "pandoc" in str(exc.value)
    assert ".html" in str(exc.value)  # points at the dependency-free alternative


def test_export_pdf_engine_failure_gets_a_hint(tmp_path, monkeypatch):
    def fake_run(cmd, **kwargs):
        return subprocess.CompletedProcess(cmd, 1, stdout=b"", stderr=b"pdflatex not found")

    monkeypatch.setattr(export.shutil, "which", lambda name: "/usr/bin/pandoc")
    monkeypatch.setattr(export.subprocess, "run", fake_run)
    with pytest.raises(RuntimeError) as exc:
        export_report("# R", str(tmp_path / "report.pdf"))
    assert "pdflatex not found" in str(exc.value)
    assert "PDF engine" in str(exc.value)


def test_export_unknown_extension_is_rejected(tmp_path):
    with pytest.raises(RuntimeError) as exc:
        export_report("# R", str(tmp_path / "report.docx"))
    assert ".docx" in str(exc.value) or "docx" in str(exc.value)
