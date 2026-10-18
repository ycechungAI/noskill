// Command docsite builds the documentation site: every page of the repository's Markdown docs as
// static HTML with a sidebar, no JavaScript and no network (block 5d). Run it from the repository
// root:
//
//	go run ./scripts/docsite [-out site] [-repo https://host/org/noskill/blob/main/]
//
// Links between pages become links between the HTML files. A link to a file that isn't a page
// (source code, config) goes to -repo when it is set and is marked as a source link when not. A link
// to a file that doesn't exist fails the build, so the docs can't drift from the tree.
package main

import (
	"bytes"
	"flag"
	"fmt"
	stdhtml "html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// Page is one Markdown file in the site.
type Page struct {
	Src   string // repo-relative, slash-separated
	Out   string // site-relative, slash-separated
	Title string
	Group string
	Label string // nav label; the title when empty
}

type group struct {
	Name  string
	Pages []string // repo paths, in order; a trailing "/" entry expands a folder (sorted)
}

// groups is the sidebar. Pages not listed here aren't in the site: REVIEW_BLOCK3 and WEEK_RUN are
// working notes for the person running the project.
var groups = []group{
	{"Start", []string{"README.md", "docs/VISION.md", "docs/CLI.md", "docs/CONFIG.md"}},
	{"Skills", []string{"docs/SKILL_AUTHORING.md", "docs/SKILL_FORMAT.md", "docs/LEARNING.md", "skills/README.md",
		"skills/web-search/SKILL.md", "skills/research/SKILL.md", "skills/files/SKILL.md", "skills/code-runner/SKILL.md",
		"skills/local-docs/SKILL.md", "skills/watcher/SKILL.md", "skills/anime-tracker/SKILL.md", "skills/model-manager/SKILL.md",
		"skills/orchestrator/SKILL.md", "skills/skill-author/SKILL.md"}},
	{"How it works", []string{"docs/ARCHITECTURE.md", "docs/ORCHESTRATOR.md", "docs/MODEL_ADAPTERS.md", "docs/PROXY.md",
		"docs/SECURITY.md", "docs/PLATFORMS.md", "docs/DATA_MODEL.md"}},
	{"Project", []string{"SPEC.md", "docs/ROADMAP.md", "docs/PACKAGING.md", "docs/OPEN_QUESTIONS.md", "docs/PRIOR_ART.md", "docs/PHASE0_TESTS.md"}},
	{"Decisions", []string{"docs/adr/"}},
	{"Phase 0 results", []string{"docs/spikes/"}},
}

// examples are links that aren't links: output formats the docs show a model, written in Markdown.
var examples = map[string]bool{"skills/web-search/SKILL.md|url": true}

// dirPages are the pages a link to a folder lands on.
var dirPages = map[string]string{"skills": "skills/README.md", "docs": "README.md", ".": "README.md"}

func main() {
	out := flag.String("out", "site", "folder to write the site to (replaced file by file; nothing is deleted)")
	repo := flag.String("repo", "", "base URL source-file links point to, ending in /, e.g. https://host/org/noskill/blob/main/")
	root := flag.String("root", ".", "the repository root")
	flag.Parse()
	n, problems, err := build(*root, *out, *repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docsite:", err)
		os.Exit(1)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "docsite:", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	fmt.Printf("wrote %d pages to %s\n", n, *out)
}

// collect lists the pages in nav order.
func collect(root string) ([]*Page, error) {
	var pages []*Page
	seen := map[string]bool{}
	for _, g := range groups {
		for _, p := range g.Pages {
			var srcs []string
			if strings.HasSuffix(p, "/") {
				ents, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(p)))
				if err != nil {
					return nil, err
				}
				for _, e := range ents {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
						srcs = append(srcs, p+e.Name())
					}
				}
				sort.Strings(srcs)
			} else {
				srcs = []string{p}
			}
			for _, s := range srcs {
				if seen[s] {
					continue
				}
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(s))); err != nil {
					return nil, fmt.Errorf("the site lists %s, which doesn't exist", s)
				}
				seen[s] = true
				pages = append(pages, &Page{Src: s, Out: outPath(s), Group: g.Name})
			}
		}
	}
	return pages, nil
}

func outPath(src string) string {
	if src == "README.md" {
		return "index.html"
	}
	return strings.TrimSuffix(src, ".md") + ".html"
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithAttribute()),
	goldmark.WithRendererOptions(html.WithUnsafe()), // the docs hold a few HTML comments; they're ours
)

// build renders every page. problems are broken links, which don't stop the pages being written.
func build(root, out, repo string) (int, []string, error) {
	pages, err := collect(root)
	if err != nil {
		return 0, nil, err
	}
	bySrc := map[string]*Page{}
	for _, p := range pages {
		bySrc[p.Src] = p
	}
	var problems []string
	rendered := map[string]string{}
	for _, p := range pages {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.Src)))
		if err != nil {
			return 0, nil, err
		}
		doc := md.Parser().Parse(text.NewReader(src))
		p.Title = title(doc, src, p.Src)
		p.Title, p.Label = names(p)
		rendered[p.Src] = string(src)
	}
	for _, p := range pages {
		src := []byte(rendered[p.Src])
		doc := md.Parser().Parse(text.NewReader(src))
		ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if l, ok := n.(*ast.Link); ok && entering {
				dest, prob := rewriteLink(root, repo, p, bySrc, string(l.Destination))
				if prob != "" {
					problems = append(problems, p.Src+": "+prob)
				}
				if dest.src {
					l.SetAttributeString("class", []byte("src"))
					l.SetAttributeString("title", []byte("a file in the repository"))
				}
				l.Destination = []byte(dest.url)
			}
			return ast.WalkContinue, nil
		})
		var body bytes.Buffer
		if err := md.Renderer().Render(&body, src, doc); err != nil {
			return 0, nil, err
		}
		page := layout(p, pages, body.String())
		dst := filepath.Join(out, filepath.FromSlash(p.Out))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, nil, err
		}
		if err := os.WriteFile(dst, []byte(page), 0o644); err != nil {
			return 0, nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(out, "style.css"), []byte(css), 0o644); err != nil {
		return 0, nil, err
	}
	sort.Strings(problems)
	return len(pages), problems, nil
}

