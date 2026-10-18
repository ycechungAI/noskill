package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Skill optimisation (FR-L9, ADR-0014): a GEPA-style search over a skill's instructions. The
// author model reads a candidate's failed tasks and rewrites the instructions; every rewrite is
// scored on all training tasks, the best ones form a Pareto pool, and the finalist must beat the
// original on the training tasks and match it on tasks the search never saw. Only the Markdown
// body changes: the front-matter (permissions, tools) is the caller's and stays as it is.

// MinTrainTasks is how many training tasks a skill needs before it is worth optimising.
const MinTrainTasks = 5

// TaskResult is one task's outcome with some instructions.
type TaskResult struct {
	ID     string `json:"id"`
	Passed bool   `json:"passed"`
	Steps  int    `json:"steps"`
	Why    string `json:"why,omitempty"`    // why the check failed
	Prompt string `json:"prompt,omitempty"` // what was asked
	Want   string `json:"want,omitempty"`   // what the check wanted, in words
	Calls  string `json:"calls,omitempty"`  // the calls made, compressed
}

// Score sums task results.
type Score struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
	Steps  int `json:"steps"`
}

func (s Score) String() string {
	return fmt.Sprintf("%d/%d passed, %d steps", s.Passed, s.Total, s.Steps)
}

func scoreOf(rs []TaskResult) Score {
	s := Score{Total: len(rs)}
	for _, r := range rs {
		if r.Passed {
			s.Passed++
		}
		s.Steps += r.Steps
	}
	return s
}

// better: more passed, then fewer steps.
func (s Score) better(o Score) bool {
	return s.Passed > o.Passed || (s.Passed == o.Passed && s.Steps < o.Steps)
}

// NeedMore is how many more tasks a challenger must pass to be clearly better: max(2, 10%).
func NeedMore(base Score) int { return max(2, int(math.Ceil(0.1*float64(base.Total)))) }

// ClearlyBetter is ADR-0014's bar, which trained models meet too (ADR-0015): max(2, 10%) more
// tasks passed, or as many (at least one) with at least 15% fewer steps. Fewer steps with nothing
// passed is a run that gave up early, not a faster one.
func ClearlyBetter(s, base Score) bool {
	return s.Passed >= base.Passed+NeedMore(base) ||
		(s.Passed >= base.Passed && s.Passed > 0 && base.Steps > 0 && float64(s.Steps) <= 0.85*float64(base.Steps))
}

// Candidate is one version of the instructions the search tried.
type Candidate struct {
	Body    string       `json:"body"`
	Parent  int          `json:"parent"` // index of the candidate it was made from; -1 for the original
	Results []TaskResult `json:"results,omitempty"`
	Score   Score        `json:"score"`
	Dropped string       `json:"dropped,omitempty"` // why the rewrite wasn't scored
}

// Optimiser is one search.
type Optimiser struct {
	Skill string
	Base  string   // the original instructions
	Train []string // task ids the search learns from
	Held  []string // task ids it never sees; run for the original and the finalist only
	// Eval runs the tasks with these instructions.
	Eval func(ctx context.Context, body string, ids []string) ([]TaskResult, error)
	// LLM is the author model (JSON reply, as Author.LLM).
	LLM    func(ctx context.Context, prompt string, schema json.RawMessage, maxTokens int) (string, error)
	Budget int // rewrites to try (default 4)
	// Values are words from the training tasks (file names, request values) the instructions must
	// not pick up: instructions that name a test's files score well and help nobody.
	Values []string
	// Check loads the skill with these instructions; an error drops the candidate.
	Check    func(body string) error
	MaxLines int // a hard cap on the whole body's non-empty lines (0 = only the growth limit)
	Progress func(format string, a ...any)
}

// OptimiseResult is what the search found.
type OptimiseResult struct {
	Candidates []Candidate `json:"candidates"`
	Best       int         `json:"best"` // index of the finalist; -1 when no rewrite beat the original
	BaseHeld   Score       `json:"base_held"`
	BestHeld   Score       `json:"best_held"`
	Accepted   bool        `json:"accepted"`
	Why        string      `json:"why"`
}

