package docs

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Chunk sizes are in characters, tuned for small models (8k context): about 400 tokens a chunk, so
// six hits fit beside the question. The design note said ~800 tokens; this is the deviation.
const (
	ChunkChars   = 1600
	ChunkOverlap = 200
	maxPara      = 2400 // longer paragraphs are cut at sentence ends
)

// Chunk is a slice of a file's text: [Start, End) are byte offsets into the extracted text, so
// docs_read can show the surroundings of a hit.
type Chunk struct {
	Start, End int
	Text       string
	Section    string // nearest Markdown heading above the chunk, for citations
}

var headingRe = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t#]*$`)

type span struct{ start, end int }

// Split cuts text into chunks of about ChunkChars at paragraph edges, each overlapping the previous
// by ChunkOverlap characters so a sentence on the boundary is whole in one of them.
func Split(text string, markdown bool) []Chunk {
	var paras []span
	pos := 0
	for pos < len(text) {
		end := strings.Index(text[pos:], "\n\n")
		if end < 0 {
			end = len(text) - pos
		}
		p := span{pos, pos + end}
		if strings.TrimSpace(text[p.start:p.end]) != "" {
			paras = append(paras, cut(text, p)...)
		}
		pos += end + 2
	}
	var heads []struct {
		at   int
		text string
	}
	if markdown {
		for _, m := range headingRe.FindAllStringSubmatchIndex(text, -1) {
			heads = append(heads, struct {
				at   int
				text string
			}{m[0], text[m[2]:m[3]]})
		}
	}
	section := func(at int) string {
		s := ""
		for _, h := range heads {
			if h.at > at {
				break
			}
			s = h.text
		}
		return s
	}
	var out []Chunk
	// A chunk's section is the heading in force where its body text begins, so a chunk that opens
	// with "# Title" then "## Part" is cited under "Part".
	flush := func(start, end, bodyAt int) {
		if t := strings.TrimSpace(text[start:end]); t != "" {
			out = append(out, Chunk{Start: start, End: end, Text: t, Section: section(max(bodyAt, start))})
		}
	}
	// A Markdown heading starts a new chunk (once the current one has body text), so a chunk belongs
	// to one section and its citation names the right heading.
	isHeading := func(p span) bool { return markdown && strings.HasPrefix(text[p.start:p.end], "#") }
	start, end, bodyAt := -1, 0, -1
	for _, p := range paras {
		switch {
		case start >= 0 && isHeading(p) && bodyAt >= 0:
			flush(start, end, bodyAt)
			start, bodyAt = p.start, -1
		case start >= 0 && p.end-start > ChunkChars:
			flush(start, end, bodyAt)
			// The next chunk starts a little before this paragraph, on a word edge.
			start, bodyAt = overlapStart(text, end, p.start), p.start
		}
		if start < 0 {
			start = p.start
		}
		end = p.end
		if bodyAt < 0 && !isHeading(p) {
			bodyAt = p.start
		}
	}
	if start >= 0 {
		flush(start, end, bodyAt)
	}
	return out
}

// overlapStart returns where the next chunk begins: up to ChunkOverlap characters before from,
// moved forward to a space, but never before the previous chunk's content or after floor.
func overlapStart(text string, from, floor int) int {
	s := max(from-ChunkOverlap, 0)
	for s < from && !utf8.RuneStart(text[s]) {
		s++
	}
	if i := strings.IndexAny(text[s:from], " \n"); i >= 0 && s+i+1 < from {
		s += i + 1
	}
	if s > floor {
		s = floor
	}
	return s
}

var sentenceEnd = regexp.MustCompile(`[.!?。！？]["')\]]*\s+`)

// cut splits a paragraph longer than maxPara at sentence ends (or, failing that, at spaces).
func cut(text string, p span) []span {
	if p.end-p.start <= maxPara {
		return []span{p}
	}
	var out []span
	start := p.start
	for p.end-start > maxPara {
		window := text[start : start+maxPara]
		cutAt := -1
		for _, m := range sentenceEnd.FindAllStringIndex(window, -1) {
			if m[1] >= maxPara/3 {
				cutAt = m[1]
			}
		}
		if cutAt < 0 {
			cutAt = strings.LastIndexByte(window, ' ') + 1
		}
		if cutAt <= 0 {
			cutAt = maxPara
		}
		for start+cutAt < p.end && !utf8.RuneStart(text[start+cutAt]) {
			cutAt++
		}
		out = append(out, span{start, start + cutAt})
		start += cutAt
	}
	return append(out, span{start, p.end})
}
