package deepresearch

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var defaultSections = []Section{
	{"Background", "what it is, definition, basic picture, history"},
	{"How it works", "how it is done in practice, mechanism, method"},
	{"What the evidence says", "findings, data, numbers, results, studies"},
	{"Criticism and limits", "problems, doubts, disagreements, weaknesses"},
	{"Costs, risks and trade-offs", "cost, risk, who pays, who gains"},
	{"Recent developments", "recent change, current state, what is next"},
}

var cite = regexp.MustCompile(`\[(\d+)\]`)

func (r *run) write(ctx context.Context) (Result, error) {
	st := r.st
	Corroborate(st.Claims, st.Sources)
	conflicts := Conflicts(st.Claims, st.Sources, 8)
	secs := r.outline(ctx)
	r.say("write", 0, 0, len(secs), "writing %d sections from %d verified claims", len(secs), len(st.Claims))

	// Claims to sections, strongest first.
	qtext := map[int]string{}
	for _, q := range st.Questions {
		qtext[q.ID] = q.Text
	}
	by := make([][]Claim, len(secs))
	for _, c := range st.Claims {
		i := bestSection(c, secs, qtext[c.Question])
		if i < 0 {
			i = 0
		}
		by[i] = append(by[i], c)
	}
	body := make([]string, len(secs))
	for i, s := range secs {
		sortClaims(by[i])
		if len(by[i]) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		r.say("write", 0, i+1, len(secs), "%s", s.Title)
		use := by[i]
		if len(use) > 10 {
			use = use[:10]
		}
		body[i] = r.section(ctx, s, use)
	}
	summary := r.summary(ctx, secs, by)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", st.Topic)
	fmt.Fprintf(&b, "*Deep research, %s. %d sources read, %d verified claims, %d rounds.*\n\n", r.d.Now().Format("2 January 2006"), len(st.Sources), len(st.Claims), st.RoundsDone)
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", summary)
	for i, s := range secs {
		if body[i] == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n%s\n\n", s.Title, body[i], sectionNote(by[i]))
	}
	if len(conflicts) > 0 {
		b.WriteString("## Where sources disagree\n\n")
		for _, c := range conflicts {
			fmt.Fprintf(&b, "- **%s** [%d] says %s; **%s** [%d] says %s.\n  - “%s” [%d]\n  - “%s” [%d]\n",
				r.host(c.A.Source), c.A.Source, joinNums(c.Numbers[0]), r.host(c.B.Source), c.B.Source, joinNums(c.Numbers[1]),
				clip(c.A.Quote, 220), c.A.Source, clip(c.B.Quote, 220), c.B.Source)
		}
		b.WriteString("\n")
	}
	gaps := unanswered(st.Questions)
	if len(gaps) > 0 || st.Stopped != "" {
		b.WriteString("## What this research could not establish\n\n")
		for _, q := range gaps {
			fmt.Fprintf(&b, "- %s (no page that was read gave a quotable answer)\n", q.Text)
		}
		if st.Stopped != "" {
			fmt.Fprintf(&b, "- The research stopped early (%s limit); questions still open were not answered.\n", st.Stopped)
		}
		b.WriteString("\n")
	}
	multi := 0
	for _, c := range st.Claims {
		if c.Support >= 2 {
			multi++
		}
	}
	b.WriteString("## Method and limits\n\n")
	fmt.Fprintf(&b, "- %d rounds, %d searches, %d pages read (%d reached by following a link from another page), %d model calls.\n",
		st.RoundsDone, st.Stats.Searches, st.Stats.PagesRead, st.Stats.LeadsFollowed, st.Stats.Calls)
	fmt.Fprintf(&b, "- %d claims proposed, %d kept: each kept claim has a quote that was found in its page and says what the claim says; %d were dropped.\n",
		st.Stats.ClaimsProposed, st.Stats.ClaimsVerified, st.Stats.ClaimsDropped)
	fmt.Fprintf(&b, "- %d of %d claims are made by more than one site. A claim from a single site is a lead, not a settled fact.\n", multi, len(st.Claims))
	b.WriteString("- Every sentence was written from the claims above and nothing else; a number or citation not in them is removed or the section is shown as its claims.\n")
	b.WriteString("- Pages were read as text (no PDFs, no scripts, no paywalled pages), so this is what the open web said on the day.\n\n")

	b.WriteString("## Sources\n\n")
	for _, s := range st.Sources {
		if !used(st.Claims, s.ID) {
			continue
		}
		t := s.Title
		if t == "" {
			t = s.Host
		}
		fmt.Fprintf(&b, "%d. %s: %s (read %s)\n", s.ID, clip(t, 100), s.URL, s.Fetched.Format("2 Jan 2006"))
	}
	b.WriteString("\n## Appendix: the evidence\n\n| # | Claim | Quote | Source | Sites |\n|---|---|---|---|---|\n")
	for _, c := range st.Claims {
		fmt.Fprintf(&b, "| %d | %s | “%s” | [%d] | %d |\n", c.ID, cell(c.Text), cell(clip(c.Quote, 240)), c.Source, c.Support)
	}

	ev, _ := json.MarshalIndent(map[string]any{"topic": st.Topic, "claims": st.Claims, "sources": st.Sources, "questions": st.Questions, "stats": st.Stats, "stopped": st.Stopped}, "", " ")
	res := Result{Files: []File{{"report.md", b.String()}, {"evidence.json", string(ev)}}, Summary: summary, State: *st, Conflict: len(conflicts), Unanswered: len(gaps)}
	r.say("done", st.RoundsDone, 0, 0, "report written: %d sections, %d sources, %d claims", len(secs), len(st.Sources), len(st.Claims))
	return res, nil
}

