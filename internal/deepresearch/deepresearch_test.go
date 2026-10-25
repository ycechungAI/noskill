package deepresearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

type fakeLLM struct {
	calls   atomic.Int32
	scope   []string
	gaps    []string
	extract func(question, passage string) []map[string]string // claims for a passage
	section func(user string) string
	summary func(user string) string
	queries []string
}

func (f *fakeLLM) Ask(_ context.Context, system, user string, _ json.RawMessage, _ int) (string, error) {
	f.calls.Add(1)
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	switch {
	case strings.HasPrefix(system, "Break this research topic"):
		return js(map[string]any{"questions": f.scope}), nil
	case strings.HasPrefix(system, "Write a web search query"), strings.HasPrefix(system, "The first search"):
		f.queries = append(f.queries, user)
		return "kw " + strings.ToLower(strings.Fields(strings.TrimPrefix(user, "Topic: "))[0]), nil
	case strings.HasPrefix(system, "You read one passage"):
		q := strings.TrimPrefix(strings.SplitN(user, "\n", 2)[0], "Question: ")
		p := user[strings.Index(user, "Passage:\n")+9:]
		return js(map[string]any{"claims": f.extract(q, p)}), nil
	case strings.HasPrefix(system, "You are checking a research for gaps"):
		return js(map[string]any{"gaps": f.gaps}), nil
	case strings.HasPrefix(system, "Propose 4 to 6 sections"):
		return js(map[string]any{"sections": []map[string]string{{"title": "Basics", "scope": "what tea is and how it is made"}, {"title": "Health", "scope": "health evidence and studies"}, {"title": "Trade", "scope": "production, price and trade"}}}), nil
	case strings.HasPrefix(system, "Write one section"):
		return f.section(user), nil
	case strings.HasPrefix(system, "Write the summary"):
		return f.summary(user), nil
	}
	return "", errors.New("unexpected prompt: " + system[:30])
}

type fakeWeb struct {
	hits  map[string][]Hit // by query substring ("" = any)
	pages map[string]Page
	fails int // searches that fail first
	n     atomic.Int32
}

func (w *fakeWeb) Search(_ context.Context, q string) ([]Hit, error) {
	w.n.Add(1)
	if int(w.n.Load()) <= w.fails {
		return nil, errors.New("search is down")
	}
	for k, h := range w.hits {
		if k != "" && strings.Contains(q, k) {
			return h, nil
		}
	}
	return w.hits[""], nil
}

func (w *fakeWeb) Fetch(_ context.Context, u string) (Page, error) {
	if p, ok := w.pages[u]; ok {
		return p, nil
	}
	return Page{}, errors.New("404")
}

func page(url, title string, sentences ...string) Page {
	text := strings.Join(sentences, " ")
	for len([]rune(text)) < 400 {
		text += " This sentence is filler that says nothing about the subject at hand."
	}
	return Page{URL: url, Title: title, Text: text}
}

func hit(u string) Hit { return Hit{Title: u, URL: u, Snippet: "s"} }

// claimsFrom proposes, for each passage, every sentence of it that has a keyword as a claim whose
// quote is that sentence: what a careful model does.
func claimsFrom(keywords ...string) func(q, p string) []map[string]string {
	return func(_, p string) []map[string]string {
		var out []map[string]string
		for _, s := range strings.SplitAfter(p, ". ") {
			s = strings.TrimSpace(s)
			for _, k := range keywords {
				if strings.Contains(s, k) {
					out = append(out, map[string]string{"claim": strings.TrimSuffix(s, "."), "quote": s})
					break
				}
			}
		}
		return out
	}
}

func baseLLM() *fakeLLM {
	return &fakeLLM{
		scope:   []string{"What is tea and how is it made?", "What does the research say about tea and health?", "Where is tea produced and traded?"},
		extract: claimsFrom("Tea is", "studies", "tonnes"),
		section: func(user string) string { return "Tea is a drink made from leaves [1]." },
		summary: func(user string) string { return "Tea is a widely drunk beverage [1]." },
	}
}

var cfgFast = Config{SearchWaits: []time.Duration{0}}

