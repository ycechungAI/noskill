package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"noskill/internal/backends"
	"noskill/internal/tools"
	"noskill/internal/tools/builtin"
)

// STORM research (skills/storm, docs/LEARNING.md): the pipeline runs here, in Go, and the model is
// given one small job at a time. A 3B model asked to follow a 60-step procedure skipped it (the
// first live check made six calls and wrote one answer), so the loop over viewpoints, questions,
// notes, merging and writing is not left to it.

type stormPersona struct{ name, focus string }

var stormPersonas = []stormPersona{
	{"practitioner", "real-world use, how it is done, what works in practice"},
	{"academic", "theory, careful definitions, the big picture"},
	{"skeptic", "weak evidence, hidden assumptions, overconfident claims"},
	{"economist", "incentives, costs, trade-offs, wider effects"},
	{"historian", "how it developed over time"},
}

// stormFallback are the questions a viewpoint asks when the model can't plan them: %s is the topic.
var stormFallback = map[string][]string{
	"practitioner": {"How is %s used in practice, and what works?", "What goes wrong when people actually use %s?", "What do experienced users of %s recommend?", "What does it take to get started with %s?", "What are the common mistakes with %s?"},
	"academic":     {"How is %s defined, and what theory explains it?", "What does the research say about %s?", "What are the main open questions about %s?", "How is %s measured or evaluated?", "How does %s relate to neighbouring ideas?"},
	"skeptic":      {"What is overstated about %s?", "What is the weakest evidence for %s?", "What assumptions does %s rest on?", "Who disagrees about %s, and why?", "What would show that %s does not work?"},
	"economist":    {"What does %s cost, and who pays?", "What are the trade-offs of %s?", "Who gains and who loses from %s?", "What incentives shape %s?", "What are the wider effects of %s?"},
	"historian":    {"How did %s develop over time?", "What came before %s?", "What turning points shaped %s?", "How has the view of %s changed?", "What was %s like at the start?"},
}

var (
	stormURL  = regexp.MustCompile(`https?://[^\s<>"')\]]+`)
	stormCite = regexp.MustCompile(`\[(\d+)\]`)
)

// stormSearchWaits are the pauses before each try of a search that failed (a provider that just
// failed is skipped for a minute, so the retry waits it out); stormPace is the pause after every
// search, to stay polite to the provider. stormGiveUp is how many questions in a row may lose their
// search before the research stops. Tests set the first two to zero.
var (
	stormSearchWaits = []time.Duration{0, 65 * time.Second}
	stormPace        = time.Second
	stormGiveUp      = 4
)

type stormRun struct {
	s                     *runServices
	topic                 string
	n                     int
	dir                   string
	answered, unsupported int
	seenQuestions         []map[string]bool // the words of every question asked so far
	searchFails           int               // questions in a row whose search failed
}

// asked says whether a question is (nearly) one already asked, by another viewpoint, and remembers
// it when it isn't: small models plan the same question for several viewpoints.
func (st *stormRun) asked(q string) bool {
	// Every question mentions the topic: compare what is left, or all of them would look alike.
	w := words(q)
	for k := range words(st.topic) {
		delete(w, k)
	}
	if len(w) < 2 {
		return false
	}
	for _, o := range st.seenQuestions {
		common := 0
		for k := range w {
			if o[k] {
				common++
			}
		}
		if union := len(w) + len(o) - common; union > 0 && float64(common)/float64(union) >= 0.7 {
			return true
		}
	}
	st.seenQuestions = append(st.seenQuestions, w)
	return false
}

func (st *stormRun) say(format string, args ...any) {
	if w := st.s.a.Opts.Stderr; w != nil {
		fmt.Fprintf(w, "storm: "+format+"\n", args...)
	}
}