func used(cs []Claim, id int) bool {
	for _, c := range cs {
		if c.Source == id {
			return true
		}
	}
	return false
}

func (r *run) host(id int) string {
	for _, s := range r.st.Sources {
		if s.ID == id {
			return s.Host
		}
	}
	return "a source"
}

func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "/"), "\n", " ")
}

func sectionNote(cs []Claim) string {
	hosts, multi := map[int]bool{}, 0
	for _, c := range cs {
		hosts[c.Source] = true
		if c.Support >= 2 {
			multi++
		}
	}
	return fmt.Sprintf("*%d %s from %d %s; %d confirmed by more than one site.*", len(cs), plural(len(cs), "claim", "claims"), len(hosts), plural(len(hosts), "page", "pages"), multi)
}

// ---- outline ----

func (r *run) outline(ctx context.Context) []Section {
	var b strings.Builder
	for _, q := range r.st.Questions {
		fmt.Fprintf(&b, "- %s (%d facts)\n", clip(q.Text, 120), q.Claims)
	}
	out, err := r.ask(ctx, `Propose 4 to 6 sections for a research report on the topic, grouped by theme, from the questions the research asked. Titles are short (2 to 6 words); scope is one line of the words the section covers. Reply with JSON {"sections": [{"title": "...", "scope": "..."}]}.`,
		"Topic: "+r.st.Topic+"\n\nQuestions:\n"+b.String(),
		json.RawMessage(`{"type":"object","properties":{"sections":{"type":"array","minItems":3,"maxItems":6,"items":{"type":"object","properties":{"title":{"type":"string"},"scope":{"type":"string"}},"required":["title","scope"]}}},"required":["sections"]}`), 450)
	var p struct{ Sections []Section }
	if err != nil || json.Unmarshal([]byte(out), &p) != nil {
		return defaultSections
	}
	var secs []Section
	for _, s := range p.Sections {
		if s.Title = strings.TrimSpace(strings.Trim(s.Title, "#*: ")); s.Title != "" {
			s.Scope = strings.TrimSpace(s.Scope)
			secs = append(secs, s)
		}
	}
	if len(secs) < 3 {
		return defaultSections
	}
	return secs
}