func doRun(t *testing.T, topic string, llm *fakeLLM, web *fakeWeb, cfg Config, local Local) Result {
	t.Helper()
	d := Deps{LLM: llm, Search: web, Fetch: web, Local: local}
	res, err := Run(context.Background(), topic, d, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// ---- tests ----

func TestVerifyClaim(t *testing.T) {
	text := "In 2023 the farm produced 1,200 tonnes of green tea.\nThe “first harvest” starts in April, according to the growers' association."
	cases := []struct {
		claim, quote string
		ok           bool
	}{
		{"The farm produced 1,200 tonnes of green tea in 2023", "In 2023 the farm produced 1,200 tonnes of green tea.", true},
		{"First harvest starts in April", `The "first harvest" starts in April, according to the growers' association.`, true}, // quotes and case differ only in typography
		{"The farm produced 5,000 tonnes", "In 2023 the farm produced 1,200 tonnes of green tea.", false},                      // a number the quote doesn't have
		{"The farm produced 1,200 tonnes of green tea", "In 2024 the farm produced 9,000 tonnes of black tea.", false},         // the quote isn't in the page
		{"Chocolate is sweet and made from beans", "In 2023 the farm produced 1,200 tonnes of green tea.", false},              // a real quote behind an invented fact
		{"Green tea", "green tea", false}, // too short
	}
	for _, c := range cases {
		if ok, why := verifyClaim(c.claim, c.quote, text); ok != c.ok {
			t.Errorf("%q / %q: ok = %v (%s)", c.claim, c.quote, ok, why)
		}
	}
}

func TestEndToEndKeepsOnlyClaimsTheirQuotesBack(t *testing.T) {
	web := &fakeWeb{
		hits: map[string][]Hit{"": {hit("https://a.example.org/tea"), hit("https://b.example.edu/health"), hit("https://c.example.com/trade")}},
		pages: map[string]Page{
			"https://a.example.org/tea":    page("https://a.example.org/tea", "Tea basics", "Tea is a drink made by steeping the leaves of Camellia sinensis in hot water."),
			"https://b.example.edu/health": page("https://b.example.edu/health", "Health", "Several studies link regular tea drinking to lower blood pressure in adults."),
			"https://c.example.com/trade":  page("https://c.example.com/trade", "Trade", "In 2023 the country exported 41,000 tonnes of tea to Europe and Asia."),
		},
	}
	llm := baseLLM()
	extract := llm.extract
	llm.extract = func(q, p string) []map[string]string {
		out := extract(q, p)
		// A model that invents: a quote that isn't on the page, and a real quote with a made-up number.
		out = append(out, map[string]string{"claim": "Tea cures every illness", "quote": "Tea is said to cure every illness known to people"})
		if strings.Contains(p, "exported") {
			out = append(out, map[string]string{"claim": "The country exported 99,000 tonnes of tea", "quote": "In 2023 the country exported 41,000 tonnes of tea to Europe and Asia."})
		}
		return out
	}
	llm.section = func(user string) string {
		if strings.Contains(user, "41000") || strings.Contains(user, "41,000") {
			return "The country exported 41,000 tonnes of tea [3] and sold some at 12 dollars [7]." // 12 isn't in the facts
		}
		return "Tea is a drink made from leaves [1], and studies link it to lower blood pressure [2] [9]." // [9] isn't a source
	}
	res := doRun(t, "Tea", llm, web, Config{Rounds: 1, SearchWaits: []time.Duration{0}}, nil)
	rep := res.Files[0].Content
	for _, bad := range []string{"cures every illness", "99,000", "[9]", "12 dollars"} {
		if strings.Contains(rep, bad) {
			t.Errorf("the report holds %q:\n%s", bad, rep)
		}
	}
	for _, want := range []string{"# Tea", "## Summary", "## Appendix: the evidence", "## Method and limits", "## Sources", "41,000 tonnes", "[2]", "https://c.example.com/trade"} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report lacks %q:\n%s", want, rep)
		}
	}
	st := res.State.Stats
	if st.ClaimsDropped < 3 || st.ClaimsVerified != len(res.State.Claims) || st.ClaimsVerified < 3 {
		t.Errorf("stats = %+v", st)
	}
	// The trade section's prose broke the number rule twice, so it is shown as its claims.
	if !strings.Contains(rep, "- In 2023 the country exported 41,000 tonnes of tea to Europe and Asia [3]") {
		t.Errorf("the section that failed the number check should list its claims:\n%s", rep)
	}
	if len(res.Files) != 2 || res.Files[1].Name != "evidence.json" {
		t.Errorf("files = %v", res.Files)
	}
}