// Storm runs the whole research and returns the report folder's files.
func (s *runServices) Storm(ctx context.Context, topic string, perViewpoint int) (*tools.StormResult, error) {
	if perViewpoint <= 0 {
		perViewpoint = 3
	}
	base, err := s.NotesDir(s.runID)
	if err != nil {
		return nil, err
	}
	st := &stormRun{s: s, topic: strings.TrimSpace(topic), n: min(perViewpoint, 5), dir: base + "/storm"}
	for _, p := range stormPersonas {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		qs := st.questions(ctx, p)
		if p.name != "practitioner" {
			if c := st.crossCheck(ctx, p); c != "" {
				qs = append(qs, c)
			}
		}
		for i, q := range qs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if st.asked(q) {
				st.say("%s %d/%d: skipped, asked already: %s", p.name, i+1, len(qs), q)
				continue
			}
			st.say("%s %d/%d: %s", p.name, i+1, len(qs), q)
			st.research(ctx, p, q)
			if st.searchFails >= stormGiveUp {
				break
			}
		}
		if st.searchFails >= stormGiveUp {
			st.say("the search failed for %d questions in a row; stopping the research here", st.searchFails)
			break
		}
		if p.name == "skeptic" {
			for i, q := range st.claimsToCheck(ctx) {
				if st.asked(q) {
					continue
				}
				st.say("skeptic check %d: %s", i+1, q)
				st.research(ctx, p, q)
			}
		}
	}
	if st.answered == 0 {
		why := "no question could be answered from a page that was fetched: is the network reachable?"
		if st.searchFails > 0 {
			why = "web search keeps failing, so nothing could be researched. Try again later, or set a search provider that works from here (BRAVE_API_KEY, or searxng_url in [skills.web-search])."
		}
		return nil, errors.New(why + " (the notes are kept in the run's folder)")
	}
	return st.write(ctx)
}

// ---- model jobs ----

func (st *stormRun) chat(ctx context.Context, system, user string, schema json.RawMessage, num int) (string, error) {
	no, temp := false, 0.2
	req := backends.ChatRequest{Model: st.s.model.Name, Think: &no,
		Messages: []backends.Message{{Role: backends.RoleSystem, Content: system}, {Role: backends.RoleUser, Content: user}},
		Options:  backends.Options{NumCtx: st.s.a.numCtx(st.s.model), NumPredict: num, Temperature: &temp}}
	if schema != nil {
		req.Format = schema
	}
	stream, err := st.s.model.NB.B.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	r, err := backends.Collect(stream, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Content), nil
}

func (st *stormRun) questions(ctx context.Context, p stormPersona) []string {
	goal := fmt.Sprintf("%s: the questions a %s would ask, about %s", st.topic, p.name, p.focus)
	qs, err := st.s.PlanQuestions(ctx, goal, st.n)
	if err != nil || len(qs) == 0 {
		for _, t := range stormFallback[p.name] {
			if len(qs) < st.n {
				qs = append(qs, fmt.Sprintf(t, st.topic))
			}
		}
	}
	for i := range qs {
		qs[i] = shortQuestion(qs[i])
	}
	return qs
}

// digest is the first lines of every lane so far, short enough for a small model's context.
func (st *stormRun) digest(perLane, lineChars int) string {
	var b strings.Builder
	for _, lane := range builtin.NoteLanes(st.dir) {
		fmt.Fprintf(&b, "%s:\n", lane)
		for i, l := range builtin.ReadLane(st.dir, lane) {
			if i >= perLane {
				break
			}
			fmt.Fprintf(&b, "- %s\n", clipRunes(stormURL.ReplaceAllString(l, ""), lineChars))
		}
	}
	return b.String()
}

