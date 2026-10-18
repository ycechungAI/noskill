package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"noskill/internal/backends"
	"noskill/internal/skills"
)

// The verification gate (LEARNING.md §4, FR-L3). Static checks are here; the app replays the
// tests through the normal run path with Replay standing in for the real tools, so no test touches
// real files or the network.

const MaxSkillLines = 60

// Check is the result of the static checks.
type Check struct {
	Problems    []string // fixable by the author (size, secrets, load errors)
	Escalations []string // permissions beyond the source trace: always need a human
}

func (c Check) OK() bool { return len(c.Problems) == 0 && len(c.Escalations) == 0 }

// StaticCheck checks a draft against the trace it came from. known are secret values held by
// noskill's secrets store; none may appear in the skill's files.
func StaticCheck(sk *skills.Skill, issues []skills.Issue, files map[string][]byte, tr *Trace, home string, known []string) Check {
	c := FileChecks(sk, issues, files, known)
	if sk == nil {
		return c
	}
	p := sk.Meta.Permissions
	if len(p.Network) > 0 {
		c.Escalations = append(c.Escalations, "network access ("+strings.Join(p.Network, ", ")+")")
	}
	if p.Exec || p.Subprocess {
		c.Escalations = append(c.Escalations, "running programs")
	}
	if len(p.Secrets) > 0 {
		c.Escalations = append(c.Escalations, "secrets ("+strings.Join(p.Secrets, ", ")+")")
	}
	if len(sk.Meta.ToolRefs) > 0 && !scriptToolOnly(sk) {
		c.Escalations = append(c.Escalations, "tools of its own")
	}
	au := &Author{Home: home}
	deps, used := au.scopes(tr, tr.Calls())
	for _, d := range sk.Meta.DependsOn {
		if !slices.Contains(deps, d) {
			c.Escalations = append(c.Escalations, "skill "+d+", which the run didn't use")
		}
	}
	for _, decl := range p.Filesystem {
		if !scopeCovered(decl, used) {
			c.Escalations = append(c.Escalations, "folder access "+decl)
		}
	}
	return c
}

// FileChecks are the checks that don't depend on a trace: the skill loads, SKILL.md is short, and
// no file holds a secret.
func FileChecks(sk *skills.Skill, issues []skills.Issue, files map[string][]byte, known []string) Check {
	var c Check
	for _, is := range issues {
		if is.Level == "error" {
			c.Problems = append(c.Problems, "doesn't load: "+is.Msg)
		}
	}
	if sk == nil {
		return c
	}
	if n := strings.Count(strings.TrimRight(string(files["SKILL.md"]), "\n"), "\n") + 1; n >= MaxSkillLines {
		c.Problems = append(c.Problems, fmt.Sprintf("SKILL.md has %d lines; keep it under %d", n, MaxSkillLines))
	}
	for name, b := range files {
		if what := findSecret(string(b), known, !strings.HasPrefix(name, "tests/fixtures/")); what != "" {
			c.Problems = append(c.Problems, fmt.Sprintf("%s contains %s", name, what))
		}
	}
	return c
}

// scopeCovered: decl is one of used, or inside one with at least the same access.
func scopeCovered(decl string, used []string) bool {
	acc, p, ok := strings.Cut(decl, ":")
	if !ok {
		return false
	}
	for _, u := range used {
		uacc, up, _ := strings.Cut(u, ":")
		if (uacc == acc || uacc == "write") && under(p, up) {
			return true
		}
	}
	return false
}

var secretPrefixes = []string{"sk-", "ghp_", "github_pat_", "xoxb-", "xoxp-", "AKIA", "AIza", "-----BEGIN"}

// findSecret names what looks like a credential in text: a known secret value, a well-known key
// prefix, or (when random is set) a long random-looking token.
func findSecret(text string, known []string, random bool) string {
	for _, k := range known {
		if len(k) >= 6 && strings.Contains(text, k) {
			return "a stored secret's value"
		}
	}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(" \t\n\"'`,;()[]{}<>=", r) }) {
		for _, p := range secretPrefixes {
			if strings.HasPrefix(f, p) && len(f) >= len(p)+12 {
				return "what looks like a key (" + p + "…)"
			}
		}
		if random && len(f) >= 32 && !strings.ContainsAny(f, `/\.:`) && entropy(f) > 4.0 {
			return "a long random-looking token"
		}
	}
	return ""
}