func TestSourcesThatGiveDifferentNumbersAreReported(t *testing.T) {
	web := &fakeWeb{
		hits: map[string][]Hit{"": {hit("https://one.example.org/a"), hit("https://two.example.org/b")}},
		pages: map[string]Page{
			"https://one.example.org/a": page("https://one.example.org/a", "One", "Global tea production reached 6.5 million tonnes in 2022 across all producing countries."),
			"https://two.example.org/b": page("https://two.example.org/b", "Two", "Global tea production reached 5.8 million tonnes in 2022 across all producing countries."),
		},
	}
	llm := baseLLM()
	llm.scope = []string{"How much tea is produced in the world?", "Where is tea grown?", "Who drinks the most tea?"}
	llm.extract = claimsFrom("production")
	res := doRun(t, "Tea", llm, web, Config{Rounds: 1, SearchWaits: []time.Duration{0}}, nil)
	rep := res.Files[0].Content
	if res.Conflict != 1 || !strings.Contains(rep, "## Where sources disagree") || !strings.Contains(rep, "6.5") || !strings.Contains(rep, "5.8") {
		t.Errorf("the disagreement isn't reported (conflicts %d):\n%s", res.Conflict, rep)
	}
}

func TestCorroborationCountsDistinctSites(t *testing.T) {
	srcs := []Source{{ID: 1, Host: "a.org"}, {ID: 2, Host: "b.org"}, {ID: 3, Host: "a.org"}}
	cs := []Claim{
		{ID: 1, Text: "Tea lowers blood pressure in adults", Source: 1},
		{ID: 2, Text: "Regular tea lowers blood pressure in adults", Source: 2},
		{ID: 3, Text: "Tea lowers blood pressure in adults", Source: 3}, // same site as claim 1: not extra support
		{ID: 4, Text: "Coffee is roasted before it is ground", Source: 2},
	}
	Corroborate(cs, srcs)
	if cs[0].Support != 2 || cs[1].Support != 2 || cs[3].Support != 1 {
		t.Errorf("support = %d %d %d %d", cs[0].Support, cs[1].Support, cs[2].Support, cs[3].Support)
	}
}

func TestGapRoundAsksNewQuestionsAndRetriesEmptyOnes(t *testing.T) {
	web := &fakeWeb{
		hits: map[string][]Hit{
			"kw":  {hit("https://a.example.org/tea")},
			"alt": {hit("https://z.example.org/other")},
		},
		pages: map[string]Page{
			"https://a.example.org/tea":   page("https://a.example.org/tea", "A", "Tea is a drink made from the leaves of a plant that grows in warm wet places."),
			"https://z.example.org/other": page("https://z.example.org/other", "Z", "Studies of tea farms show that shade trees raise the yield of leaves each year."),
		},
	}
	llm := baseLLM()
	llm.scope = []string{"What is tea?", "How do shade trees affect tea farms?", "What is the history of tea?"}
	llm.gaps = []string{"What do tea farmers earn each year?"}
	// The first query for a question is "kw ..."; a retry says "other words" and gets "alt".
	asked := 0
	llm.extract = func(q, p string) []map[string]string {
		asked++
		return claimsFrom("Tea is", "Studies")(q, p)
	}
	res := doRun(t, "Tea", llm, web, Config{Rounds: 2, SearchWaits: []time.Duration{0}}, nil)
	var gap, check bool
	for _, q := range res.State.Questions {
		switch q.Kind {
		case "gap":
			gap = q.Text == "What do tea farmers earn each year?" && q.Round == 2
		case "check":
			check = true
		}
	}
	if !gap {
		t.Errorf("the gap the model named wasn't asked in round 2: %+v", res.State.Questions)
	}
	if !check {
		t.Errorf("a claim from one site should have produced a check question: %+v", res.State.Questions)
	}
	if res.State.RoundsDone != 2 {
		t.Errorf("rounds = %d", res.State.RoundsDone)
	}
}

