package deepresearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type run struct {
	d       Deps
	cfg     Config
	st      *State
	started time.Time
	hostN   map[string]int // pages read per site
	srcURL  map[string]int // address → source id
	fails   int            // searches in a row that failed
}

// Run researches topic and writes the report. A state file in cfg.Dir (when set) lets a stopped run
// continue where it was: questions already answered aren't asked again.
func Run(ctx context.Context, topic string, d Deps, cfg Config) (Result, error) {
	cfg = cfg.withDefaults()
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return Result{}, errors.New("deepresearch: no topic")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Wait == nil {
		d.Wait = func(ctx context.Context, w time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w):
				return nil
			}
		}
	}
	r := &run{d: d, cfg: cfg, started: d.Now(), hostN: map[string]int{}, srcURL: map[string]int{}}
	r.st = r.load(topic)
	for _, s := range r.st.Sources {
		r.hostN[s.Host]++
		r.srcURL[s.URL] = s.ID
	}
	if len(r.st.Questions) == 0 {
		r.scope(ctx)
	}
	for round := r.st.RoundsDone + 1; round <= cfg.Rounds && ctx.Err() == nil && r.over() == ""; round++ {
		r.round(ctx, round)
		if r.st.Stopped != "" || ctx.Err() != nil {
			break
		}
		r.st.RoundsDone = round
		r.save()
		if round < cfg.Rounds {
			if added := r.findGaps(ctx, round); added == 0 && len(r.open()) == 0 {
				r.say("gaps", round, 0, 0, "nothing left to look for: stopping after round %d", round)
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		r.save()
		return Result{}, err
	}
	if r.st.Stats.ClaimsVerified == 0 {
		why := "no claim could be backed by a quote from a page that was read: is the network reachable and a search provider working?"
		if r.st.Stopped == "search" {
			why = "web search keeps failing, so nothing could be researched. Try again later, or set a search provider that works from here (BRAVE_API_KEY, or searxng_url in [skills.web-search])."
		}
		return Result{State: *r.st}, errors.New(why)
	}
	return r.write(ctx)
}

func (r *run) say(stage string, round, done, total int, format string, args ...any) {
	if r.d.Progress != nil {
		r.d.Progress(Event{Stage: stage, Round: round, Done: done, Total: total, Message: fmt.Sprintf(format, args...)})
	}
}

// over says why the run has to stop now, or "".
func (r *run) over() string {
	switch {
	case r.st.Stopped != "":
		return r.st.Stopped
	case r.d.Now().Sub(r.started) >= r.cfg.Budget:
		r.st.Stopped = "time"
	case r.st.Stats.Calls >= r.cfg.MaxCalls:
		r.st.Stopped = "calls"
	case r.st.Stats.PagesRead >= r.cfg.MaxPages:
		r.st.Stopped = "pages"
	}
	return r.st.Stopped
}

func (r *run) ask(ctx context.Context, system, user string, schema json.RawMessage, n int) (string, error) {
	if r.over() != "" {
		return "", errors.New("over budget")
	}
	r.st.Stats.Calls++
	out, err := r.d.LLM.Ask(ctx, system, user, schema, n)
	return strings.TrimSpace(out), err
}

// ---- state ----

func (r *run) path() string {
	if r.cfg.Dir == "" {
		return ""
	}
	return filepath.Join(r.cfg.Dir, "state.json")
}

func (r *run) load(topic string) *State {
	st := &State{Topic: topic, Started: r.d.Now()}
	if p := r.path(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var old State
			if json.Unmarshal(b, &old) == nil && strings.EqualFold(old.Topic, topic) {
				old.Stopped = "" // a resumed run has a fresh budget
				return &old
			}
		}
	}
	return st
}

func (r *run) save() {
	p := r.path()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(r.st, "", " ")
	if err != nil || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p)
	}
}

// ---- scope ----

var scopeFallback = []string{
	"What is %s, and how is it defined?",
	"How does %s work in practice?",
	"What does the evidence say about %s?",
	"What are the main criticisms or limits of %s?",
	"What are the costs, risks or trade-offs of %s?",
	"How did %s develop, and what changed recently?",
}

