package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"noskill/internal/backends"
	"noskill/internal/backendtest"
	"noskill/internal/tools"
	"noskill/internal/tools/builtin"
)

// stormFake plays every job of a STORM run: the questions, the research (a fetch, then an answer
// that cites it and one page it never fetched), the outline, the sections and the summary.
// stormFake plays every job a STORM run gives the model: the questions (with an aside and a second
// sentence, as small models write them), a search query, the answer from the pages, the outline,
// the sections and the summary. Searching and fetching are stubbed separately.
func stormFake() *backendtest.Fake {
	return &backendtest.Fake{Reply: func(r backends.ChatRequest) (backends.Response, error) {
		sys := r.Messages[0].Content
		user := r.Messages[len(r.Messages)-1].Content
		json := func(s string) (backends.Response, error) { return backends.Response{Content: s}, nil }
		switch {
		case strings.Contains(sys, "Break the research goal"):
			who := regexp.MustCompile(`a (\w+) would ask`).FindStringSubmatch(user)[1]
			// Different words per viewpoint: the same question twice is skipped.
			ask := map[string][2]string{
				"practitioner": {"How does it work in day to day use?", "Which tools make everyday work easier?"},
				"academic":     {"Which theory explains the results?", "How is success measured in studies?"},
				"skeptic":      {"Which claims lack evidence?", "What assumptions hide in the benchmarks?"},
				"economist":    {"Who pays for the hardware?", "Which incentives shape adoption?"},
				"historian":    {"When did the idea first appear?", "Which turning points followed?"},
			}[who]
			return json(fmt.Sprintf(`{"questions":["%s (e.g., in an office) And what else?","%s"]}`, ask[0], ask[1]))
		case strings.Contains(sys, "Pick one claim"):
			return json(fmt.Sprintf(`{"question":"UNANSWERED: is claim number %d right?"}`, len(user)))
		case strings.Contains(sys, "List the 3 most important"):
			return json(`{"questions":["Is there a second source for the main claim?","Is there a counter-claim to it?"]}`)
		case strings.Contains(sys, "Propose 4 to 7 sections"):
			return json(`{"sections":[{"title":"Day to day use","scope":"how it works in practice"},{"title":"What people learned","scope":"lessons"},{"title":"Costs and trade-offs","scope":"who pays"},{"title":"History","scope":"how it developed"}]}`)
		case strings.Contains(sys, "Write a web search query"):
			return backends.Response{Content: "search " + strings.ToLower(user[strings.Index(user, "Question: ")+10:])}, nil
		case strings.Contains(sys, "Answer the question using ONLY"):
			return backends.Response{Content: "It works in practice [1]. More detail on the second page [2]. An odd citation [9]."}, nil
		case strings.Contains(sys, "You write one section"):
			title := strings.TrimPrefix(sys[strings.Index(sys, "## "):], "## ")
			if strings.HasPrefix(title, "History") {
				return backends.Response{Content: "## " + title + "\n\nText with no citations at all."}, nil
			}
			return backends.Response{Content: "## " + title + "\n\nA fact from the notes [1]. Another one [2]. An invented citation [99]."}, nil
		case strings.Contains(sys, "Write a summary"):
			return backends.Response{Content: "It works in practice [1]."}, nil
		}
		return backends.Response{Content: `{"task_type":"chat"}`}, nil
	}}
}