func TestLinksFromGoodPagesAreFollowedToPrimarySources(t *testing.T) {
	web := &fakeWeb{
		hits: map[string][]Hit{"": {hit("https://blog.example.com/post")}},
		pages: map[string]Page{
			"https://blog.example.com/post": page("https://blog.example.com/post", "Blog", "Tea is a drink that people have made for thousands of years. See the [official tea statistics](https://stats.example.gov/tea) for the numbers."),
			"https://stats.example.gov/tea": page("https://stats.example.gov/tea", "Statistics", "Tea is a crop whose output was measured at 5.8 million tonnes in the latest official count."),
		},
	}
	llm := baseLLM()
	llm.extract = claimsFrom("Tea is")
	res := doRun(t, "Tea", llm, web, Config{Rounds: 1, SearchWaits: []time.Duration{0}}, nil)
	var lead bool
	for _, s := range res.State.Sources {
		if s.URL == "https://stats.example.gov/tea" && s.Lead {
			lead = true
		}
	}
	if !lead || res.State.Stats.LeadsFollowed != 1 {
		t.Errorf("the link to the statistics page wasn't followed: %+v %+v", res.State.Sources, res.State.Stats)
	}
	if !strings.Contains(res.Files[0].Content, "reached by following a link") {
		t.Error("the method section should say a link was followed")
	}
}

