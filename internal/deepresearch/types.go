// Package deepresearch is the deep-research skill's pipeline (skills/deep-research): one model, one
// instance, and a research that still goes deep because the loop, the checks and the writing rules
// are code and the model gets one small job at a time.
//
// What makes it more than a search:
//   - an evidence ledger: every claim carries an exact quote from a fetched page, and a claim whose
//     quote isn't in the page, or doesn't say what the claim says, is dropped (code checks, not the model);
//   - rounds: after each round the gaps (questions nothing answered, claims only one site makes) become
//     the next round's questions, and links found in good pages are followed to primary sources;
//   - corroboration and conflict: the same claim from different sites counts as support, and sources
//     that give different numbers for the same thing are reported, with both quotes;
//   - writing from the ledger only: each section is written from its claims, a citation or a number
//     that isn't in them is caught, and a section that can't pass falls back to its claims verbatim;
//   - a budget and a state file: it stops cleanly on time or call limits and a stopped run resumes.
package deepresearch

import (
	"context"
	"encoding/json"
	"time"
)

// LLM is one small model job: a system prompt, the input, optionally a JSON schema for the answer.
type LLM interface {
	Ask(ctx context.Context, system, user string, schema json.RawMessage, maxTokens int) (string, error)
}

// Hit is one search result.
type Hit struct{ Title, URL, Snippet string }

// Page is fetched text. Links are the addresses in it (markdown links), for following to sources.
type Page struct {
	URL, Title, Text string
	Links            []Link
}

// Link is an outbound link with its anchor text.
type Link struct{ Text, URL string }

// Searcher finds pages for a query (the web-search skill's search).
type Searcher interface {
	Search(ctx context.Context, query string) ([]Hit, error)
}

// Fetcher reads one page as text.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (Page, error)
}

// Local searches the user's own documents (local-docs); optional. Each result is a passage as a
// Page whose URL names the file.
type Local interface {
	Search(ctx context.Context, query string, k int) ([]Page, error)
}

// Event is one line of progress for the person watching.
type Event struct {
	Stage   string // scope | search | read | gaps | write | done
	Round   int
	Done    int
	Total   int
	Message string
}

// Deps is everything the pipeline reaches outside itself for.
type Deps struct {
	LLM      LLM
	Search   Searcher
	Fetch    Fetcher
	Local    Local
	Progress func(Event)
	Now      func() time.Time
	// Wait pauses between a failed search and its retry (nil = time.After).
	Wait func(ctx context.Context, d time.Duration) error
}

// Config bounds a run. Zero values take the defaults.
type Config struct {
	Rounds            int           // research rounds (3)
	QuestionsPerRound int           // questions answered in a round (6)
	PagesPerQuestion  int           // pages read per question (3)
	MaxPages          int           // pages read in all (40)
	MaxPerHost        int           // pages from one site in all (3)
	LeadsPerRound     int           // links from good pages followed to primary sources per round (3)
	Budget            time.Duration // wall-clock limit (30 minutes)
	MaxCalls          int           // model calls in all (500)
	Dir               string        // where the state file is kept (empty: not kept, no resume)
	SearchWaits       []time.Duration
}

func (c Config) withDefaults() Config {
	def := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	def(&c.Rounds, 3)
	def(&c.QuestionsPerRound, 6)
	def(&c.PagesPerQuestion, 3)
	def(&c.MaxPages, 40)
	def(&c.MaxPerHost, 3)
	def(&c.LeadsPerRound, 3)
	def(&c.MaxCalls, 500)
	if c.Budget <= 0 {
		c.Budget = 30 * time.Minute
	}
	if c.SearchWaits == nil {
		c.SearchWaits = []time.Duration{0, 65 * time.Second}
	}
	return c
}

// ---- the state, which is also what resumes ----

// Question is something the research set out to answer.
type Question struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Round  int    `json:"round"`            // the round it was asked in
	Kind   string `json:"kind,omitempty"`   // "" | check (a claim looked at again) | gap (found missing after a round)
	Status string `json:"status"`           // open | done | empty
	Tries  int    `json:"tries,omitempty"`  // searches made for it
	Claims int    `json:"claims,omitempty"` // verified claims it produced
}

// Source is a page the research read.
type Source struct {
	ID      int       `json:"id"` // the number the report cites
	URL     string    `json:"url"`
	Title   string    `json:"title,omitempty"`
	Host    string    `json:"host"`
	Fetched time.Time `json:"fetched"`
	Local   bool      `json:"local,omitempty"` // one of the user's own documents
	Lead    bool      `json:"lead,omitempty"`  // reached by a link from another page
}

// Claim is a fact with the exact words that back it.
type Claim struct {
	ID       int    `json:"id"`
	Text     string `json:"text"`
	Quote    string `json:"quote"`
	Source   int    `json:"source"`
	Question int    `json:"question"`
	Round    int    `json:"round"`
	// Support is the number of distinct sites that make this claim or one close to it (1 = only its own).
	Support int `json:"support"`
}

// Lead is a link to a page that might be a primary source, waiting to be read.
type Lead struct {
	URL      string `json:"url"`
	Text     string `json:"text"`
	Question int    `json:"question"`
	From     string `json:"from"`
}

// Stats counts what happened, for the report's method section.
type Stats struct {
	Calls          int `json:"calls"`
	Searches       int `json:"searches"`
	SearchFailures int `json:"search_failures"`
	PagesRead      int `json:"pages_read"`
	ClaimsProposed int `json:"claims_proposed"`
	ClaimsVerified int `json:"claims_verified"`
	ClaimsDropped  int `json:"claims_dropped"` // the quote wasn't in the page, or didn't support the claim
	LeadsFollowed  int `json:"leads_followed"`
}

// State is the whole research so far.
type State struct {
	Topic      string     `json:"topic"`
	Started    time.Time  `json:"started"`
	Questions  []Question `json:"questions"`
	Sources    []Source   `json:"sources"`
	Claims     []Claim    `json:"claims"`
	Leads      []Lead     `json:"leads,omitempty"`
	RoundsDone int        `json:"rounds_done"`
	Stats      Stats      `json:"stats"`
	Stopped    string     `json:"stopped,omitempty"` // why it ended early: budget | calls | pages | search
}

// File is one file of the finished report folder.
type File struct{ Name, Content string }

// Result is a finished research.
type Result struct {
	Files      []File
	Summary    string
	State      State
	Conflict   int // places where sources disagree
	Unanswered int
}