func (r *run) scope(ctx context.Context) {
	r.say("scope", 0, 0, 0, "planning the questions")
	out, err := r.ask(ctx, `Break this research topic into 5 or 6 specific questions that together would let someone write a thorough report: what it is, how it works, the evidence, the disagreements and criticisms, costs or risks, and recent change. One sentence each. Reply with JSON {"questions": ["...", "..."]}.`,
		"Topic: "+r.st.Topic, json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":3,"maxItems":8,"items":{"type":"string"}}},"required":["questions"]}`), 400)
	var p struct{ Questions []string }
	if err != nil || json.Unmarshal([]byte(out), &p) != nil {
		p.Questions = nil
	}
	for _, q := range p.Questions {
		if q = oneSentence(q); q != "" && !similarQuestion(q, r.st.Questions, r.st.Topic) {
			r.addQuestion(q, 1, "")
		}
	}
	if len(r.st.Questions) < 3 {
		for _, t := range scopeFallback {
			q := fmt.Sprintf(t, r.st.Topic)
			if !similarQuestion(q, r.st.Questions, r.st.Topic) {
				r.addQuestion(q, 1, "")
			}
		}
	}
	r.save()
}

func (r *run) addQuestion(text string, round int, kind string) int {
	id := len(r.st.Questions) + 1
	r.st.Questions = append(r.st.Questions, Question{ID: id, Text: text, Round: round, Kind: kind, Status: "open"})
	return id
}

func oneSentence(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if i := strings.Index(q, "?"); i > 0 {
		q = q[:i+1]
	}
	return clip(q, 220)
}

func (r *run) open() []int {
	var ids []int
	for i, q := range r.st.Questions {
		if q.Status == "open" {
			ids = append(ids, i)
		}
	}
	return ids
}

// ---- a round ----

func (r *run) round(ctx context.Context, round int) {
	ids := r.open()
	if len(ids) > r.cfg.QuestionsPerRound {
		ids = ids[:r.cfg.QuestionsPerRound]
	}
	for n, i := range ids {
		if ctx.Err() != nil || r.over() != "" {
			return
		}
		q := &r.st.Questions[i]
		r.say("search", round, n+1, len(ids), "%s", q.Text)
		r.research(ctx, q, round)
		r.save()
		if r.fails >= 4 {
			r.st.Stopped = "search"
			r.say("search", round, n+1, len(ids), "the search failed %d times in a row; stopping here", r.fails)
			return
		}
	}
	r.followLeads(ctx, round)
}

func (r *run) research(ctx context.Context, q *Question, round int) {
	q.Tries++
	before := q.Claims
	if r.d.Local != nil {
		if pages, err := r.d.Local.Search(ctx, q.Text, 3); err == nil {
			for _, p := range pages {
				r.readPage(ctx, q, round, p, false, true)
			}
		}
	}
	query := r.query(ctx, q)
	hits, err := r.search(ctx, query)
	if err != nil {
		if ctx.Err() == nil {
			r.st.Stats.SearchFailures++
			r.fails++
		}
	} else {
		r.fails = 0
		read := 0
		for _, h := range r.rank(hits) {
			if read >= r.cfg.PagesPerQuestion || ctx.Err() != nil || r.over() != "" {
				break
			}
			pg, err := r.d.Fetch.Fetch(ctx, h.URL)
			if err != nil || len([]rune(pg.Text)) < 300 {
				continue
			}
			if pg.URL == "" {
				pg.URL = h.URL
			}
			if pg.Title == "" {
				pg.Title = h.Title
			}
			r.readPage(ctx, q, round, pg, false, false)
			read++
		}
	}
	if q.Claims > before {
		q.Status = "done"
	} else {
		q.Status = "empty"
	}
}

// query writes the search words for a question; a later try asks for different words.
func (r *run) query(ctx context.Context, q *Question) string {
	sys := "Write a web search query for this question: 3 to 8 keywords, not a sentence. Reply with the query only."
	if q.Tries > 1 {
		sys = "The first search for this question found nothing useful. Write a different web search query for it, with other words: 3 to 8 keywords, not a sentence. Reply with the query only."
	}
	out, err := r.ask(ctx, sys, "Topic: "+r.st.Topic+"\nQuestion: "+q.Text, nil, 40)
	out = strings.Trim(strings.TrimSpace(out), "\"'`")
	if err != nil || out == "" || strings.Contains(out, "\n") {
		return q.Text
	}
	return out
}

func (r *run) search(ctx context.Context, query string) ([]Hit, error) {
	var err error
	for _, wait := range r.cfg.SearchWaits {
		if wait > 0 {
			if werr := r.d.Wait(ctx, wait); werr != nil {
				return nil, werr
			}
		}
		r.st.Stats.Searches++
		var hits []Hit
		if hits, err = r.d.Search.Search(ctx, query); err == nil {
			return hits, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

// rank orders results the research may read: pages not read yet, from sites that haven't had their
// share, best source first (ties keep the search's order).
func (r *run) rank(hits []Hit) []Hit {
	var out []Hit
	seen := map[string]bool{}
	for _, h := range hits {
		host := hostOf(h.URL)
		if h.URL == "" || r.srcURL[h.URL] != 0 || seen[h.URL] || r.hostN[host] >= r.cfg.MaxPerHost || sourceScore(h.URL) <= -4 {
			continue
		}
		seen[h.URL] = true
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return sourceScore(out[i].URL) > sourceScore(out[j].URL) })
	return out
}

// readPage takes the passages of a page that bear most on the question and asks the model for the
// facts in each; only claims whose quote checks out join the ledger.
func (r *run) readPage(ctx context.Context, q *Question, round int, pg Page, lead, local bool) {
	if r.srcURL[pg.URL] != 0 {
		return
	}
	src := Source{ID: len(r.st.Sources) + 1, URL: pg.URL, Title: pg.Title, Host: hostOf(pg.URL), Fetched: r.d.Now(), Local: local, Lead: lead}
	r.st.Sources = append(r.st.Sources, src)
	r.srcURL[pg.URL] = src.ID
	r.hostN[src.Host]++
	r.st.Stats.PagesRead++
	picks := bestPassages(passages(pg.Text, 1200), q.Text, r.st.Topic, 2)
	got := 0
	for _, p := range picks {
		if r.over() != "" {
			break
		}
		got += r.extract(ctx, q, round, src.ID, p, pg.Text)
	}
	r.say("read", round, 0, 0, "%s: %d verified %s", clip(pg.URL, 70), got, plural(got, "claim", "claims"))
	if got > 0 && !local {
		links := pg.Links
		if len(links) == 0 {
			links = LinksIn(pg.Text)
		}
		r.addLeads(q, pg.URL, links)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var claimSchema = json.RawMessage(`{"type":"object","properties":{"claims":{"type":"array","maxItems":3,"items":{"type":"object","properties":{"claim":{"type":"string"},"quote":{"type":"string"}},"required":["claim","quote"]}}},"required":["claims"]}`)

// extract asks for the facts one passage gives and verifies each against the whole page.
func (r *run) extract(ctx context.Context, q *Question, round, source int, passage, page string) int {
	out, err := r.ask(ctx, `You read one passage and list the facts in it that help answer the question. Reply with JSON {"claims": [{"claim": "the fact as one plain sentence", "quote": "words copied exactly from the passage that state the fact"}]}. At most 3 claims. Only facts the passage states; copy the quote exactly, 10 to 40 words. If the passage has nothing useful, reply {"claims": []}.`,
		"Question: "+q.Text+"\n\nPassage:\n"+passage, claimSchema, 500)
	if err != nil {
		return 0
	}
	var p struct {
		Claims []struct{ Claim, Quote string }
	}
	if json.Unmarshal([]byte(out), &p) != nil {
		return 0
	}
	n := 0
	for _, c := range p.Claims {
		r.st.Stats.ClaimsProposed++
		ok, _ := verifyClaim(c.Claim, c.Quote, page)
		if !ok || r.dup(c.Claim) {
			r.st.Stats.ClaimsDropped++
			continue
		}
		r.st.Claims = append(r.st.Claims, Claim{ID: len(r.st.Claims) + 1, Text: strings.TrimSpace(c.Claim), Quote: strings.TrimSpace(c.Quote),
			Source: source, Question: q.ID, Round: round, Support: 1})
		r.st.Stats.ClaimsVerified++
		q.Claims++
		n++
	}
	return n
}

func (r *run) dup(text string) bool {
	n := norm(text)
	for _, c := range r.st.Claims {
		if norm(c.Text) == n {
			return true
		}
	}
	return false
}

// ---- leads: links from good pages to primary sources ----

func (r *run) addLeads(q *Question, from string, links []Link) {
	topic := tokenSet(r.st.Topic + " " + q.Text)
	fromHost := hostOf(from)
	added := 0
	for _, l := range links {
		h := hostOf(l.URL)
		if h == fromHost || r.srcURL[l.URL] != 0 || sourceScore(l.URL) <= -4 || r.hostN[h] >= r.cfg.MaxPerHost || added >= 2 {
			continue
		}
		dup := false
		for _, e := range r.st.Leads {
			if e.URL == l.URL {
				dup = true
			}
		}
		if dup || (overlap(tokenSet(l.Text), topic) == 0 && sourceScore(l.URL) < 2) {
			continue // not about the topic and not from a primary-looking site
		}
		r.st.Leads = append(r.st.Leads, Lead{URL: l.URL, Text: l.Text, Question: q.ID, From: from})
		added++
	}
}

func (r *run) followLeads(ctx context.Context, round int) {
	sort.SliceStable(r.st.Leads, func(i, j int) bool { return sourceScore(r.st.Leads[i].URL) > sourceScore(r.st.Leads[j].URL) })
	n := 0
	var rest []Lead
	for _, l := range r.st.Leads {
		if n >= r.cfg.LeadsPerRound || ctx.Err() != nil || r.over() != "" {
			rest = append(rest, l)
			continue
		}
		pg, err := r.d.Fetch.Fetch(ctx, l.URL)
		if err != nil || len([]rune(pg.Text)) < 300 {
			continue
		}
		if pg.URL == "" {
			pg.URL = l.URL
		}
		if pg.Title == "" {
			pg.Title = l.Text
		}
		r.st.Stats.LeadsFollowed++
		n++
		q := &r.st.Questions[l.Question-1]
		before := q.Claims
		r.readPage(ctx, q, round, pg, true, false)
		if q.Claims > before && q.Status != "done" {
			q.Status = "done"
		}
	}
	r.st.Leads = rest
}

// ---- gaps ----

// findGaps turns what the round left unanswered into the next round's questions and returns how many it added.
func (r *run) findGaps(ctx context.Context, round int) int {
	r.say("gaps", round, 0, 0, "looking for what is missing")
	added := 0
	// Questions that found nothing get another try with other words.
	for i := range r.st.Questions {
		q := &r.st.Questions[i]
		if q.Status == "empty" && q.Tries < 2 {
			q.Status = "open"
			added++
		}
	}
	// Claims only one site makes are asked about again, to find a second source or a counter-claim.
	Corroborate(r.st.Claims, r.st.Sources)
	checked := 0
	for _, c := range r.st.Claims {
		if checked == 2 {
			break
		}
		if c.Support == 1 && c.Round == round {
			t := "Is this confirmed or disputed elsewhere: " + clip(c.Text, 160)
			if !similarQuestion(t, r.st.Questions, r.st.Topic) {
				r.addQuestion(t, round+1, "check")
				added++
				checked++
			}
		}
	}
	// The model reads the ledger and names what is still missing.
	var b strings.Builder
	for _, q := range r.st.Questions {
		fmt.Fprintf(&b, "- %s (%d facts)\n", clip(q.Text, 120), q.Claims)
	}
	b.WriteString("\nFacts so far:\n")
	for i, c := range r.st.Claims {
		if i >= 25 {
			break
		}
		fmt.Fprintf(&b, "- %s\n", clip(c.Text, 150))
	}
	out, err := r.ask(ctx, `You are checking a research for gaps. From the questions and the facts so far, write up to 2 new questions about important things the research has not covered yet: a side of the topic, a number, a date, a counter-argument. Reply with JSON {"gaps": ["...", "..."]}; reply {"gaps": []} if it is well covered.`,
		"Topic: "+r.st.Topic+"\n\nQuestions asked:\n"+b.String(), json.RawMessage(`{"type":"object","properties":{"gaps":{"type":"array","maxItems":2,"items":{"type":"string"}}},"required":["gaps"]}`), 250)
	var g struct{ Gaps []string }
	if err == nil && json.Unmarshal([]byte(out), &g) == nil {
		for _, t := range g.Gaps {
			if t = oneSentence(t); t != "" && !similarQuestion(t, r.st.Questions, r.st.Topic) {
				r.addQuestion(t, round+1, "gap")
				added++
			}
		}
	}
	r.save()
	return added
}
