"""Report export: Markdown, standalone HTML, and PDF.

- .md    — written as-is.
- .html  — rendered by pandoc when installed, otherwise by the small built-in
           Markdown renderer below (headings, lists, emphasis, code, links,
           and the pipeline's [[n]](url) citation links), so HTML export works
           with no external dependencies.
- .pdf   — requires pandoc (plus one of its PDF engines); raises a clear
           error with install hints when unavailable.
"""

import html
import logging
import re
import shutil
import subprocess
from pathlib import Path

logger = logging.getLogger("orcastrator.export")

_HTML_STYLE = """
  body { margin: 0 auto; max-width: 760px; padding: 2.5rem 1.25rem 4rem;
         font: 16px/1.6 -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
         color: #1a1d21; }
  h1 { font-size: 1.6rem; line-height: 1.3; } h2 { font-size: 1.25rem; } h3 { font-size: 1.05rem; }
  code { background: #eef1f4; border-radius: 4px; padding: .1em .35em; font-size: .9em; }
  pre { background: #eef1f4; border-radius: 8px; padding: .8rem 1rem; overflow-x: auto; }
  pre code { background: none; padding: 0; }
  blockquote { border-left: 3px solid #dde1e6; margin-left: 0; padding-left: 1rem; color: #5c6470; }
  a { color: #2563eb; }
  a.cite { font-size: .78em; vertical-align: super; text-decoration: none; font-weight: 600; }
"""


def _inline(escaped: str) -> str:
    """Inline markdown on an already-HTML-escaped line (mirrors the web UI renderer)."""
    out = re.sub(r"`([^`]+)`", r"<code>\1</code>", escaped)
    # [[n]](url) citation links produced by the pipeline.
    out = re.sub(
        r"\[\[(\d+)\]\]\((https?://[^\s)]+)\)",
        r'<a class="cite" href="\2">[\1]</a>',
        out,
    )
    # [Title](url) links, e.g. the References section.
    out = re.sub(
        r"\[([^\]]+)\]\((https?://[^\s)]+)\)",
        r'<a href="\2">\1</a>',
        out,
    )
    out = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", out)
    out = re.sub(r"\*([^*]+)\*", r"<em>\1</em>", out)
    return out


def markdown_to_html(markdown_text: str) -> str:
    """Small Markdown -> HTML body converter covering what reports contain."""
    out: list[str] = []
    para: list[str] = []
    list_tag: str = ""
    fence: list[str] = []
    in_fence = False

    def flush_para():
        if para:
            out.append("<p>" + _inline(" ".join(para)) + "</p>")
            para.clear()

    def flush_list():
        nonlocal list_tag
        if list_tag:
            out.append(f"</{list_tag}>")
            list_tag = ""

    for raw in markdown_text.split("\n"):
        line = html.escape(raw, quote=False)
        if in_fence:
            if raw.startswith("```"):
                out.append("<pre><code>" + "\n".join(fence) + "</code></pre>")
                fence.clear()
                in_fence = False
            else:
                fence.append(line)
            continue
        if raw.startswith("```"):
            flush_para(); flush_list()
            in_fence = True
            continue
        heading = re.match(r"^(#{1,6})\s+(.*)", line)
        if heading:
            flush_para(); flush_list()
            level = len(heading.group(1))
            out.append(f"<h{level}>" + _inline(heading.group(2)) + f"</h{level}>")
            continue
        bullet = re.match(r"^\s*[-*]\s+(.*)", line)
        numbered = re.match(r"^\s*\d+[.)]\s+(.*)", line)
        if bullet or numbered:
            flush_para()
            want = "ul" if bullet else "ol"
            if list_tag != want:
                flush_list()
                out.append(f"<{want}>")
                list_tag = want
            out.append("<li>" + _inline((bullet or numbered).group(1)) + "</li>")
            continue
        if line.startswith("&gt;"):
            flush_para(); flush_list()
            out.append("<blockquote><p>" + _inline(line[4:].strip()) + "</p></blockquote>")
            continue
        if not line.strip():
            flush_para(); flush_list()
            continue
        para.append(line)
    flush_para(); flush_list()
    if in_fence:  # unterminated fence
        out.append("<pre><code>" + "\n".join(fence) + "</code></pre>")
    return "\n".join(out)


def render_html_document(markdown_text: str, title: str = "Research report") -> str:
    return (
        "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n"
        f"<title>{html.escape(title)}</title>\n<style>{_HTML_STYLE}</style>\n</head>\n<body>\n"
        f"{markdown_to_html(markdown_text)}\n</body>\n</html>\n"
    )


def _pandoc(markdown_text: str, out_path: str) -> None:
    pandoc = shutil.which("pandoc")
    if not pandoc:
        raise FileNotFoundError("pandoc not installed")
    result = subprocess.run(
        [pandoc, "-f", "markdown", "-s", "--metadata", "title=Research report", "-o", out_path],
        input=markdown_text.encode(),
        capture_output=True,
        timeout=120,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"pandoc failed: {result.stderr.decode(errors='replace')[:300]}"
        )


def export_report(markdown_text: str, out_path: str, title: str = "Research report") -> str:
    """Write the report in the format implied by out_path's extension.

    Returns a short human-readable note about what was written.
    """
    ext = Path(out_path).suffix.lower()
    if ext in ("", ".md", ".markdown", ".txt"):
        # Attach the title if not already present as a markdown h1 header
        if title and "# " not in markdown_text:
            markdown_text = f"# {title}\n\n" + markdown_text
        Path(out_path).write_text(markdown_text)
        return f"Wrote Markdown to {out_path}"

    if ext in (".html", ".htm"):
        try:
            _pandoc(markdown_text, out_path)
            return f"Wrote HTML to {out_path} (via pandoc)"
        except FileNotFoundError:
            Path(out_path).write_text(render_html_document(markdown_text, title))
            return f"Wrote HTML to {out_path} (built-in renderer; install pandoc for richer output)"

    if ext == ".pdf":
        try:
            _pandoc(markdown_text, out_path)
        except FileNotFoundError:
            raise RuntimeError(
                "PDF export needs pandoc (e.g. `brew install pandoc`) plus a PDF engine "
                "such as `brew install basictex` or weasyprint. Alternatively export "
                ".html, or use the web UI's Print/PDF button."
            )
        except RuntimeError as exc:
            raise RuntimeError(
                f"{exc}\nhint: pandoc needs a PDF engine — try `brew install basictex` "
                "or `pip install weasyprint`, or export .html instead."
            )
        return f"Wrote PDF to {out_path} (via pandoc)"

    raise RuntimeError(
        f"unsupported output extension {ext!r} — use .md, .html, or .pdf"
    )