func (st *stormRun) crossCheck(ctx context.Context, p stormPersona) string {
	d := st.digest(3, 220)
	if strings.TrimSpace(d) == "" {
		return ""
	}
	out, err := st.chat(ctx, `Read these research notes. Pick one claim that rests on a single source or looks doubtful, and write one short question that would check it. Reply with JSON {"question": "..."}.`,
		"Topic: "+st.topic+"\n\n"+d, json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`), 120)
	var r struct{ Question string }
	if err != nil || json.Unmarshal([]byte(out), &r) != nil {
		return ""
	}
	return shortQuestion(r.Question)
}

func (st *stormRun) claimsToCheck(ctx context.Context) []string {
	out, err := st.chat(ctx, `List the 3 most important factual claims in these research notes, each as a short question that would find a second independent source or a counter-claim. Reply with JSON {"questions": ["...", "...", "..."]}.`,
		"Topic: "+st.topic+"\n\n"+st.digest(4, 220), json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"string"}}},"required":["questions"]}`), 250)
	var r struct{ Questions []string }
	if err != nil || json.Unmarshal([]byte(out), &r) != nil {
		return nil
	}
	if len(r.Questions) > 3 {
		r.Questions = r.Questions[:3]
	}
	for i := range r.Questions {
		r.Questions[i] = shortQuestion(r.Questions[i])
	}
	return r.Questions
}

// ---- research: Go searches and fetches; the model only reads ----

// stormSkip are sites that orient a search but are never a source (encyclopedias).
var stormSkip = []string{"wikipedia.org", "britannica.com", "baike.baidu.com", "wikiwand.com", "fandom.com"}

type webResult struct{ Title, URL, Snippet string }

// webEnv is the environment of the web-search skill's tools: its network permission, its settings
// and the secrets it declares, so a search here behaves as one made by the skill.
func (a *App) webEnv(runID string) (*tools.Env, error) {
	sk, ok := a.Skills.Get("web-search")
	if !ok {
		return nil, errors.New("the web-search skill isn't available")
	}
	set, err := tools.SkillPerms(a.resolver(), sk)
	if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	for _, sec := range sk.Meta.Permissions.SecretList() {
		declared[sec.Name] = true
	}
	return &tools.Env{RunID: runID, Skill: sk, Perms: set,
		Secret: func(name string) string {
			if !declared[name] {
				return ""
			}
			v, _ := a.Secrets.Get(name)
			return v
		},
		Setting: func(k string) (string, bool) { return a.userSetting("web-search", k) }}, nil
}

func (st *stormRun) tool(ctx context.Context, id string, args any) (string, error) {
	b, ok := st.s.a.Registry[id]
	if !ok {
		return "", fmt.Errorf("the tool %s isn't available", id)
	}
	env, err := st.s.a.webEnv(st.s.runID)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(args)
	return b.Run(ctx, env, raw)
}

func (st *stormRun) search(ctx context.Context, query string) ([]webResult, error) {
	out, err := st.tool(ctx, "web.search", map[string]any{"query": query, "max_results": 8})
	if err != nil {
		return nil, err
	}
	var list []webResult
	if json.Unmarshal([]byte(out), &list) != nil {
		var wrap struct{ Results []webResult }
		if json.Unmarshal([]byte(out), &wrap) != nil {
			return nil, errors.New("the search gave an answer noskill couldn't read")
		}
		list = wrap.Results
	}
	return list, nil
}