// Finalist returns the accepted candidate.
func (r *OptimiseResult) Finalist() (Candidate, bool) {
	if !r.Accepted || r.Best < 0 {
		return Candidate{}, false
	}
	return r.Candidates[r.Best], true
}

func (o *Optimiser) progress(format string, a ...any) {
	if o.Progress != nil {
		o.Progress(format, a...)
	}
}

// ErrTooFewTasks: the skill hasn't enough training tasks to score rewrites on.
var ErrTooFewTasks = errors.New("not enough checked tasks")

// Run does the search.
func (o *Optimiser) Run(ctx context.Context) (*OptimiseResult, error) {
	if len(o.Train) < MinTrainTasks {
		return nil, fmt.Errorf("%w: %s has %d training tasks and needs %d; add some with `noskill eval pin`", ErrTooFewTasks, o.Skill, len(o.Train), MinTrainTasks)
	}
	budget := o.Budget
	if budget <= 0 {
		budget = 4
	}
	res := &OptimiseResult{Best: -1}
	o.progress("scoring the current instructions on %d tasks", len(o.Train))
	base, err := o.Eval(ctx, o.Base, o.Train)
	if err != nil {
		return nil, err
	}
	res.Candidates = append(res.Candidates, Candidate{Body: o.Base, Parent: -1, Results: base, Score: scoreOf(base)})
	o.progress("current instructions: %s", res.Candidates[0].Score)
	used := map[int]int{}
	for i := 0; i < budget && ctx.Err() == nil; i++ {
		parent := paretoPick(res.Candidates, used)
		used[parent]++
		o.progress("rewrite %d of %d (from version %d)", i+1, budget, parent)
		body, err := o.reflect(ctx, res.Candidates[parent], "")
		if err == nil {
			// Small models often copy a request's names or drop a rule; one try with the reason.
			if gerr := GuardBody(o.Base, body, o.Values, o.MaxLines); gerr != nil {
				o.progress("  refused (%s); asking once more", gerr)
				body, err = o.reflect(ctx, res.Candidates[parent], gerr.Error())
			}
		}
		c := Candidate{Body: body, Parent: parent}
		if err == nil {
			err = GuardBody(o.Base, body, o.Values, o.MaxLines)
		}
		if err == nil && o.Check != nil {
			err = o.Check(body)
		}
		if err == nil && seen(res.Candidates, body) {
			err = errors.New("same as a version already tried")
		}
		if err != nil {
			c.Dropped = err.Error()
			o.progress("  dropped: %s", c.Dropped)
			res.Candidates = append(res.Candidates, c)
			continue
		}
		rs, err := o.Eval(ctx, body, o.Train)
		if err != nil {
			return res, err
		}
		c.Results, c.Score = rs, scoreOf(rs)
		o.progress("  %s", c.Score)
		res.Candidates = append(res.Candidates, c)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	baseScore := res.Candidates[0].Score
	for i, c := range res.Candidates[1:] {
		if c.Dropped == "" && (res.Best < 0 || c.Score.better(res.Candidates[res.Best].Score)) {
			res.Best = i + 1
		}
	}
	if res.Best < 0 {
		res.Why = "no rewrite could be scored"
		return res, nil
	}
	best := res.Candidates[res.Best].Score
	if !ClearlyBetter(best, baseScore) {
		res.Why = fmt.Sprintf("the best rewrite (%s) isn't enough better than the current instructions (%s): it needs %d more tasks passed, or as many with 15%% fewer steps", best, baseScore, NeedMore(baseScore))
		return res, nil
	}
	if len(o.Held) == 0 {
		res.Accepted = true
		res.Why = fmt.Sprintf("better on the training tasks (%s against %s); there are no held-out tasks to check it on", best, baseScore)
		return res, nil
	}
	o.progress("checking on %d held-out tasks", len(o.Held))
	bh, err := o.Eval(ctx, o.Base, o.Held)
	if err != nil {
		return res, err
	}
	fh, err := o.Eval(ctx, res.Candidates[res.Best].Body, o.Held)
	if err != nil {
		return res, err
	}
	res.BaseHeld, res.BestHeld = scoreOf(bh), scoreOf(fh)
	if res.BestHeld.Passed < res.BaseHeld.Passed {
		res.Why = fmt.Sprintf("better on the training tasks (%s against %s) but worse on the held-out tasks (%s against %s): it learned the tests, not the skill", best, baseScore, res.BestHeld, res.BaseHeld)
		return res, nil
	}
	res.Accepted = true
	res.Why = fmt.Sprintf("training tasks %s (was %s); held-out tasks %s (was %s)", best, baseScore, res.BestHeld, res.BaseHeld)
	return res, nil
}

func seen(cs []Candidate, body string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, c := range cs {
		if norm(c.Body) == norm(body) {
			return true
		}
	}
	return false
}

// paretoPick chooses the parent of the next rewrite: among the candidates that are best on at least
// one task (passed it, in the fewest steps), the one best on the most tasks, less how often it has
// already been a parent, so the search doesn't keep rewriting one version.
func paretoPick(cs []Candidate, used map[int]int) int {
	wins := map[int]int{}
	if len(cs) > 0 {
		for t := range cs[0].Results {
			best, bestSteps := -1, 0
			for i, c := range cs {
				if c.Dropped != "" || t >= len(c.Results) || !c.Results[t].Passed {
					continue
				}
				if best < 0 || c.Results[t].Steps < bestSteps {
					best, bestSteps = i, c.Results[t].Steps
				}
			}
			if best >= 0 {
				wins[best]++
			}
		}
	}
	pick, pickVal := 0, math.MinInt
	for i, c := range cs {
		if c.Dropped != "" || (wins[i] == 0 && i != 0) {
			continue
		}
		v := wins[i]*4 + c.Score.Passed - used[i]*3
		if v > pickVal {
			pick, pickVal = i, v
		}
	}
	return pick
}

// changesSchema is the reflection's reply: a few line changes, not the whole text. Asked for the
// whole instructions as one JSON string, qwen3:4b looped on a rule ("Never move a file to a path
// that is not in the same …") until its tokens ran out in half the replies; without a schema it
// reasoned aloud instead of writing instructions.
var changesSchema = json.RawMessage(`{"type":"object","properties":{"changes":{"type":"array","maxItems":3,"items":{"type":"object","properties":{"heading":{"type":"string"},"replace":{"type":"string"},"with":{"type":"string"}},"required":["heading","replace","with"]}}},"required":["changes"]}`)

// Change is one line the author model wants changed: replace that line (or, when Replace is
// empty or matches no line, add With at the end of the section under Heading).
type Change struct {
	Heading string `json:"heading"`
	Replace string `json:"replace"`
	With    string `json:"with"`
}

const fixedMark = "(fixed) "

var listMarker = regexp.MustCompile(`^\s*(?:[-*]|(\d+)\.)\s+`)

// lineText is a line without its list marker or a copied (fixed) mark, trimmed.
func lineText(l string) string {
	l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), strings.TrimSpace(fixedMark)))
	l = strings.TrimSpace(listMarker.ReplaceAllString(l, ""))
	return strings.TrimSpace(strings.TrimPrefix(l, strings.TrimSpace(fixedMark)))
}