// stubWeb replaces the search and fetch tools: a search returns an encyclopedia page, a page that
// can't be fetched, and four good pages (only three are read); a query containing "unanswered" finds nothing.
func stubWeb(e *env) {
	long := strings.Repeat("The page explains how small models behave on a laptop in daily use. ", 12)
	search := e.app.Registry["web.search"]
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: search.Schema,
		Run: func(_ context.Context, _ *tools.Env, raw json.RawMessage) (string, error) {
			var a struct{ Query string }
			json.Unmarshal(raw, &a)
			if strings.Contains(a.Query, "unanswered") {
				return `[]`, nil
			}
			s := slug(a.Query)
			var out []map[string]string
			for _, h := range []string{"en.wikipedia.org", "bad.example", "a.example", "b.example", "c.example", "d.example"} {
				out = append(out, map[string]string{"title": h, "url": "https://" + h + "/" + s, "snippet": "x"})
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}}
	fetch := e.app.Registry["web.fetch"]
	e.app.Registry["web.fetch"] = tools.Builtin{ID: "web.fetch", Description: "stub", Schema: fetch.Schema,
		Run: func(_ context.Context, _ *tools.Env, raw json.RawMessage) (string, error) {
			var a struct{ URL string }
			json.Unmarshal(raw, &a)
			if strings.Contains(a.URL, "bad.example") {
				return "", errors.New("bad.example answered 503")
			}
			b, _ := json.Marshal(map[string]string{"url": a.URL, "title": "t", "text_markdown": long})
			return string(b), nil
		}}
}

// fastStorm removes the pauses a real run takes between searches and after a failed one.
func fastStorm(t *testing.T) {
	t.Helper()
	oldWaits, oldPace := stormSearchWaits, stormPace
	stormSearchWaits, stormPace = []time.Duration{0, 0}, 0
	t.Cleanup(func() { stormSearchWaits, stormPace = oldWaits, oldPace })
}

func TestStormPipeline(t *testing.T) {
	fastStorm(t)
	ctx := context.Background()
	e := newApp(t, stormFake(), noRules, "")
	stubWeb(e)
	m, err := e.app.FindModel(ctx, "qwen3:4b")
	if err != nil {
		t.Fatal(err)
	}
	svc := &runServices{a: e.app, runID: "storm-test", model: m}
	res, err := svc.Storm(ctx, "small models on laptops", 2)
	if err != nil {
		t.Fatal(err)
	}

	// Five viewpoints, one lane each, with the notes in the lane files.
	dir, _ := svc.NotesDir("storm-test")
	lanes := builtin.NoteLanes(dir + "/storm")
	if strings.Join(lanes, ",") != "academic,economist,historian,practitioner,skeptic" {
		t.Fatalf("lanes %v", lanes)
	}
	prac := strings.Join(builtin.ReadLane(dir+"/storm", "practitioner"), "\n")
	// Page numbers became the pages' addresses; an encyclopedia, a page that failed and the fourth
	// good page were never used; a citation to a page that doesn't exist is gone.
	if !strings.Contains(prac, "(https://a.example/") || !strings.Contains(prac, "(https://b.example/") || strings.Contains(prac, "wikipedia") ||
		strings.Contains(prac, "bad.example") || strings.Contains(prac, "d.example") || strings.Contains(prac, "[9]") || strings.Contains(prac, "(e.g.") {
		t.Errorf("practitioner notes:\n%s", prac)
	}
	if !strings.Contains(prac, "Q: How does it work in day to day use? A:") {
		t.Errorf("the question wasn't cut to its first sentence:\n%s", prac)
	}
	// Only fetched pages are sources, and an answer without one is "no reliable source found".
	var report string
	names := map[string]bool{}
	for _, f := range res.Files {
		names[f.Name] = true
		if f.Name == "report.md" {
			report = f.Content
		}
		if strings.Contains(f.Content, "wikipedia") || strings.Contains(f.Content, "bad.example") || strings.Contains(f.Content, "d.example") {
			t.Errorf("a page that was never used is cited in %s", f.Name)
		}
		if strings.Contains(f.Content, "[99]") {
			t.Errorf("an invented citation survived in %s", f.Name)
		}
	}
	for _, want := range []string{"00-summary.md", "01-day-to-day-use.md", "04-history.md", "outline.md", "references.md", "report.md", "notes-skeptic.md", "notes-historian.md"} {
		if !names[want] {
			t.Errorf("missing %s (have %v)", want, names)
		}
	}
	for _, want := range []string{"# small models on laptops", "## Summary", "It works in practice [1].", "## Day to day use", "## Sources", "[1] https://a.example/"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if res.Sections != 4 || res.Unsupported != 1 || res.Answered != 12 || res.Sources < 12 { // the cross-check question repeats: asked once
		t.Errorf("counts: %+v", res)
	}
	if res.Summary != "It works in practice [1]." {
		t.Errorf("summary %q", res.Summary)
	}
	// A section the model wrote without citing is not left bare: it names the sources of its notes.
	for _, f := range res.Files {
		if strings.HasPrefix(f.Name, "04-history") && (!strings.Contains(f.Content, "_Written from notes citing [") || strings.Contains(f.Content, "<!-- gap")) {
			t.Errorf("the uncited section:\n%s", f.Content)
		}
	}
}

// A search that fails is tried again after a pause, instead of costing the question.
func TestStormRetriesAFailedSearch(t *testing.T) {
	fastStorm(t)
	e := newApp(t, stormFake(), noRules, "")
	stubWeb(e)
	good := e.app.Registry["web.search"]
	fails := 0
	e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: good.Schema,
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
			if fails < 1 {
				fails++
				return "", errors.New("every search provider failed: ddg: context deadline exceeded")
			}
			return good.Run(ctx, env, raw)
		}}
	m, _ := e.app.FindModel(context.Background(), "qwen3:4b")
	st := &stormRun{s: &runServices{a: e.app, runID: "retry", model: m}, topic: "t", n: 1, dir: t.TempDir()}
	st.research(context.Background(), stormPersonas[0], "How does it work in day to day use?")
	if fails != 1 || st.answered != 1 || st.unsupported != 0 {
		t.Errorf("fails %d, answered %d, unsupported %d", fails, st.answered, st.unsupported)
	}
	// Failures on every try do cost the question, and say why.
	fails = -5
	st2 := &stormRun{s: st.s, topic: "t", n: 1, dir: t.TempDir()}
	st2.research(context.Background(), stormPersonas[0], "Another question?")
	if st2.unsupported != 1 || !strings.Contains(strings.Join(builtin.ReadLane(st2.dir, "practitioner"), ""), "the search failed") {
		t.Errorf("answered %d unsupported %d", st2.answered, st2.unsupported)
	}
}