var adrFile = regexp.MustCompile(`^([0-9]{4})-(.+)\.md$`)

// names gives the title and the sidebar label. A skill is named by its folder (the headings in a
// SKILL.md are for the model), an ADR by its file name (its title is a full sentence).
func names(p *Page) (title, label string) {
	switch {
	case p.Src == "README.md":
		return "noskill", "Overview"
	case p.Src == "skills/README.md":
		return p.Title, "Built-in skills"
	case strings.HasPrefix(p.Src, "skills/") && strings.HasSuffix(p.Src, "/SKILL.md"):
		name := path.Base(path.Dir(p.Src))
		return name + " (skill)", name
	}
	if m := adrFile.FindStringSubmatch(path.Base(p.Src)); m != nil && strings.HasPrefix(p.Src, "docs/adr/") {
		return p.Title, m[1] + " " + strings.ReplaceAll(m[2], "-", " ")
	}
	return p.Title, ""
}

func title(doc ast.Node, src []byte, name string) string {
	for c := doc.FirstChild(); c != nil; c = c.NextSibling() {
		if h, ok := c.(*ast.Heading); ok && h.Level == 1 {
			return plain(h, src)
		}
	}
	return strings.TrimSuffix(path.Base(name), ".md")
}

func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := c.(*ast.Text); ok && entering {
			b.Write(t.Segment.Value(src))
		}
		if t, ok := c.(*ast.String); ok && entering {
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

type linkDest struct {
	url string
	src bool // a file that isn't a page
}

var lineSuffix = regexp.MustCompile(`:[0-9]+(-[0-9]+)?$`)

// rewriteLink points a link at the HTML page it lands on. problem is set when its target is missing.
func rewriteLink(root, repo string, from *Page, pages map[string]*Page, dest string) (linkDest, string) {
	if dest == "" || strings.HasPrefix(dest, "#") || examples[from.Src+"|"+dest] {
		return linkDest{url: dest}, ""
	}
	u, err := url.Parse(dest)
	if err != nil {
		return linkDest{url: dest}, fmt.Sprintf("link %q can't be read", dest)
	}
	if u.Scheme != "" || u.Host != "" {
		return linkDest{url: dest}, "" // http, mailto…
	}
	target := path.Clean(path.Join(path.Dir(from.Src), u.Path))
	frag := ""
	if u.Fragment != "" {
		frag = "#" + u.Fragment
	}
	if strings.HasPrefix(target, "../") || target == ".." {
		return linkDest{url: dest}, fmt.Sprintf("link %q leaves the repository", dest)
	}
	if pg, ok := dirPages[target]; ok {
		target = pg
	}
	if pg, ok := pages[target]; ok {
		return linkDest{url: rel(from.Out, pg.Out) + frag}, ""
	}
	file := lineSuffix.ReplaceAllString(target, "")
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
		return linkDest{url: dest}, fmt.Sprintf("link %q goes to %s, which doesn't exist", dest, target)
	}
	if repo != "" {
		return linkDest{url: repo + strings.TrimSuffix(file, "/"), src: true}, ""
	}
	return linkDest{url: "#", src: true}, ""
}

// rel is the relative URL from one site file to another.
func rel(from, to string) string {
	fd := strings.Split(path.Dir(from), "/")
	if fd[0] == "." {
		fd = nil
	}
	td := strings.Split(path.Dir(to), "/")
	if td[0] == "." {
		td = nil
	}
	i := 0
	for i < len(fd) && i < len(td) && fd[i] == td[i] {
		i++
	}
	parts := strings.Repeat("../", len(fd)-i) + strings.Join(append(td[i:len(td):len(td)], path.Base(to)), "/")
	return parts
}

func layout(p *Page, pages []*Page, body string) string {
	var nav strings.Builder
	cur := ""
	for _, pg := range pages {
		if pg.Group != cur {
			if cur != "" {
				nav.WriteString("</ul>\n")
			}
			cur = pg.Group
			fmt.Fprintf(&nav, "<h2>%s</h2>\n<ul>\n", stdhtml.EscapeString(cur))
		}
		label := pg.Title
		if pg.Label != "" {
			label = pg.Label
		}
		class := ""
		if pg == p {
			class = ` class="here" aria-current="page"`
		}
		fmt.Fprintf(&nav, "<li><a href=\"%s\"%s>%s</a></li>\n", stdhtml.EscapeString(rel(p.Out, pg.Out)), class, stdhtml.EscapeString(label))
	}
	nav.WriteString("</ul>\n")
	home := rel(p.Out, "index.html")
	pageTitle := p.Title + " · noskill"
	if p.Src == "README.md" {
		pageTitle = "noskill"
	}
	return fmt.Sprintf(pageHTML, stdhtml.EscapeString(pageTitle), rel(p.Out, "style.css"), home, nav.String(), body, stdhtml.EscapeString(p.Src))
}