func headingText(l string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#")))
}

// ApplyChanges puts the author model's changes into body and says how many took. A protected line
// (ProtectedLines) is never replaced: when the new line is that rule with more after it, the rest
// becomes a line of its own after the rule (small models extend a rule rather than add one), and
// anything else aimed at it is dropped.
func ApplyChanges(body string, cs []Change) (string, int) {
	lines := strings.Split(body, "\n")
	n := 0
	for _, c := range cs {
		with := lineText(c.With)
		if with == "" || strings.HasPrefix(with, "#") || slices.ContainsFunc(lines, func(l string) bool { return strings.EqualFold(lineText(l), with) }) {
			continue // nothing, a heading, or a line it already has
		}
		at := -1 // the line to add after
		if rep := strings.ToLower(lineText(c.Replace)); rep != "" {
			for i, l := range lines {
				if strings.HasPrefix(strings.TrimSpace(l), "#") || strings.ToLower(lineText(l)) != rep {
					continue
				}
				orig := lineText(l)
				if !protectedWord.MatchString(l) {
					lines[i] = l[:len(l)-len(strings.TrimLeft(l, " \t"))] + markerOf(l) + with
					n++
					at = -2
				} else if extra := strings.TrimSpace(strings.TrimPrefix(with, orig)); strings.HasPrefix(with, orig) && extra != "" {
					with, at = strings.TrimLeft(extra, ".;,: "), i
				} else {
					at = -2 // a rule it may not change
				}
				break
			}
		}
		if at == -2 || with == "" {
			continue
		}
		if at < 0 {
			at = sectionEnd(lines, c.Heading)
		}
		line := nextMarker(lines, at) + with
		lines = append(lines[:at+1], append([]string{line}, lines[at+1:]...)...)
		n++
	}
	return strings.Join(lines, "\n"), n
}