func TestNearDuplicateQuestionsAreSkipped(t *testing.T) {
	st := &stormRun{topic: "small language models on a laptop"}
	if st.asked("What evidence shows that benchmarks of these models are reliable?") {
		t.Fatal("the first question was a repeat")
	}
	// The same question in other words around the topic, and the topic's own words don't count.
	if !st.asked("What evidence shows that benchmarks of small language models are reliable on a laptop?") {
		t.Error("a near-identical question was asked again")
	}
	if st.asked("Who pays for the hardware of small language models on a laptop, and who gains?") {
		t.Error("a different question was skipped")
	}
	if st.asked("small language models laptop") || st.asked("small language models laptop") {
		t.Error("a question with nothing but the topic's words must not be treated as a repeat")
	}
}

// When the search keeps failing the research stops after a few questions, and says so: with nothing
// answered it is an error with advice, with something answered the report is written from that.
func TestStormStopsWhenSearchKeepsFailing(t *testing.T) {
	fastStorm(t)
	for _, goodCalls := range []int{0, 3} {
		e := newApp(t, stormFake(), noRules, "")
		stubWeb(e)
		good := e.app.Registry["web.search"]
		calls := 0
		e.app.Registry["web.search"] = tools.Builtin{ID: "web.search", Description: "stub", Schema: good.Schema,
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				calls++
				if calls <= goodCalls {
					return good.Run(ctx, env, raw)
				}
				return "", errors.New("every search provider failed: ddg: timeout")
			}}
		m, _ := e.app.FindModel(context.Background(), "qwen3:4b")
		svc := &runServices{a: e.app, runID: fmt.Sprintf("giveup-%d", goodCalls), model: m}
		res, err := svc.Storm(context.Background(), "small models on laptops", 2)
		if goodCalls == 0 {
			if err == nil || !strings.Contains(err.Error(), "web search keeps failing") || !strings.Contains(err.Error(), "BRAVE_API_KEY") {
				t.Errorf("nothing answered: %v", err)
			}
			continue
		}
		if err != nil || res.Answered != 3 || res.Unsupported != stormGiveUp {
			t.Errorf("partial: %v %+v", err, res)
		}
	}
}