func (st *stormRun) searchRetry(ctx context.Context, query string) ([]webResult, error) {
	var err error
	for _, wait := range stormSearchWaits {
		if wait > 0 {
			st.say("the search failed (%v); trying again in %s", err, wait)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		var r []webResult
		if r, err = st.search(ctx, query); err == nil {
			select {
			case <-ctx.Done():
			case <-time.After(stormPace):
			}
			return r, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

func (st *stormRun) fetch(ctx context.Context, url string) (string, error) {
	out, err := st.tool(ctx, "web.fetch", map[string]any{"url": url, "max_chars": 2400})
	if err != nil {
		return "", err
	}
	var p struct {
		Text string `json:"text_markdown"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return "", err
	}
	return strings.TrimSpace(p.Text), nil
}

// shortQuestion keeps a planned question to one sentence: small models write several, with
// "(e.g., …)" asides.
func shortQuestion(q string) string {
	q = regexp.MustCompile(`\s*\([^)]*\)`).ReplaceAllString(q, "")
	if i := strings.Index(q, "?"); i > 0 {
		q = q[:i+1]
	}
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > 200 {
		q = string(r[:200])
		if i := strings.LastIndex(q, " "); i > 100 {
			q = q[:i]
		}
	}
	return q
}

func skipHost(u string) bool {
	for _, s := range stormSkip {
		if strings.Contains(strings.ToLower(u), s) {
			return true
		}
	}
	return false
}

// research answers one question from pages Go fetched, and keeps only those pages as sources.
func (st *stormRun) research(ctx context.Context, p stormPersona, q string) {
	note := func(text string) {
		if err := builtin.AppendNote(st.dir, p.name, text); err != nil {
			st.say("%v", err)
		}
	}
	none := func(why string) {
		st.unsupported++
		note(fmt.Sprintf("Q: %s A: no reliable source found%s", q, why))
	}
	query, err := st.chat(ctx, "Write a web search query for this question: 3 to 8 keywords, not a sentence. Reply with the query only.", "Topic: "+st.topic+"\nQuestion: "+q, nil, 40)
	if query = strings.Trim(strings.TrimSpace(query), "\"'`"); err != nil || query == "" {
		query = q
	}
	results, err := st.searchRetry(ctx, query)
	if err != nil {
		if ctx.Err() == nil {
			st.searchFails++
			none(fmt.Sprintf(" (the search failed: %v)", err))
		}
		return
	}
	st.searchFails = 0
	type page struct{ url, text string }
	var pages []page
	seen := map[string]bool{}
	for _, r := range results {
		if len(pages) == 3 {
			break
		}
		host := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(r.URL, "https://"), "http://"), "/", 2)[0]
		if r.URL == "" || skipHost(r.URL) || seen[host] {
			continue
		}
		seen[host] = true
		text, err := st.fetch(ctx, r.URL)
		if ctx.Err() != nil {
			return
		}
		if err != nil || len([]rune(text)) < 300 {
			continue
		}
		pages = append(pages, page{r.URL, clipRunes(text, 1800)})
	}
	if len(pages) == 0 {
		none(" (no page could be fetched)")
		return
	}
	var b strings.Builder
	for i, pg := range pages {
		fmt.Fprintf(&b, "[%d] %s\n%s\n\n", i+1, pg.url, pg.text)
	}
	ans, err := st.chat(ctx, "Answer the question using ONLY the numbered pages below. Write 2 to 4 sentences. After each fact put the number of the page it came from, like [2]. If the pages do not answer the question, reply exactly: no reliable source found",
		"Question: "+q+"\n\nPages:\n"+b.String(), nil, 350)
	ans = strings.Join(strings.Fields(ans), " ")
	if err != nil || ans == "" || strings.Contains(strings.ToLower(ans), "no reliable source found") {
		none("")
		return
	}
	// Page numbers become the pages' addresses; a number that isn't a page is dropped. If the
	// answer cites none, the pages it was written from are its sources.
	cited := false
	ans = stormCite.ReplaceAllStringFunc(ans, func(m string) string {
		var n int
		fmt.Sscanf(m, "[%d]", &n)
		if n < 1 || n > len(pages) {
			return ""
		}
		cited = true
		return " (" + pages[n-1].url + ")"
	})
	ans = strings.Join(strings.Fields(ans), " ")
	if !cited {
		for _, pg := range pages {
			ans += " (" + pg.url + ")"
		}
	}
	st.answered++
	note(fmt.Sprintf("Q: %s A: %s", q, ans))
}

// ---- merge and write ----

type stormSection struct{ Title, Scope string }

var stormDefaultOutline = []stormSection{
	{"What it is and why it matters", "the basic picture"},
	{"How it works in practice", "real use and what works"},
	{"Evidence and its limits", "what is known, what is doubtful"},
	{"Costs, trade-offs and wider effects", "who pays and who gains"},
	{"How it got here", "history and context"},
}

func (st *stormRun) outline(ctx context.Context) []stormSection {
	out, err := st.chat(ctx, `Propose 4 to 7 sections for a research report on the topic, grouped by theme (not by viewpoint), from these notes. Titles are short. Reply with JSON {"sections": [{"title": "...", "scope": "one line"}]}.`,
		"Topic: "+st.topic+"\n\n"+st.digest(3, 200),
		json.RawMessage(`{"type":"object","properties":{"sections":{"type":"array","minItems":4,"maxItems":7,"items":{"type":"object","properties":{"title":{"type":"string"},"scope":{"type":"string"}},"required":["title","scope"]}}},"required":["sections"]}`), 500)
	var r struct{ Sections []stormSection }
	if err != nil || json.Unmarshal([]byte(out), &r) != nil || len(r.Sections) < 3 {
		return stormDefaultOutline
	}
	var secs []stormSection
	for _, s := range r.Sections {
		if s.Title = strings.TrimSpace(strings.Trim(s.Title, "#* ")); s.Title != "" {
			secs = append(secs, s)
		}
	}
	if len(secs) < 3 {
		return stormDefaultOutline
	}
	return secs
}

type stormLine struct{ lane, text string }

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 4 {
			out[w] = true
		}
	}
	return out
}

