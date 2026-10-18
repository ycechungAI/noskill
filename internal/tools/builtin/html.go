// Package builtin holds the Go tools the built-in skills use (web, files, models, research).
package builtin

import (
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// extracted is a page reduced to its readable content.
type extracted struct {
	Title string
	Text  string // Markdown
}

// skipTags never contain readable content.
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true, "canvas": true,
	"iframe": true, "form": true, "button": true, "select": true, "input": true, "nav": true,
	"footer": true, "aside": true, "header": true, "dialog": true,
}

// extractHTML turns a page into Markdown: headings, paragraphs, lists, links, code. It prefers
// <article> or <main> when the page has one (a readability heuristic, not a full algorithm), and
// drops navigation, scripts and forms. Text is data: nothing in the page is interpreted.
func extractHTML(r io.Reader, base *url.URL) (extracted, error) {
	doc, err := html.Parse(io.LimitReader(r, 5<<20))
	if err != nil {
		return extracted{}, err
	}
	var out extracted
	out.Title = strings.TrimSpace(textOf(find(doc, "title")))
	root := find(doc, "article")
	if root == nil {
		root = find(doc, "main")
	}
	if root == nil {
		root = find(doc, "body")
	}
	if root == nil {
		root = doc
	}
	w := &mdWriter{base: base}
	w.walk(root)
	out.Text = w.String()
	return out, nil
}

func find(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, tag); f != nil {
			return f
		}
	}
	return nil
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

type mdWriter struct {
	b       strings.Builder
	base    *url.URL
	listing []int // nesting of lists: 0 = bullet, n>0 = ordered counter
	pre     bool
}

func (w *mdWriter) String() string {
	lines := strings.Split(w.b.String(), "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 || len(out) == 0 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func (w *mdWriter) block() {
	s := w.b.String()
	if s != "" && !strings.HasSuffix(s, "\n\n") {
		if !strings.HasSuffix(s, "\n") {
			w.b.WriteString("\n")
		}
		w.b.WriteString("\n")
	}
}

func (w *mdWriter) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		if w.pre {
			w.b.WriteString(n.Data)
			return
		}
		t := collapse(n.Data)
		if t == "" {
			return
		}
		s := w.b.String()
		if t[0] == ' ' && (s == "" || strings.HasSuffix(s, "\n") || strings.HasSuffix(s, " ")) {
			t = strings.TrimLeft(t, " ")
		}
		w.b.WriteString(t)
		return
	case html.ElementNode:
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c)
		}
		return
	}
	if skipTags[n.Data] || hidden(n) {
		return
	}
	switch n.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.block()
		w.b.WriteString(strings.Repeat("#", int(n.Data[1]-'0')) + " ")
		w.children(n)
		w.block()
	case "p", "div", "section", "table", "blockquote", "figure", "details":
		w.block()
		if n.Data == "blockquote" {
			w.b.WriteString("> ")
		}
		w.children(n)
		w.block()
	case "br":
		w.b.WriteString("\n")
	case "hr":
		w.block()
		w.b.WriteString("---")
		w.block()
	case "ul", "ol":
		w.block()
		if n.Data == "ol" {
			w.listing = append(w.listing, 1)
		} else {
			w.listing = append(w.listing, 0)
		}
		w.children(n)
		w.listing = w.listing[:len(w.listing)-1]
		w.block()
	case "li":
		s := w.b.String()
		if s != "" && !strings.HasSuffix(s, "\n") {
			w.b.WriteString("\n")
		}
		indent, marker := "", "- "
		if d := len(w.listing); d > 0 {
			indent = strings.Repeat("  ", d-1)
			if c := w.listing[d-1]; c > 0 {
				marker = itoa(c) + ". "
				w.listing[d-1]++
			}
		}
		w.b.WriteString(indent + marker)
		w.children(n)
		w.b.WriteString("\n")
	case "tr":
		w.children(n)
		w.b.WriteString("\n")
	case "td", "th":
		w.children(n)
		w.b.WriteString(" | ")
	case "pre":
		w.block()
		w.b.WriteString("```\n")
		w.pre = true
		w.children(n)
		w.pre = false
		w.b.WriteString("\n```")
		w.block()
	case "code":
		if w.pre {
			w.children(n)
			return
		}
		w.b.WriteString("`")
		w.children(n)
		w.b.WriteString("`")
	case "strong", "b":
		w.b.WriteString("**")
		w.children(n)
		w.b.WriteString("**")
	case "em", "i":
		w.b.WriteString("*")
		w.children(n)
		w.b.WriteString("*")
	case "a":
		href := attr(n, "href")
		start := w.b.Len()
		w.children(n)
		text := strings.TrimSpace(w.b.String()[start:])
		if href = w.resolve(href); href != "" && text != "" {
			cur := w.b.String()[:start]
			w.b.Reset()
			w.b.WriteString(cur + "[" + text + "](" + href + ")")
		}
	case "img":
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			w.b.WriteString("[image: " + alt + "]")
		}
	default:
		w.children(n)
	}
}

func (w *mdWriter) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}

// resolve keeps only http(s) links, made absolute: javascript: and data: links are dropped.
func (w *mdWriter) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if w.base != nil {
		u = w.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hidden catches elements a reader can't see: the text in them is often SEO filler or an
// injected instruction aimed at models.
func hidden(n *html.Node) bool {
	if attr(n, "hidden") != "" || attr(n, "aria-hidden") == "true" {
		return true
	}
	st := strings.ReplaceAll(strings.ToLower(attr(n, "style")), " ", "")
	return strings.Contains(st, "display:none") || strings.Contains(st, "visibility:hidden") ||
		strings.Contains(st, "font-size:0") || strings.Contains(st, "opacity:0")
}

func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == ' ' {
			space = true
			continue
		}
		if space && b.Len() >= 0 {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