func entropy(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// ---- replay ----

// Replay answers tool calls from recorded fixtures (agent.Tools). Calls are matched by tool name in
// order; once a tool's recordings are used up, its last result is served again.
type Replay struct {
	Inner    interface{ Defs() []backends.ToolDef }
	Fixtures []Fixture

	mu    sync.Mutex
	used  []bool
	Calls []Action
}

func (r *Replay) Defs() []backends.ToolDef { return r.Inner.Defs() }

func (r *Replay) Call(_ context.Context, name string, args json.RawMessage) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.used == nil {
		r.used = make([]bool, len(r.Fixtures))
	}
	r.Calls = append(r.Calls, Action{Step: len(r.Calls) + 1, Tool: name, Args: args})
	last := -1
	for i, f := range r.Fixtures {
		if !sameTool(f.Tool, name) {
			continue
		}
		last = i
		if !r.used[i] {
			r.used[i] = true
			return f.Result, nil
		}
	}
	if last >= 0 {
		return r.Fixtures[last].Result, nil
	}
	return "", fmt.Errorf("%s isn't part of this skill's recorded run; use the steps' tools", name)
}

// sameTool: names match, allowing for the skill prefix added when two skills share a tool name.
func sameTool(recorded, called string) bool {
	return recorded == called || strings.HasSuffix(called, "__"+recorded) || strings.HasSuffix(recorded, "__"+called)
}

// Outcome of one replayed case.
type CaseResult struct {
	Case     string
	Required bool
	Passed   bool
	Why      string // what didn't match, for the report and self-repair
	Calls    string // the calls made, compressed
}

// Judge compares a replay with the case's expectations.
func Judge(c Case, calls []Action, answered bool, home string) CaseResult {
	res := CaseResult{Case: c.Name, Required: c.Required, Passed: true}
	var names []string
	var b strings.Builder
	for i, a := range calls {
		names = append(names, a.Tool)
		fmt.Fprintf(&b, "%d. %s(%s)\n", i+1, a.Tool, clip(compactJSON(a.Args), 160))
	}
	res.Calls = b.String()
	fail := func(f string, args ...any) {
		res.Passed = false
		if res.Why != "" {
			res.Why += "; "
		}
		res.Why += fmt.Sprintf(f, args...)
	}
	i := 0
	for _, n := range names {
		if i < len(c.Expect.Tools) && sameTool(c.Expect.Tools[i], n) {
			i++
		}
	}
	if i < len(c.Expect.Tools) {
		fail("expected the tools %s in that order; got %s", strings.Join(c.Expect.Tools, ", "), orNone(strings.Join(names, ", ")))
	}
	var all strings.Builder
	for _, a := range calls {
		all.WriteString(strings.ToLower(string(a.Args)))
		all.WriteString("\n")
	}
	for _, v := range c.Expect.ArgsContain {
		forms := []string{strings.ToLower(jsonInner(v))}
		if home != "" && strings.HasPrefix(v, "~") {
			forms = append(forms, strings.ToLower(jsonInner(home+v[1:])))
		}
		if !slices.ContainsFunc(forms, func(f string) bool { return strings.Contains(all.String(), f) }) {
			fail("no call used %q", v)
		}
	}
	if c.Expect.Answered && !answered {
		fail("the run didn't end with an answer")
	}
	return res
}

// Passed applies the gate's rule: the replay of the source run and every required case must pass,
// and so must at least half of the other variations (small models vary from run to run; the trial
// period catches the rest).
func Passed(results []CaseResult) bool {
	if len(results) == 0 || !results[0].Passed {
		return false
	}
	ok, other := 0, 0
	for _, r := range results[1:] {
		switch {
		case r.Required && !r.Passed:
			return false
		case r.Required:
		case r.Passed:
			ok, other = ok+1, other+1
		default:
			other++
		}
	}
	return ok*2 >= other
}

// scriptToolOnly: the skill's own tools are one pass-d script (LEARNING.md §3 pass d): plain
// python with no packages, which runs in the script sandbox without network and changes files
// only through the broker's ops (ADR-0013). Anything else needs a human.
func scriptToolOnly(sk *skills.Skill) bool {
	if len(sk.Tools) != 1 || len(sk.Meta.ToolRefs) != 1 {
		return false
	}
	t := sk.Tools[0]
	return t.Runtime == "python" && len(t.Deps) == 0 && t.MCP == nil && strings.HasSuffix(t.Entry, ".py")
}