// relevant picks the note lines most about a section, so each writing job sees only its own notes.
func relevant(lines []stormLine, sec stormSection, maxLines, maxChars int) []stormLine {
	want := words(sec.Title + " " + sec.Scope)
	type scored struct {
		l stormLine
		n int
		i int
	}
	var all []scored
	for i, l := range lines {
		n := 0
		for w := range words(l.text) {
			if want[w] {
				n++
			}
		}
		if !strings.Contains(l.text, "no reliable source found") {
			all = append(all, scored{l, n, i})
		}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].n > all[b].n })
	var out []stormLine
	total := 0
	for _, s := range all {
		if len(out) >= maxLines || total+len(s.l.text) > maxChars {
			continue
		}
		out = append(out, s.l)
		total += len(s.l.text)
	}
	return out
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	if out == "" {
		out = "section"
	}
	return out
}

func (st *stormRun) write(ctx context.Context) (*tools.StormResult, error) {
	sources := builtin.NoteSources(st.dir)
	index := map[string]int{}
	for _, s := range sources {
		index[s.URL] = s.N
	}
	var lines []stormLine
	for _, lane := range builtin.NoteLanes(st.dir) {
		for _, l := range builtin.ReadLane(st.dir, lane) {
			lines = append(lines, stormLine{lane, l})
		}
	}
	cite := func(text string) string {
		return stormURL.ReplaceAllStringFunc(text, func(u string) string {
			if n, ok := index[strings.TrimRight(u, ".,;:!?")]; ok {
				return fmt.Sprintf("[%d]", n)
			}
			return ""
		})
	}
	secs := st.outline(ctx)
	res := &tools.StormResult{Answered: st.answered, Unsupported: st.unsupported, Sources: len(sources), Sections: len(secs)}
	var outline, refs strings.Builder
	fmt.Fprintf(&outline, "# Outline: %s\n\n", st.topic)
	refs.WriteString("## Sources\n\n")
	for _, s := range sources {
		fmt.Fprintf(&refs, "[%d] %s\n", s.N, s.URL)
	}
	var bodies []string
	for i, sec := range secs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st.say("writing section %d/%d: %s", i+1, len(secs), sec.Title)
		fmt.Fprintf(&outline, "%d. %s: %s\n", i+1, sec.Title, sec.Scope)
		var notes strings.Builder
		for _, l := range relevant(lines, sec, 10, 3500) {
			fmt.Fprintf(&notes, "- (%s) %s\n", l.lane, cite(l.text))
		}
		body := ""
		if notes.Len() > 0 {
			out, err := st.chat(ctx, "You write one section of a research report. Use ONLY the notes below. After every fact put its reference number in brackets, copied from the notes, like [2]. If the notes do not cover something, do not write it. Plain prose, 100 to 200 words, no lists. Start with the heading line: ## "+sec.Title,
				fmt.Sprintf("Report topic: %s\nSection: %s (%s)\n\nNotes:\n%s", st.topic, sec.Title, sec.Scope, notes.String()), nil, 500)
			if err == nil {
				body = out
			}
		}
		body = tidySection(body, sec.Title, len(sources))
		if gap := "<!-- gap: nothing in this section is cited -->"; strings.Contains(body, gap) {
			// The model wrote from the notes but didn't cite: name the sources of those notes instead.
			var nums []string
			seen := map[int]bool{}
			for _, l := range relevant(lines, sec, 10, 3500) {
				for _, u := range stormURL.FindAllString(l.text, -1) {
					if n, ok := index[strings.TrimRight(u, ".,;:!?)")]; ok && !seen[n] && len(nums) < 6 {
						seen[n] = true
						nums = append(nums, fmt.Sprintf("[%d]", n))
					}
				}
			}
			if len(nums) > 0 {
				body = strings.Replace(body, gap, "_Written from notes citing "+strings.Join(nums, " ")+"; the text itself doesn't say which fact came from which._", 1)
			}
		}
		bodies = append(bodies, body)
		res.Files = append(res.Files, tools.StormFile{Name: fmt.Sprintf("%02d-%s.md", i+1, slug(sec.Title)), Content: body + "\n"})
	}
	var digest strings.Builder
	for _, b := range bodies {
		digest.WriteString(clipRunes(b, 700) + "\n\n")
	}
	summary, err := st.chat(ctx, "Write a summary of this research report in at most 5 short lines, the answer first. Use only the section texts below, and keep the reference numbers [n] of the facts you use.", "Topic: "+st.topic+"\n\n"+digest.String(), nil, 300)
	if err != nil || summary == "" {
		summary = "(no summary could be written: read the sections)"
	}
	res.Summary = summary
	var report strings.Builder
	fmt.Fprintf(&report, "# %s\n\n## Summary\n\n%s\n\n%s\n%s", st.topic, summary, strings.Join(bodies, "\n\n"), "\n\n"+refs.String())
	res.Files = append([]tools.StormFile{{Name: "00-summary.md", Content: fmt.Sprintf("# %s\n\n## Summary\n\n%s\n", st.topic, summary)}}, res.Files...)
	res.Files = append(res.Files,
		tools.StormFile{Name: "outline.md", Content: outline.String()},
		tools.StormFile{Name: "references.md", Content: refs.String()},
		tools.StormFile{Name: "report.md", Content: report.String()})
	for _, lane := range builtin.NoteLanes(st.dir) {
		var b strings.Builder
		fmt.Fprintf(&b, "# Research notes: %s\n\n", lane)
		for _, l := range builtin.ReadLane(st.dir, lane) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		res.Files = append(res.Files, tools.StormFile{Name: "notes-" + lane + ".md", Content: b.String()})
	}
	return res, nil
}

// tidySection makes a written section safe to ship: it starts with its heading, cites only numbers
// that exist, and says so when nothing was cited.
func tidySection(body, title string, nSources int) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return "## " + title + "\n\n<!-- gap: the notes held nothing for this section -->"
	}
	if !strings.HasPrefix(body, "## ") {
		body = "## " + title + "\n\n" + strings.TrimLeft(body, "# \n")
	}
	body = stormCite.ReplaceAllStringFunc(body, func(m string) string {
		var n int
		fmt.Sscanf(m, "[%d]", &n)
		if n < 1 || n > nSources {
			return ""
		}
		return m
	})
	if !stormCite.MatchString(body) {
		body += "\n\n<!-- gap: nothing in this section is cited -->"
	}
	return body
}
