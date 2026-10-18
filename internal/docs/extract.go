// Package docs is the local-docs index (skills/local-docs): text pulled out of the user's own files,
// split into chunks, searched by keywords (SQLite FTS5) and, when an embedding model is installed,
// by meaning, with the two rankings fused. It lives in its own database, docs.db, which can be
// deleted and rebuilt at any time (DATA_MODEL.md "docs.db").
package docs

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// ErrUnindexed means a file can't be indexed; the message says why and is shown to the user.
type ErrUnindexed struct{ Reason string }

func (e ErrUnindexed) Error() string { return e.Reason }

const (
	maxFileBytes = 25 << 20 // bigger files are listed as unindexed
	maxTextBytes = 8 << 20  // text kept per file
)

var textExts = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".text": true, ".rst": true, ".org": true, ".log": true,
	".csv": true, ".tsv": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".tex": true,
}

// Supported says whether a file name is one the index looks at at all. Everything else (images,
// videos, archives, code) is ignored, not reported.
func Supported(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return textExts[ext] || ext == ".html" || ext == ".htm" || ext == ".docx" || ext == ".pdf"
}

// Extract returns a file's text. Text is normalised to "\n" line ends. A file it can't read as text
// returns ErrUnindexed with the reason.
func Extract(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > maxFileBytes {
		return "", ErrUnindexed{fmt.Sprintf("larger than %d MB", maxFileBytes>>20)}
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".pdf" {
		return "", ErrUnindexed{"PDF text extraction isn't built in yet"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var text string
	switch {
	case ext == ".docx":
		if text, err = docxText(b); err != nil {
			return "", ErrUnindexed{"can't read this Word file: " + err.Error()}
		}
	case ext == ".html" || ext == ".htm":
		if text, err = htmlText(bytes.NewReader(b)); err != nil {
			return "", ErrUnindexed{"can't read this page: " + err.Error()}
		}
	default:
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return "", ErrUnindexed{"binary, not text"}
		}
		text = string(b)
	}
	text = tidy(text)
	if letters(text) < 20 {
		return "", ErrUnindexed{"no text found (empty, or a scan)"}
	}
	if len(text) > maxTextBytes {
		text = strings.ToValidUTF8(text[:maxTextBytes], "")
	}
	return text, nil
}

func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

// tidy makes line ends "\n", removes NULs and trailing spaces, and collapses runs of blank lines.
func tidy(s string) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "")
	s = strings.ReplaceAll(s, "\x00", "")
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ---- Word ----

// docxText reads word/document.xml: text runs joined, a blank line between paragraphs.
func docxText(b []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", errors.New("not a .docx file")
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		dec := xml.NewDecoder(io.LimitReader(rc, 64<<20)) // a zip bomb stops here
		var sb strings.Builder
		inText := false
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "t":
					inText = true
				case "tab":
					sb.WriteByte('\t')
				case "br", "cr":
					sb.WriteByte('\n')
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					sb.WriteString("\n\n")
				}
			case xml.CharData:
				if inText {
					sb.Write(t)
				}
			}
		}
		return sb.String(), nil
	}
	return "", errors.New("no document.xml")
}

// ---- HTML ----

var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true, "head": true, "nav": true, "footer": true}
var blockTags = map[string]bool{"p": true, "div": true, "section": true, "article": true, "li": true, "tr": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "pre": true, "blockquote": true, "br": true, "ul": true, "ol": true, "table": true}

func htmlText(r io.Reader) (string, error) {
	doc, err := html.Parse(io.LimitReader(r, maxFileBytes))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		block := n.Type == html.ElementNode && blockTags[n.Data]
		if block {
			sb.WriteString("\n\n")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if block {
			sb.WriteString("\n\n")
		}
	}
	walk(doc)
	// Inline whitespace inside a paragraph is collapsed; paragraph breaks stay.
	paras := strings.Split(sb.String(), "\n\n")
	var out []string
	for _, p := range paras {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n"), nil
}