func TestBudgetStopsGracefullyAndSaysSo(t *testing.T) {
	web := &fakeWeb{
		hits:  map[string][]Hit{"": {hit("https://a.example.org/tea")}},
		pages: map[string]Page{"https://a.example.org/tea": page("https://a.example.org/tea", "A", "Tea is a drink made from the leaves of a plant that grows in warm wet places.")},
	}
	llm := baseLLM()
	// Time runs 10 minutes per model call; the budget is 25 minutes.
	var now time.Time = time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	d := Deps{LLM: llm, Search: web, Fetch: web, Now: func() time.Time { now = now.Add(2 * time.Minute); return now }}
	res, err := Run(context.Background(), "Tea", d, Config{Rounds: 3, Budget: 25 * time.Minute, SearchWaits: []time.Duration{0}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State.Stopped != "time" || res.State.RoundsDone >= 3 {
		t.Errorf("stopped = %q rounds = %d", res.State.Stopped, res.State.RoundsDone)
	}
	if !strings.Contains(res.Files[0].Content, "stopped early (time limit)") {
		t.Errorf("the report must say it stopped early:\n%s", res.Files[0].Content)
	}
}

func TestAStoppedRunResumesWithoutRepeatingWork(t *testing.T) {
	dir := t.TempDir()
	web := &fakeWeb{
		hits: map[string][]Hit{"": {hit("https://a.example.org/tea"), hit("https://b.example.edu/h"), hit("https://c.example.com/t")}},
		pages: map[string]Page{
			"https://a.example.org/tea": page("https://a.example.org/tea", "A", "Tea is a drink made from the leaves of a plant that grows in warm wet places."),
			"https://b.example.edu/h":   page("https://b.example.edu/h", "B", "Several studies link regular tea drinking to lower blood pressure in adults."),
			"https://c.example.com/t":   page("https://c.example.com/t", "C", "In 2023 the country exported 41,000 tonnes of tea to Europe and Asia."),
		},
	}
	llm := baseLLM()
	// First run: cancelled after the second search.
	ctx, cancel := context.WithCancel(context.Background())
	cw := &cancelWeb{fakeWeb: web, after: 2, cancel: cancel}
	d := Deps{LLM: llm, Search: cw, Fetch: web}
	if _, err := Run(ctx, "Tea", d, Config{Rounds: 1, Dir: dir, SearchWaits: []time.Duration{0}}); err == nil {
		t.Fatal("a cancelled run should return the cancellation")
	}
	searches := web.n.Load()
	res, err := Run(context.Background(), "Tea", Deps{LLM: llm, Search: web, Fetch: web}, Config{Rounds: 1, Dir: dir, SearchWaits: []time.Duration{0}})
	if err != nil {
		t.Fatal(err)
	}
	if again := web.n.Load() - searches; again >= int32(len(res.State.Questions)) {
		t.Errorf("the resumed run searched %d times for %d questions: it repeated the answered ones", again, len(res.State.Questions))
	}
	if res.State.RoundsDone != 1 || len(res.State.Claims) < 3 {
		t.Errorf("state = rounds %d claims %d", res.State.RoundsDone, len(res.State.Claims))
	}
}

type cancelWeb struct {
	*fakeWeb
	after  int32
	cancel context.CancelFunc
}

func (c *cancelWeb) Search(ctx context.Context, q string) ([]Hit, error) {
	h, err := c.fakeWeb.Search(ctx, q)
	if c.fakeWeb.n.Load() >= c.after {
		c.cancel()
	}
	return h, err
}

func TestSearchThatKeepsFailingGivesAClearError(t *testing.T) {
	web := &fakeWeb{fails: 1000, hits: map[string][]Hit{}}
	llm := baseLLM()
	_, err := Run(context.Background(), "Tea", Deps{LLM: llm, Search: web, Fetch: web}, cfgFast)
	if err == nil || !strings.Contains(err.Error(), "search keeps failing") {
		t.Errorf("err = %v", err)
	}
}

type fakeLocal struct{}

func (fakeLocal) Search(_ context.Context, q string, k int) ([]Page, error) {
	return []Page{page("file:///notes/tea.md", "My tea notes", "Tea is the drink I have every morning, brewed for three minutes in water just off the boil.")}, nil
}

func TestLocalDocumentsJoinTheEvidence(t *testing.T) {
	web := &fakeWeb{hits: map[string][]Hit{"": {}}, pages: map[string]Page{}}
	llm := baseLLM()
	res := doRun(t, "Tea", llm, web, Config{Rounds: 1, SearchWaits: []time.Duration{0}}, fakeLocal{})
	var local bool
	for _, s := range res.State.Sources {
		local = local || s.Local
	}
	if !local || !strings.Contains(res.Files[0].Content, "file:///notes/tea.md") {
		t.Errorf("the user's own document isn't a source:\n%s", res.Files[0].Content)
	}
}

func TestProseChecks(t *testing.T) {
	facts := "The country exported 41,000 tonnes in 2023."
	allowed := map[string]bool{"3": true}
	if out, ok := checkProse("It exported 41,000 tonnes in 2023 [3] [8].", allowed, facts, 100); !ok || strings.Contains(out, "[8]") {
		t.Errorf("a disallowed citation should be removed: %q %v", out, ok)
	}
	for name, s := range map[string]string{"new number": "It exported 52,000 tonnes in 2023 [3].", "no citation": "It exported 41,000 tonnes in 2023.", "only bad citation": "It exported 41,000 tonnes [8].", "empty": " "} {
		if _, ok := checkProse(s, allowed, facts, 100); ok {
			t.Errorf("%s passed", name)
		}
	}
	if _, ok := checkProse(strings.Repeat("word ", 300)+"[3]", allowed, facts, 100); ok {
		t.Error("an over-long text passed")
	}
}

func TestPassagesAndLinks(t *testing.T) {
	text := strings.Repeat("A sentence about tea that goes on. ", 100)
	ps := passages(text, 500)
	if len(ps) < 6 {
		t.Fatalf("%d passages", len(ps))
	}
	for _, p := range ps {
		if len([]rune(p)) > 520 {
			t.Errorf("a passage of %d characters", len([]rune(p)))
		}
	}
	got := bestPassages([]string{"Nothing here.", "Tea farms and shade trees in the hills.", "Another unrelated passage."}, "How do shade trees affect tea farms?", "tea", 1)
	if len(got) != 1 || !strings.Contains(got[0], "shade") {
		t.Errorf("best = %v", got)
	}
	links := LinksIn("See [the stats](https://x.gov/a) and [the stats again](https://x.gov/a) and [other](http://y.org/b).")
	if len(links) != 2 {
		t.Errorf("links = %v", links)
	}
}

func TestSourceScore(t *testing.T) {
	if !(sourceScore("https://www.nih.gov/x") > sourceScore("https://blog.example.com/x") && sourceScore("https://blog.example.com/x") > sourceScore("https://www.pinterest.com/x")) {
		t.Error("a government site should beat a blog and a blog should beat Pinterest")
	}
}

func Example_event() {
	fmt.Println(Event{Stage: "search", Round: 1}.Stage)
	// Output: search
}