// sectionEnd is the last non-empty line of the section under heading, or of the whole body when
// no heading matches.
func sectionEnd(lines []string, heading string) int {
	h, want := -1, headingText(heading)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") && want != "" && headingText(l) == want {
			h = i
			break
		}
	}
	end := len(lines)
	if h >= 0 {
		for i := h + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
				end = i
				break
			}
		}
	}
	last := max(h, 0)
	for i := max(h, 0); i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
		}
	}
	return last
}

func markerOf(l string) string {
	if m := listMarker.FindString(strings.TrimLeft(l, " \t")); m != "" {
		return m
	}
	return ""
}

// nextMarker is the list marker for a line added after lines[at]: the next number in a numbered
// list, else a dash.
func nextMarker(lines []string, at int) string {
	if at >= 0 && at < len(lines) {
		if m := listMarker.FindStringSubmatch(lines[at]); m != nil && m[1] != "" {
			k, _ := strconv.Atoi(m[1])
			return strconv.Itoa(k+1) + ". "
		}
	}
	return "- "
}

// markFixed shows the protected lines to the author model.
func markFixed(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") && protectedWord.MatchString(l) {
			lines[i] = fixedMark + l
		}
	}
	return strings.Join(lines, "\n")
}

// reflect asks the author model for a rewrite that fixes the parent's failures (or, when it has
// none, needs fewer steps).
// reflect asks the author model for a rewrite of parent. refused, when set, is why its last
// rewrite was turned down.
func (o *Optimiser) reflect(ctx context.Context, parent Candidate, refused string) (string, error) {
	var b strings.Builder
	var fails []TaskResult
	for _, r := range parent.Results {
		if !r.Passed {
			fails = append(fails, r)
		}
	}
	ask := "Work out what step or rule was missing for these requests, then suggest up to 3 changes that add it, without breaking what already works."
	if len(fails) == 0 {
		// Everything passed: the slowest tasks are what to improve.
		rs := append([]TaskResult(nil), parent.Results...)
		for i := 0; i < len(rs); i++ {
			for j := i + 1; j < len(rs); j++ {
				if rs[j].Steps > rs[i].Steps {
					rs[i], rs[j] = rs[j], rs[i]
				}
			}
		}
		fails = rs
		ask = "These succeeded but took many steps. Suggest up to 3 changes so requests like these need fewer tool calls, without breaking them."
		b.WriteString("Requests that took the most steps:\n")
	} else {
		b.WriteString("Requests it failed:\n")
	}
	for i, f := range fails[:min(3, len(fails))] {
		fmt.Fprintf(&b, "\nRequest %d: %q\n", i+1, clip(f.Prompt, 300))
		if f.Want != "" {
			fmt.Fprintf(&b, "It had to: %s\n", clip(f.Want, 200))
		}
		fmt.Fprintf(&b, "Calls made (%d steps):\n%s", f.Steps, orNone(clip(f.Calls, 700)))
		if !f.Passed && f.Why != "" {
			fmt.Fprintf(&b, "Why it failed: %s\n", clip(f.Why, 200))
		}
	}
	prompt := fmt.Sprintf(`An assistant follows these instructions for the skill %q:
---
%s
---
%s
%s Each change is one line: the heading it goes under, the existing line it replaces ("" to add a new line), and the new line.
- Lines marked (fixed) can't be replaced.
- Only name tools and features already in the instructions.
- Write rules that fit any request: don't name the files, folders or values in these requests.
%sReply with JSON: {"changes": [{"heading": "...", "replace": "...", "with": "..."}]}`, o.Skill, markFixed(parent.Body), clip(b.String(), 3000), ask, refusedNote(refused))
	out, err := o.LLM(ctx, prompt, changesSchema, 600)
	if err != nil {
		return "", err
	}
	var r struct{ Changes []Change }
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return "", fmt.Errorf("the reply isn't the JSON asked for: %w", err)
	}
	body, n := ApplyChanges("\n"+strings.TrimSpace(parent.Body)+"\n", r.Changes)
	if n == 0 {
		return "", errors.New("none of its changes could be made (each was empty or aimed at a fixed rule)")
	}
	return body, nil
}

