package skills

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"noskill/internal/backends"
)

// Embedder is the part of a backend the selector needs.
type Embedder interface {
	Embed(ctx context.Context, req backends.EmbedRequest) ([][]float32, error)
}

// VectorCache stores skill vectors per (name, content hash, embed model), so unchanged skills are
// embedded once (skill_vectors table).
type VectorCache interface {
	GetSkillVector(ctx context.Context, name, hash, model string) ([]float32, bool, error)
	PutSkillVector(ctx context.Context, name, hash, model string, v []float32) error
}

// Selector shortlists skills for a request (FR-S3): cosine similarity of the request against each
// skill's description and trigger phrases, with a keyword fallback when no embedding model is
// available. Choosing among the shortlist is the caller's job (the router for very small models).
type Selector struct {
	Embedder   Embedder    // nil = keyword matching only
	EmbedModel string      // e.g. "nomic-embed-text"
	Cache      VectorCache // optional
	MinScore   float64     // embedding matches below this are dropped; default 0.45
	MinKeyword float64     // keyword matches below this are dropped; default 0.18
	// MinLearned is the keyword floor for learned skills; default 0.6. A learned skill is narrow
	// (one chore) and has an example request to match both ways, so a weak match is a wrong one
	// (block 2c: "combine every .txt" picked filter-error-lines at 0.21).
	MinLearned float64
}

// Match is a skill with its relevance to the request.
type Match struct {
	Skill  *Skill
	Score  float64
	Method string // embedding | keyword | trigger | forced
}

// Select returns up to k matches, best first. Skills the caller may not use (disabled, projects
// awaiting approval) must be left out of candidates by the caller.
func (s *Selector) Select(ctx context.Context, candidates []*Skill, query string, k int) ([]Match, error) {
	if k <= 0 {
		k = 3
	}
	if len(candidates) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	var ms []Match
	method := "keyword"
	if s.Embedder != nil && s.EmbedModel != "" {
		var err error
		ms, err = s.byEmbedding(ctx, candidates, query)
		if err == nil {
			method = "embedding"
		} else {
			// Embeddings are an optimisation: if the model isn't installed or the server hiccups,
			// keyword matching still finds the obvious skills.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			ms = nil
		}
	}
	if ms == nil {
		ms = byKeyword(candidates, query)
	}
	min := s.MinScore
	if min == 0 {
		min = 0.45
	}
	if method == "keyword" {
		min = s.MinKeyword
		if min == 0 {
			min = 0.18
		}
	}
	q := strings.ToLower(query)
	var out []Match
	for _, m := range ms {
		// A trigger phrase appearing in the request is a strong signal either way.
		if hit := triggerHit(m.Skill, q); hit {
			m.Score = math.Min(1, m.Score+0.25)
			if m.Score < min {
				m.Score = min
			}
			m.Method = "trigger"
		} else {
			m.Method = method
		}
		floor := min
		if method == "keyword" && m.Skill.Source == SourceLearned && m.Method != "trigger" {
			floor = s.MinLearned
			if floor == 0 {
				floor = 0.6
			}
		}
		if m.Score >= floor {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Skill.Name < out[j].Skill.Name
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func triggerHit(s *Skill, lowerQuery string) bool {
	for _, t := range s.Meta.Triggers {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" && strings.Contains(lowerQuery, t) {
			return true
		}
	}
	return false
}

// docText is what represents a skill: description plus trigger phrases.
func docText(s *Skill) string {
	d := s.Description
	if len(d) > 1024 {
		d = d[:1024]
	}
	if len(s.Meta.Triggers) > 0 {
		d += " Examples: " + strings.Join(s.Meta.Triggers, "; ")
	}
	return s.Name + ": " + d
}

// nomic-embed-text and its relatives were trained with task prefixes and score worse without them.
func prefixes(model string) (doc, query string) {
	if strings.Contains(strings.ToLower(model), "nomic") {
		return "search_document: ", "search_query: "
	}
	return "", ""
}

func (s *Selector) byEmbedding(ctx context.Context, cands []*Skill, query string) ([]Match, error) {
	docP, qP := prefixes(s.EmbedModel)
	vecs := make([][]float32, len(cands))
	var missing []int
	for i, c := range cands {
		if s.Cache != nil {
			if v, ok, err := s.Cache.GetSkillVector(ctx, c.Name, c.Hash, s.EmbedModel); err == nil && ok {
				vecs[i] = v
				continue
			}
		}
		missing = append(missing, i)
	}
	input := []string{qP + query}
	for _, i := range missing {
		input = append(input, docP+docText(cands[i]))
	}
	out, err := s.Embedder.Embed(ctx, backends.EmbedRequest{Model: s.EmbedModel, Input: input})
	if err != nil {
		return nil, err
	}
	qv := out[0]
	for j, i := range missing {
		vecs[i] = out[j+1]
		if s.Cache != nil {
			s.Cache.PutSkillVector(ctx, cands[i].Name, cands[i].Hash, s.EmbedModel, vecs[i])
		}
	}
	ms := make([]Match, len(cands))
	for i, c := range cands {
		ms[i] = Match{Skill: c, Score: cosine(qv, vecs[i])}
	}
	return ms, nil
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can do does for from how i if in is it me my of on or so
		that the this to up use was what when where which who why will with you your please about into some any
		should would could might must shall than then there their them they these those have has had having been being
		get got make made need needs want wants like just also very more most other another each every all many much
		open new one two via per not no yes ok okay hello thanks thank help tell show give let lets can't dont`) {
		stop[w] = true
	}
}

func tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if len(w) < 2 || stop[w] {
			continue
		}
		out = append(out, stem(w))
	}
	return out
}

