package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"noskill/internal/skills"
)

// Refinement (LEARNING.md §6, FR-L4): when a learned skill keeps failing, the author model revises
// its steps from the failed runs. The new version must pass the old tests plus a case made from a
// failed run, then goes on trial in .next while the current version stays in place.

// StepsOf reads the numbered list under "## Steps" in a SKILL.md.
func StepsOf(md string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "## "):
			in = strings.EqualFold(strings.TrimSpace(t[3:]), "steps")
		case in && t != "" && t[0] >= '0' && t[0] <= '9':
			if s := strings.TrimSpace(strings.TrimLeft(t, "0123456789.) ")); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// WithSteps replaces the numbered list under "## Steps"; the rest of the file is kept.
func WithSteps(md string, steps []string) (string, error) {
	lines := strings.SplitAfter(md, "\n")
	start, end := -1, -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if start < 0 {
			if strings.HasPrefix(t, "## ") && strings.EqualFold(strings.TrimSpace(t[3:]), "steps") {
				start = i + 1
			}
			continue
		}
		if t != "" && t[0] >= '0' && t[0] <= '9' {
			end = i + 1
			continue
		}
		if t != "" { // the next section or comment ends the list
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("SKILL.md has no Steps section")
	}
	if end < 0 {
		end = start
	}
	return strings.Join(lines[:start], "") + numbered(steps) + strings.Join(lines[end:], ""), nil
}

// Failure is a failed run of the skill, as the reviser sees it.
type Failure struct {
	Request string
	Calls   string // compressed, with errors and refusals
	Why     string // what the outcome source said, if anything
}

// FailureOf summarises a failed run's trace.
func FailureOf(tr *Trace) Failure {
	var b strings.Builder
	for i, a := range tr.Actions {
		res := clip(a.Result, 160)
		switch {
		case a.Denied:
			res = "refused"
		case a.Err != "":
			res = "error: " + clip(a.Err, 160)
		}
		fmt.Fprintf(&b, "%d. %s(%s) → %s\n", i+1, a.Tool, clip(compactJSON(a.Args), 200), res)
	}
	why := tr.Outcome.Note
	if why == "" && tr.Stop != "" && tr.Stop != "answered" {
		why = "the run stopped: " + tr.Stop
	}
	return Failure{Request: tr.Request, Calls: b.String(), Why: why}
}

// Revise asks for new steps after failures and, on later attempts, failed tests. It doesn't fall
// back to anything: without usable steps there is no refinement.
func (au *Author) Revise(ctx context.Context, current []string, fails []Failure, tests []CaseResult) ([]string, error) {
	var b strings.Builder
	for i, f := range fails[:min(len(fails), 2)] {
		fmt.Fprintf(&b, "Request %d: %q\nCalls:\n%s", i+1, clip(f.Request, 300), orNone(clip(f.Calls, 1200)))
		if f.Why != "" {
			fmt.Fprintf(&b, "What went wrong: %s\n", clip(f.Why, 200))
		}
		b.WriteString("\n")
	}
	for _, r := range tests {
		if !r.Passed {
			fmt.Fprintf(&b, "Test %q of your last revision failed: %s\nCalls made:\n%s\n", r.Case, r.Why, orNone(clip(r.Calls, 600)))
		}
	}
	prompt := fmt.Sprintf(`An assistant followed these steps:
%s
and the runs below failed.
%s
Rewrite the steps so these requests succeed, without breaking what the steps already did right. Keep them short, one tool call per step, tool names in backticks, changing values as {name}.
Reply with JSON: {"steps": ["..."]}`, numbered(current), clip(b.String(), 3500))
	out, err := au.LLM(ctx, prompt, stepsSchema, 600)
	if err != nil {
		return nil, err
	}
	var r struct{ Steps []string }
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return nil, err
	}
	var steps []string
	for _, s := range r.Steps {
		if s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "0123456789.) ")); s != "" {
			steps = append(steps, s)
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no steps in the reply")
	}
	if v := runValue(steps, current, fails); v != "" {
		return nil, fmt.Errorf("the steps have %q from a failed run written in them", v)
	}
	return steps[:min(len(steps), maxSteps)], nil
}

// runValue is a value from a failed run's request (a path or what RequestValues finds) that the
// revised steps write in and the current steps don't, or "". A step that names one run's folder
// would send the next run there (FR-L4).
func runValue(steps, current []string, fails []Failure) string {
	text, cur := strings.ToLower(strings.Join(steps, "\n")), strings.ToLower(strings.Join(current, "\n"))
	for _, f := range fails {
		vals := RequestValues(f.Request)
		for _, w := range strings.Fields(f.Request) {
			if w = strings.Trim(w, ".,:;!?()\"'`"); strings.ContainsAny(w, `/\`) {
				vals = append(vals, strings.ToLower(w))
			}
		}
		for _, v := range vals {
			if len(v) >= 3 && strings.Contains(text, v) && !strings.Contains(cur, v) {
				return v
			}
		}
	}
	return ""
}

// AddFailureCase adds a required test made from a failed run to cases.yaml: the same request,
// replayed from that run's recorded results, must now end with an answer. It returns the new cases.yaml
// and the fixtures file to write under tests/.
func AddFailureCase(casesYAML []byte, tr *Trace) (cases []byte, fixturePath string, fixtures []byte, err error) {
	var list []Case
	if err := yaml.Unmarshal(casesYAML, &list); err != nil {
		return nil, "", nil, fmt.Errorf("tests/cases.yaml: %w", err)
	}
	id := strings.ToLower(tr.RunID)
	if len(id) > 10 {
		id = id[len(id)-10:]
	}
	name := "failed-" + id
	for _, c := range list {
		if c.Name == name {
			return casesYAML, "", nil, nil
		}
	}
	var fx []Fixture
	for _, a := range tr.Calls() {
		fx = append(fx, Fixture{Tool: a.Tool, Args: a.Args, Result: a.Result})
	}
	fixturePath = "fixtures/" + name + ".json"
	list = append(list, Case{Name: name, Input: tr.Request, Network: "recorded", Fixtures: fixturePath, Expect: Expect{Answered: true}, Required: true})
	if cases, err = yaml.Marshal(list); err != nil {
		return nil, "", nil, err
	}
	if fixtures, err = json.MarshalIndent(fx, "", "  "); err != nil {
		return nil, "", nil, err
	}
	return cases, fixturePath, fixtures, nil
}

// AccessGrowth lists what a new version may do that the old one couldn't: more network, programs,
// secrets, tools, dependencies or folders. Any of it means the refinement asks again before use.
func AccessGrowth(old, cur *skills.Skill) []string {
	var out []string
	add := func(what string, a, b []string) {
		for _, x := range b {
			if !slices.Contains(a, x) {
				out = append(out, what+" "+x)
			}
		}
	}
	op, cp := old.Meta.Permissions, cur.Meta.Permissions
	add("network", op.Network, cp.Network)
	add("secret", op.Secrets, cp.Secrets)
	add("skill", old.Meta.DependsOn, cur.Meta.DependsOn)
	add("folder", op.Filesystem, cp.Filesystem)
	toolNames := func(s *skills.Skill) (n []string) {
		for _, t := range s.Meta.ToolRefs {
			n = append(n, t.Path+t.Builtin)
			if t.MCP != nil {
				n = append(n, fmt.Sprintf("mcp %+v", *t.MCP))
			}
		}
		return n
	}
	add("tool", toolNames(old), toolNames(cur))
	if (cp.Exec && !op.Exec) || (cp.Subprocess && !op.Subprocess) {
		out = append(out, "running programs")
	}
	return out
}
