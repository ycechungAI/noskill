package docs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

	"noskill/internal/backends"
)

// Hit is one passage that matches a question.
type Hit struct {
	File    string  `json:"file"`              // relative to the folder you added, for citations
	Folder  string  `json:"folder"`            // the added folder it is in
	Path    string  `json:"path"`              // full path, for docs_read
	Section string  `json:"section,omitempty"` // nearest Markdown heading
	Start   int     `json:"start"`             // where the passage begins in the file's text (docs_read)
	End     int     `json:"end"`
	Score   float64 `json:"score"` // 0–1, higher is better
	Text    string  `json:"text"`
	// How it was found, for debugging poor results.
	Keywords bool `json:"keywords,omitempty"`
	Meaning  bool `json:"meaning,omitempty"`
}

var stop = map[string]bool{"the": true, "and": true, "for": true, "are": true, "was": true, "were": true, "what": true, "which": true,
	"who": true, "how": true, "when": true, "where": true, "why": true, "does": true, "did": true, "with": true, "that": true,
	"this": true, "from": true, "have": true, "has": true, "had": true, "about": true, "into": true, "your": true, "you": true, "can": true,
	"say": true, "says": true, "tell": true, "any": true, "all": true, "not": true, "but": true, "his": true, "her": true, "its": true}

// ftsQuery turns a question into an FTS5 query: its meaningful words, OR-ed, each quoted so
// punctuation can't be read as syntax. "" when nothing is left.
func ftsQuery(q string) string {
	var words, all []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 2 {
			continue
		}
		all = append(all, w)
		if !stop[w] {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		words = all
	}
	seen := map[string]bool{}
	var parts []string
	for _, w := range words {
		if !seen[w] {
			seen[w] = true
			parts = append(parts, `"`+w+`"`)
		}
	}
	return strings.Join(parts, " OR ")
}

const (
	candidates = 40
	rrfK       = 60.0
)

// Search ranks passages by keywords (BM25) and, when vectors exist for the current embedding model,
// by meaning (cosine), fusing the two rankings by reciprocal rank. folder limits it to one added
// folder ("" = all of them).
func (ix *Index) Search(ctx context.Context, query string, k int, folder string) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("say what to look for")
	}
	if k <= 0 {
		k = 6
	}
	k = min(k, 20)
	lexical := map[int64]int{} // chunk id → rank (1 = best)
	if q := ftsQuery(query); q != "" {
		sqlq := `SELECT c.id FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid JOIN files f ON f.id = c.file_id
			WHERE chunks_fts MATCH ?`
		args := []any{q}
		if folder != "" {
			sqlq += ` AND f.folder = ?`
			args = append(args, folder)
		}
		rows, err := ix.DB.QueryContext(ctx, sqlq+` ORDER BY bm25(chunks_fts) LIMIT ?`, append(args, candidates)...)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		for r := 1; rows.Next(); r++ {
			var id int64
			if rows.Scan(&id) == nil {
				lexical[id] = r
			}
		}
		rows.Close()
	}
	semantic := ix.semantic(ctx, query, folder)

	score := map[int64]float64{}
	for id, r := range lexical {
		score[id] += 1 / (rrfK + float64(r))
	}
	for id, r := range semantic {
		score[id] += 1 / (rrfK + float64(r))
	}
	type ranked struct {
		id int64
		s  float64
	}
	var order []ranked
	for id, s := range score {
		order = append(order, ranked{id, s})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].s != order[j].s {
			return order[i].s > order[j].s
		}
		return order[i].id < order[j].id
	})
	if len(order) > k {
		order = order[:k]
	}
	maxScore := 2 / (rrfK + 1)
	if len(semantic) == 0 {
		maxScore = 1 / (rrfK + 1)
	}
	var out []Hit
	for _, o := range order {
		var h Hit
		var section *string
		err := ix.DB.QueryRowContext(ctx, `SELECT f.rel, f.folder, f.path, c.section, c.start, c.end, c.text
			FROM chunks c JOIN files f ON f.id = c.file_id WHERE c.id = ?`, o.id).Scan(&h.File, &h.Folder, &h.Path, &section, &h.Start, &h.End, &h.Text)
		if err != nil {
			continue
		}
		if section != nil {
			h.Section = *section
		}
		h.Score = round2(o.s / maxScore)
		_, h.Keywords = lexical[o.id]
		_, h.Meaning = semantic[o.id]
		out = append(out, h)
	}
	return out, nil
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// semantic ranks chunks by cosine similarity to the query. It returns nothing when there is no
// embedding model, no vectors for it, or the model fails: keyword search then stands alone.
func (ix *Index) semantic(ctx context.Context, query, folder string) map[int64]int {
	if ix.Embedder == nil || ix.EmbedModel == "" {
		return nil
	}
	q := `SELECT v.chunk_id, v.vector FROM chunk_vectors v JOIN chunks c ON c.id = v.chunk_id JOIN files f ON f.id = c.file_id WHERE v.embed_model = ?`
	args := []any{ix.EmbedModel}
	if folder != "" {
		q += ` AND f.folder = ?`
		args = append(args, folder)
	}
	rows, err := ix.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	type vec struct {
		id int64
		v  []float32
	}
	var vs []vec
	for rows.Next() {
		var id int64
		var b []byte
		if rows.Scan(&id, &b) == nil {
			vs = append(vs, vec{id, decode(b)})
		}
	}
	rows.Close()
	if len(vs) == 0 {
		return nil
	}
	qv, err := ix.Embedder.Embed(ctx, backends.EmbedRequest{Model: ix.EmbedModel, Input: []string{query}})
	if err != nil || len(qv) != 1 {
		return nil
	}
	type sc struct {
		id int64
		s  float64
	}
	var scored []sc
	for _, v := range vs {
		if s := cosine(qv[0], v.v); s > 0.2 { // below this nothing is related
			scored = append(scored, sc{v.id, s})
		}
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].s > scored[j].s })
	if len(scored) > candidates {
		scored = scored[:candidates]
	}
	out := map[int64]int{}
	for r, s := range scored {
		out[s.id] = r + 1
	}
	return out
}

// ReadText returns up to max characters of a file's text from start (a byte offset into the text
// that Extract returns, as in Hit.Start), along with the total length, so the next call can
// continue. The file must be inside an added folder.
func (ix *Index) ReadText(ctx context.Context, path string, start, max int) (text string, total int, err error) {
	if _, real, err := ix.Within(ctx, path); err != nil {
		return "", 0, err
	} else {
		path = real
	}
	if _, err := os.Stat(path); err != nil {
		return "", 0, err
	}
	full, err := Extract(path)
	if err != nil {
		return "", 0, err
	}
	if max <= 0 {
		max = 4000
	}
	max = min(max, 20000)
	start = clampStart(full, start)
	end := start + max
	if end >= len(full) {
		end = len(full)
	} else {
		for end < len(full) && !runeStart(full[end]) {
			end++
		}
	}
	return full[start:end], len(full), nil
}

func clampStart(s string, start int) int {
	if start < 0 {
		start = 0
	}
	if start > len(s) {
		start = len(s)
	}
	for start < len(s) && !runeStart(s[start]) {
		start++
	}
	return start
}

func runeStart(b byte) bool { return b&0xC0 != 0x80 }