// stripPaths drops the paths in a request: a temp folder's segments and file names say nothing
// about the task but would weigh as much as the words that do.
func stripPaths(q string) string {
	f := strings.Fields(q)
	out := f[:0]
	for _, w := range f {
		if !strings.ContainsAny(w, `/\`) {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// exampleOf is the example request in a learned skill's description (learning appends
// ` Example request: "…"`), or "".
func exampleOf(s *Skill) string {
	_, ex, ok := strings.Cut(s.Description, `Example request: "`)
	if !ok {
		return ""
	}
	ex, _, _ = strings.Cut(ex, `"`)
	return ex
}

// stem trims the plural and verb endings that matter most for matching ("searches" ~ "search").
func stem(w string) string {
	for _, suf := range []string{"ing", "es", "ed", "s"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// byKeyword scores by the share of the request's words found in the skill's text, weighted by how
// rare each word is across the candidates.
func byKeyword(cands []*Skill, query string) []Match {
	docs := make([]map[string]bool, len(cands))
	df := map[string]int{}
	for i, c := range cands {
		set := map[string]bool{}
		for _, t := range tokens(docText(c)) {
			set[t] = true
		}
		docs[i] = set
		for t := range set {
			df[t]++
		}
	}
	qt := map[string]bool{}
	for _, t := range tokens(stripPaths(query)) {
		qt[t] = true
	}
	if len(qt) == 0 {
		return nil
	}
	n := float64(len(cands))
	weight := func(t string) float64 { return math.Log(1 + n/float64(1+df[t])) }
	var total float64
	for t := range qt {
		total += weight(t)
	}
	ms := make([]Match, len(cands))
	for i, c := range cands {
		var hit float64
		for t := range qt {
			if docs[i][t] {
				hit += weight(t)
			}
		}
		score := hit / total
		// A skill with an example request must also have most of that example's words in the
		// request: a short request ("How many files are in X?") otherwise matches any skill that
		// mentions files.
		if ex := tokens(exampleOf(c)); len(ex) > 0 {
			var in, all float64
			seen := map[string]bool{}
			for _, t := range ex {
				if seen[t] {
					continue
				}
				seen[t] = true
				all += weight(t)
				if qt[t] {
					in += weight(t)
				}
			}
			score = math.Sqrt(score * in / all)
		}
		ms[i] = Match{Skill: c, Score: score}
	}
	return ms
}