func refusedNote(why string) string {
	if why == "" {
		return ""
	}
	return fmt.Sprintf("Your last changes were turned down: %s. Suggest them again without that; use general words (\"the file\", \"the folder\") instead of names from the requests.\n", why)
}

// lineLimit is the most non-empty lines a rewrite may have: 25% more than the original, at most 8
// lines more, and never above max (when set).
func lineLimit(orig string, maxLines int) int {
	n := countLines(orig)
	l := n + min(8, max(1, n/4))
	if maxLines > 0 {
		l = min(l, maxLines)
	}
	return l
}

func countLines(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

var protectedWord = regexp.MustCompile(`(?i)\b(never|always|don't|do not|only|must)\b`)

// ProtectedLines are the lines of the instructions that say what must never or always happen:
// a rewrite keeps each word for word.
func ProtectedLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") && protectedWord.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

// GuardBody checks a rewrite against the original (ADR-0014): protected lines kept, not much
// longer, and no values from the training tasks it didn't have.
func GuardBody(orig, cand string, values []string, maxLines int) error {
	if strings.TrimSpace(cand) == "" {
		return errors.New("empty")
	}
	if strings.HasPrefix(strings.TrimSpace(cand), "---") {
		return errors.New("it starts with front-matter; only the instructions may change")
	}
	for _, p := range ProtectedLines(orig) {
		if !strings.Contains(cand, p) {
			return fmt.Errorf("it dropped or changed the rule %q", clip(p, 80))
		}
	}
	if n, l := countLines(cand), lineLimit(orig, maxLines); n > l {
		return fmt.Errorf("%d lines; at most %d", n, l)
	}
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if len(v) >= 3 && hasWord(cand, v) && !hasWord(orig, v) {
			return fmt.Errorf("it names %q from a training task", v)
		}
	}
	return nil
}

// hasWord says whether s holds w as a whole word (any case): "log" isn't in "catalog".
func hasWord(s, w string) bool {
	return regexp.MustCompile(`(?i)(^|[^\pL\pN_])` + regexp.QuoteMeta(w) + `($|[^\pL\pN_])`).MatchString(s)
}

// SplitSkillMD splits a SKILL.md into its front-matter block (with both --- lines) and the body.
func SplitSkillMD(raw []byte) (front, body string, err error) {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", "", errors.New("SKILL.md has no front-matter")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		if strings.HasSuffix(s, "\n---") {
			return s, "", nil
		}
		return "", "", errors.New("SKILL.md's front-matter isn't closed")
	}
	cut := 4 + end + len("\n---\n")
	return s[:cut], s[cut:], nil
}

// JoinSkillMD puts a SKILL.md back together from the original front-matter and new instructions.
func JoinSkillMD(front, body string) []byte {
	if !strings.HasPrefix(body, "\n") {
		body = "\n" + body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return []byte(front + body)
}