// ---- writing a section from its claims ----

func (r *run) section(ctx context.Context, s Section, cs []Claim) string {
	var facts strings.Builder
	allowedSrc := map[string]bool{}
	var text strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&facts, "[%d] %s\n", c.Source, c.Text)
		allowedSrc[fmt.Sprint(c.Source)] = true
		text.WriteString(c.Text + " ")
	}
	sys := `Write one section of a research report from the facts below and nothing else. 80 to 180 words, plain prose in one or two paragraphs. After each fact you use, put the number of its source in square brackets, like [3]. Do not add facts, numbers, names or dates that are not in the facts. If facts disagree, say so.`
	warn := ""
	for try := 0; try < 2; try++ {
		out, err := r.ask(ctx, sys+warn, "Section: "+s.Title+" ("+s.Scope+")\n\nFacts:\n"+facts.String(), nil, 450)
		if err != nil {
			break
		}
		if fixed, ok := checkProse(out, allowedSrc, text.String(), 260); ok {
			return fixed
		}
		warn = "\nYour last answer used a number, citation or length that the facts don't allow. Use only the facts, only their source numbers, and keep to 180 words."
	}
	// The section as its claims: always grounded, less fluent.
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s [%d]\n", c.Text, c.Source)
	}
	return strings.TrimRight(b.String(), "\n")
}

// checkProse removes citations that aren't allowed and says whether the text holds: at least one
// citation, no number that is not in the facts, not too long.
func checkProse(out string, allowed map[string]bool, facts string, maxWords int) (string, bool) {
	out = strings.TrimSpace(out)
	if out == "" || len(strings.Fields(out)) > maxWords {
		return "", false
	}
	n := 0
	fixed := cite.ReplaceAllStringFunc(out, func(m string) string {
		if allowed[strings.Trim(m, "[]")] {
			n++
			return m
		}
		return ""
	})
	fixed = strings.Join(strings.Fields(fixed), " ")
	if n == 0 {
		return "", false
	}
	known := map[string]bool{}
	for _, x := range numbersIn(facts) {
		known[x] = true
	}
	for _, x := range numbersIn(cite.ReplaceAllString(fixed, " ")) {
		if !known[x] {
			return "", false
		}
	}
	return fixed, true
}

func (r *run) summary(ctx context.Context, secs []Section, by [][]Claim) string {
	var top []Claim
	for _, cs := range by {
		if len(cs) > 0 {
			top = append(top, cs[0])
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Support > top[j].Support })
	if len(top) > 6 {
		top = top[:6]
	}
	var facts strings.Builder
	allowed := map[string]bool{}
	var all strings.Builder
	for _, c := range top {
		fmt.Fprintf(&facts, "[%d] %s\n", c.Source, c.Text)
		allowed[fmt.Sprint(c.Source)] = true
		all.WriteString(c.Text + " ")
	}
	for try := 0; try < 2 && len(top) > 0; try++ {
		out, err := r.ask(ctx, `Write the summary at the top of a research report from these key facts and nothing else: 3 to 5 sentences, 120 words at most. After each fact you use, put its source number in square brackets, like [3]. Do not add facts, numbers or names that are not in the facts.`,
			"Topic: "+r.st.Topic+"\n\nKey facts:\n"+facts.String(), nil, 300)
		if err != nil {
			break
		}
		if fixed, ok := checkProse(out, allowed, all.String(), 150); ok {
			return fixed
		}
	}
	// Without a model's prose: what was found, in the claims' own words.
	var b strings.Builder
	fmt.Fprintf(&b, "This research read %d pages and kept %d verified claims. The strongest findings: ", len(r.st.Sources), len(r.st.Claims))
	for i, c := range top {
		if i == 3 {
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s [%d]", strings.TrimRight(c.Text, "."), c.Source)
		b.WriteString(".")
	}
	return b.String()
}
