package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"noskill/internal/decide"
)

// The skill-author passes (LEARNING.md §3, skills/skill-author). Go drives them: each model call is
// one small pass with constrained JSON output, which 4B–8B models follow far better than one
// "write a skill" prompt. Values, permissions and tests come from the trace in Go, not from the
// model, so a draft can't claim more than the run did.

// LLM is one constrained call on the author model: the reply is JSON matching schema, or plain
// text when schema is nil (pass d writes code, which small models garble inside a JSON string).
type LLM func(ctx context.Context, prompt string, schema json.RawMessage, maxTokens int) (string, error)

type Author struct {
	LLM    LLM
	Decide decide.Decider         // is_parameter; nil = unclear values become inputs
	Taken  func(name string) bool // skill names already in use
	Home   string                 // shown as ~ in permissions and defaults
	// SkillOf names the skill that owns a tool, for traces written before tool_call carried it.
	SkillOf func(tool string) string
	// Script runs pass (d)'s tools in the sandbox; nil = no script tools (no OS sandbox).
	Script ScriptRun
	// Confirm says whether a tool asks before it acts (a skill's `confirm` list).
	Confirm func(tool string) bool
}

// Input is a value that changes between uses of the skill.
type Input struct {
	Name        string
	Default     string
	FromRequest bool // it appeared in the user's request, so a new request names it
}

// Draft is a skill before verification.
type Draft struct {
	Name        string
	Description string
	Triggers    []string
	TaskTypes   []string
	Inputs      []Input
	Steps       []string
	DependsOn   []string
	Filesystem  []string
	Cases       []Case
	Fixtures    []Fixture
	SourceRun   string
	Pattern     string
	Notes       []string // passes that fell back to a default
	// Tool is pass (d)'s script tool, when the chore is deterministic and the tool did it right.
	Tool *ScriptTool
	// Example is the source request with its values cut out, added to the description so that
	// requests worded like it find the skill (block 2c: other prefixes found nothing).
	Example string

	plainSteps   []string // the steps without the tool, for when the model can't use it
	plainInputs  []Input
	toolRepaired bool
}

// Case is one entry of tests/cases.yaml. Learned skills use skill-level cases replayed against
// recorded tool results (SKILL_FORMAT.md "Tests").
type Case struct {
	Name     string `yaml:"name"`
	Input    string `yaml:"input"`
	Network  string `yaml:"network"`
	Fixtures string `yaml:"fixtures"`
	Expect   Expect `yaml:"expect"`
	// Required cases must pass on every new version: a failed run that a refinement fixed, or the
	// replay of a skill merged into this one.
	Required bool `yaml:"required,omitempty"`
}

type Expect struct {
	Tools       []string `yaml:"tools,omitempty"`        // called in this order (other calls may come between)
	ArgsContain []string `yaml:"args_contain,omitempty"` // each appears in some call's arguments
	Answered    bool     `yaml:"answered,omitempty"`     // the run ends with an answer
}

// Fixture is one recorded tool result, served in order during replay.
type Fixture struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result string          `json:"result"`
}

const (
	maxInputs      = 6
	maxValueLen    = 120
	maxSteps       = 8
	isParameterMin = 0.6 // skill-author decisions.is_parameter.threshold
)

// Draft runs the passes on a successful trace.
func (au *Author) Draft(ctx context.Context, tr *Trace) (*Draft, error) {
	calls := tr.Calls()
	if len(calls) == 0 {
		return nil, errors.New("the run made no successful tool calls")
	}
	d := &Draft{SourceRun: tr.RunID, Pattern: tr.Pattern()}
	if tr.TaskType != "" && tr.TaskType != "chat" {
		d.TaskTypes = []string{tr.TaskType}
	}
	// (b) parameterise
	if err := au.parameterise(ctx, d, tr, calls); err != nil {
		return nil, err
	}
	actions := au.grouped(calls, d.Inputs)
	// (c) steps
	if err := au.steps(ctx, d, tr, actions); err != nil {
		return nil, err
	}
	// (d) a script tool that does the whole chore in one call
	if au.Script != nil {
		if err := au.script(ctx, d, tr, calls); err != nil {
			return nil, err
		}
	} else {
		d.Notes = append(d.Notes, "no script tool: no script sandbox on this system")
	}
	var vals []string
	for _, in := range d.Inputs {
		if in.FromRequest {
			vals = append(vals, in.Default)
		}
	}
	d.Example = ExampleRequest(tr.Request, vals)
	// (f) describe; permissions and dependencies come from the trace.
	if err := au.describe(ctx, d, tr); err != nil {
		return nil, err
	}
	d.DependsOn, d.Filesystem = au.scopes(tr, calls)
	// (e) tests
	au.tests(ctx, d, tr, calls)
	return d, nil
}

// ---- (b) parameterise ----

type value struct {
	key, val string
}

func (au *Author) parameterise(ctx context.Context, d *Draft, tr *Trace, calls []Action) error {
	var vals []value
	seen := map[string]bool{}
	for _, c := range calls {
		var v any
		if json.Unmarshal(c.Args, &v) != nil {
			continue
		}
		walkLeaves("", v, func(key, s string) {
			if s = strings.TrimSpace(s); len(s) < 2 || len(s) > maxValueLen || seen[s] || key == "dry_run" {
				return
			}
			seen[s] = true
			vals = append(vals, value{key, s})
		})
	}
	req := strings.ToLower(tr.Request)
	var unclear []value
	for _, v := range vals {
		if inRequest(req, v.val) {
			au.addInput(d, v, true)
		} else {
			unclear = append(unclear, v)
		}
	}
	if len(unclear) == 0 || len(d.Inputs) >= maxInputs {
		return nil
	}
	unclear = unclear[:min(len(unclear), 10)]
	answers := map[string]float64{}
	if au.Decide != nil {
		qs := map[string]decide.Question{}
		for i, v := range unclear {
			qs["v"+strconv.Itoa(i)] = decide.Question{Type: decide.Noul,
				Instructions: fmt.Sprintf("The value %q (argument %s): should it change between uses of this task (an input), rather than stay fixed?", v.val, v.key)}
		}
		ans, err := au.Decide.Decide(ctx, map[string]any{"request": clip(tr.Request, 600), "actions": tr.Compress(80)}, qs)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.Notes = append(d.Notes, "is_parameter failed; unclear values became inputs: "+err.Error())
		} else {
			for k, a := range ans.Answers {
				answers[k] = a.Noul
			}
		}
	}
	for i, v := range unclear {
		p, ok := answers["v"+strconv.Itoa(i)]
		if !ok || p >= isParameterMin { // unsure → input, with the trace value as default
			au.addInput(d, v, false)
		}
		if len(d.Inputs) >= maxInputs {
			break
		}
	}
	return nil
}

func walkLeaves(key string, v any, f func(key, s string)) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			walkLeaves(k, x[k], f)
		}
	case []any:
		for _, e := range x {
			walkLeaves(key, e, f)
		}
	case string:
		f(key, x)
	case float64:
		f(key, strconv.FormatFloat(x, 'f', -1, 64))
	}
}

// inRequest: the value, or a path's last element, appears in the request.
func inRequest(lowerReq, v string) bool {
	lv := strings.ToLower(v)
	if strings.Contains(lowerReq, lv) {
		return true
	}
	if strings.ContainsAny(v, `/\`) {
		base := strings.ToLower(path.Base(strings.ReplaceAll(strings.TrimRight(v, `/\`), `\`, "/")))
		return len(base) >= 3 && strings.Contains(lowerReq, base)
	}
	return false
}

var identRe = regexp.MustCompile(`[^a-z0-9]+`)

func (au *Author) addInput(d *Draft, v value, fromReq bool) {
	if len(d.Inputs) >= maxInputs {
		return
	}
	name := strings.Trim(identRe.ReplaceAllString(strings.ToLower(v.key), "_"), "_")
	if name == "" {
		name = "value"
	}
	base := name
	for i := 2; slices.ContainsFunc(d.Inputs, func(x Input) bool { return x.Name == name }); i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	d.Inputs = append(d.Inputs, Input{Name: name, Default: au.tilde(v.val), FromRequest: fromReq})
}

func (au *Author) tilde(p string) string {
	if au.Home != "" && (p == au.Home || strings.HasPrefix(p, au.Home+"/") || strings.HasPrefix(p, au.Home+`\`)) {
		return "~" + p[len(au.Home):]
	}
	return p
}

// templated is pass (a) with inputs written as {name}.
func (au *Author) templArgs(c Action, inputs []Input) string {
	args := compactJSON(c.Args)
	for _, in := range inputs {
		args = strings.ReplaceAll(args, jsonInner(in.Default), "{"+in.Name+"}")
		if au.Home != "" && strings.HasPrefix(in.Default, "~") {
			args = strings.ReplaceAll(args, jsonInner(au.Home+in.Default[1:]), "{"+in.Name+"}")
		}
	}
	return args
}

// groups splits calls into runs of the same tool: a tool called once per file is one step.
func groups(calls []Action) [][]Action {
	var out [][]Action
	for i := 0; i < len(calls); {
		j := i + 1
		for j < len(calls) && calls[j].Tool == calls[i].Tool {
			j++
		}
		out = append(out, calls[i:j])
		i = j
	}
	return out
}

// grouped is templated with each run of one tool shown once, the parts that change per item
// written as <each item>. Shown one by one, a small model copies this run's file names into the
// steps, and the skill then reads files the next request doesn't have (block 2c).
func (au *Author) grouped(calls []Action, inputs []Input) string {
	var b strings.Builder
	for i, g := range groups(calls) {
		fmt.Fprintf(&b, "%d. %s(%s) → %s", i+1, g[0].Tool, au.groupArgs(g, inputs), clip(g[0].Result, 120))
		if len(g) > 1 {
			fmt.Fprintf(&b, " (%d times, once per item)", len(g))
		}
		b.WriteString("\n")
	}
	return b.String()
}

const eachItem = "<each item>"

// groupArgs is the first call's templated arguments with every top-level string that differs
// across the group cut down to its shared prefix and suffix around <each item>.
func (au *Author) groupArgs(g []Action, inputs []Input) string {
	first := au.templArgs(g[0], inputs)
	if len(g) == 1 {
		return first
	}
	ms := make([]map[string]any, len(g))
	for i, c := range g {
		if json.Unmarshal([]byte("{"+au.templArgs(c, inputs)+"}"), &ms[i]) != nil { // compactJSON drops the braces
			return first
		}
	}
	for k, v := range ms[0] {
		s, ok := v.(string)
		if !ok {
			continue
		}
		vals := []string{s}
		for _, m := range ms[1:] {
			o, _ := m[k].(string)
			vals = append(vals, o)
		}
		if slices.ContainsFunc(vals, func(o string) bool { return o != s }) {
			ms[0][k] = varying(vals)
		}
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(ms[0]) != nil {
		return first
	}
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(b.String()), "{"), "}")
}

func varying(vals []string) string {
	p, sfx := len(vals[0]), len(vals[0])
	for _, v := range vals[1:] {
		p = min(p, commonPrefix(vals[0], v))
		sfx = min(sfx, commonPrefix(reverse(vals[0]), reverse(v)))
	}
	short := slices.MinFunc(vals, func(a, b string) int { return len(a) - len(b) })
	sfx = min(sfx, len(short)-p)
	return vals[0][:p] + eachItem + vals[0][len(vals[0])-sfx:]
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func reverse(s string) string {
	r := []byte(s)
	slices.Reverse(r)
	return string(r)
}

// perItem is the values that change from item to item within the run: what differs across a run
// of one tool, and the elements of list arguments (move_many's items). Steps that quote them only
// fit this run's files.
func perItem(calls []Action) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		for _, v := range []string{s, path.Base(strings.ReplaceAll(s, `\`, "/"))} {
			if len(v) >= 3 && strings.ContainsFunc(v, unicode.IsLetter) && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	for _, g := range groups(calls) {
		ms := make([]map[string]any, len(g))
		for i, c := range g {
			_ = json.Unmarshal(c.Args, &ms[i])
		}
		for k, v := range ms[0] {
			if arr, ok := v.([]any); ok {
				walkLeaves(k, arr, func(_, s string) { add(s) })
			}
			s, ok := v.(string)
			if !ok {
				continue
			}
			for _, m := range ms[1:] {
				if o, _ := m[k].(string); o != s {
					add(s)
					add(o)
				}
			}
		}
		for _, m := range ms[1:] {
			for k, v := range m {
				if arr, ok := v.([]any); ok {
					walkLeaves(k, arr, func(_, s string) { add(s) })
				}
			}
		}
	}
	return out
}

// codeRe catches steps written as code rather than as a tool call: a small model then answers
// with the "plan" as text instead of calling the tool (block 2c).
var codeRe = regexp.MustCompile(`\[\s*\{|\bfor \w+ in \b|\blambda\b|=>|\bvariable\b`)

// badSteps says why written steps can't be used as they are, or "".
func badSteps(steps []string, calls []Action, inputs []Input, request string) string {
	if len(steps) == 0 || !mentionsTools(steps, calls) {
		return "no usable steps"
	}
	text := strings.Join(steps, "\n")
	if m := codeRe.FindString(text); m != "" {
		return fmt.Sprintf("steps written as code (%q)", m)
	}
	req := strings.ToLower(request)
	for _, v := range perItem(calls) {
		if inRequest(req, v) || slices.ContainsFunc(inputs, func(in Input) bool { return in.Default == v || path.Base(in.Default) == v }) {
			continue
		}
		if strings.Contains(text, v) {
			return fmt.Sprintf("steps name %q from this run", v)
		}
	}
	return ""
}

// jsonInner is s as it appears inside a JSON string.
func jsonInner(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// ---- (c) steps ----

var stepsSchema = json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","minItems":1,"maxItems":8,
"items":{"type":"string","maxLength":200}}},"required":["steps"]}`)

func (au *Author) steps(ctx context.Context, d *Draft, tr *Trace, actions string) error {
	var names []string
	for _, in := range d.Inputs {
		names = append(names, "{"+in.Name+"}")
	}
	prompt := fmt.Sprintf(`A user asked: %q
These tool calls did it:
%s
Write the steps another assistant should follow to do this kind of task again.
Rules: short, numbered imperative steps; one tool call per step; name the tool in backticks; write the changing values as %s; no other specific values. A tool used once per item is one step ("for each file that list_dir found, call ..."); never name this run's files. Plain words, no code.
Reply with JSON: {"steps": ["..."]}`, clip(tr.Request, 600), actions, orNone(strings.Join(names, ", ")))
	out, err := au.LLM(ctx, prompt, stepsSchema, 600)
	var r struct{ Steps []string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &r)
	}
	if err == nil {
		for _, s := range r.Steps {
			s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "0123456789.) "))
			if s != "" {
				d.Steps = append(d.Steps, s)
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if why := badSteps(d.Steps, tr.Calls(), d.Inputs, tr.Request); why != "" {
		if err != nil {
			why = err.Error()
		}
		d.Notes = append(d.Notes, "steps written from the trace ("+why+")")
		d.Steps = mechanicalSteps(tr.Calls(), d.Inputs, au)
	}
	if len(d.Steps) > maxSteps {
		d.Steps = d.Steps[:maxSteps]
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// mentionsTools: the steps name at least half of the distinct tools the run used.
func mentionsTools(steps []string, calls []Action) bool {
	text := strings.Join(steps, "\n")
	var tools []string
	for _, c := range calls {
		if !slices.Contains(tools, c.Tool) {
			tools = append(tools, c.Tool)
		}
	}
	hit := 0
	for _, t := range tools {
		if strings.Contains(text, t) {
			hit++
		}
	}
	return hit*2 >= len(tools)
}

func mechanicalSteps(calls []Action, inputs []Input, au *Author) []string {
	var out []string
	for _, g := range groups(calls) {
		g = slices.Clone(g)
		for i := range g {
			g[i].Args = withoutPrograms(g[i].Args)
		}
		rest := au.groupArgs(g, inputs)
		s := fmt.Sprintf("Call `%s`", g[0].Tool)
		if rest != "" && rest != "{}" {
			s += " with " + clip(rest, 140)
		}
		if len(g) > 1 {
			s += " once for each item"
		}
		out = append(out, s+".")
	}
	return append(out, "Answer with what the tools returned.")
}

// withoutPrograms replaces multi-line string arguments (run_python's code) with a description. A
// step that quoted the program would carry this run's data into the next run.
func withoutPrograms(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	changed := false
	for k, v := range m {
		if s, ok := v.(string); ok && strings.Contains(strings.TrimSpace(s), "\n") {
			desc := "(the text made for the request)"
			if k == "code" || k == "script" {
				desc = "(a short program written for the request that prints the result)"
			}
			m[k], changed = desc, true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// ---- (f) describe ----

var describeSchema = json.RawMessage(`{"type":"object","properties":{
"name":{"type":"string","pattern":"^[a-z][a-z0-9-]{2,39}$"},
"description":{"type":"string","minLength":20,"maxLength":300},
"triggers":{"type":"array","maxItems":4,"items":{"type":"string","maxLength":40}}},
"required":["name","description","triggers"]}`)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,39}$`)

func (au *Author) describe(ctx context.Context, d *Draft, tr *Trace) error {
	prompt := fmt.Sprintf(`A user asked: %q
The assistant did it in these steps:
%s
Name this as a reusable skill and say when to use it.
- name: lowercase-with-hyphens, 2 to 4 words, the kind of task (not this one request)
- description: one sentence starting with what it does, then "Use when ..."
- triggers: up to 4 short phrases a user might say
Reply with JSON: {"name": "...", "description": "...", "triggers": ["..."]}`, clip(tr.Request, 600), numbered(d.Steps))
	out, err := au.LLM(ctx, prompt, describeSchema, 300)
	var r struct {
		Name        string
		Description string
		Triggers    []string
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &r)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.Name = strings.Trim(identRe.ReplaceAllString(strings.ToLower(r.Name), "-"), "-")
	if err != nil || !nameRe.MatchString(r.Name) || len(strings.TrimSpace(r.Description)) < 20 {
		why := "invalid reply"
		if err != nil {
			why = err.Error()
		}
		d.Notes = append(d.Notes, "name and description from the trace ("+why+")")
		r.Name = fallbackName(tr)
		r.Description = fmt.Sprintf("Repeat a learned procedure (%s). Use when the user asks something like: %s", strings.ReplaceAll(d.Pattern, ">", ", "), clip(tr.Request, 160))
		r.Triggers = nil
	}
	d.Name = r.Name
	for i := 2; au.Taken != nil && au.Taken(d.Name); i++ {
		d.Name = fmt.Sprintf("%s-%d", r.Name, i)
	}
	d.Description = strings.TrimSpace(r.Description)
	for _, t := range r.Triggers {
		if t = strings.TrimSpace(t); t != "" && len(d.Triggers) < 4 {
			d.Triggers = append(d.Triggers, t)
		}
	}
	return nil
}

// ExampleRequest is a request with its values cut out: paths, file names, numbers and the given
// values become "…". What's left is how this kind of request is worded, which is what the selector
// should match a new one on.
func ExampleRequest(req string, values []string) string {
	words := strings.Fields(req)
	for i, w := range words {
		core := strings.TrimRight(w, ".,:;!?)\"'")
		tail := w[len(core):]
		bare := strings.Trim(core, "(\"'")
		lb := strings.ToLower(bare)
		isValue := slices.ContainsFunc(values, func(v string) bool {
			v = strings.ToLower(strings.TrimSpace(v))
			return v != "" && lb != "" && (lb == v || lb == "."+v || strings.TrimPrefix(lb, ".") == strings.TrimPrefix(v, ".") ||
				(strings.ContainsAny(v, "/\\") && strings.HasSuffix(v, "/"+lb)))
		})
		if isValue || strings.ContainsAny(bare, "/\\.") || strings.ContainsFunc(bare, unicode.IsDigit) {
			words[i] = "…" + tail
		}
	}
	out := strings.Join(words, " ")
	for strings.Contains(out, "… …") {
		out = strings.ReplaceAll(out, "… …", "…")
	}
	return clip(out, 300)
}

func fallbackName(tr *Trace) string {
	words := strings.Fields(identRe.ReplaceAllString(strings.ToLower(tr.Request), " "))
	var keep []string
	for _, w := range words {
		if len(w) > 3 && !slices.Contains([]string{"please", "could", "would", "that", "this", "with", "from", "into", "what", "the"}, w) {
			keep = append(keep, w)
		}
		if len(keep) == 3 {
			break
		}
	}
	n := "learned-" + strings.Join(keep, "-")
	if len(n) > 40 {
		n = strings.TrimRight(n[:40], "-")
	}
	if !nameRe.MatchString(n) {
		n = "learned-procedure"
	}
	return n
}

func numbered(steps []string) string {
	var b strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	return b.String()
}

// ---- permissions and dependencies, from the trace ----

// fileWriteTools change the files they name; any other tool with a path argument only reads.
var fileWriteTools = []string{"write_text", "move", "move_many", "copy_verify", "make_dir", "trash"}

func (au *Author) scopes(tr *Trace, calls []Action) (deps, fsys []string) {
	for _, c := range calls {
		sk := c.Skill
		if sk == "" && au.SkillOf != nil {
			sk = au.SkillOf(c.Tool)
		}
		if sk != "" && !slices.Contains(deps, sk) {
			deps = append(deps, sk)
		}
	}
	slices.Sort(deps)
	var writes, reads []string
	add := func(list *[]string, p string) {
		if p = au.tilde(strings.TrimRight(p, `/\`)); p != "" && !slices.Contains(*list, p) {
			*list = append(*list, p)
		}
	}
	for _, e := range tr.Effects {
		for _, p := range []string{e.From, e.To} {
			if p != "" {
				add(&writes, dirOf(p))
			}
		}
	}
	for _, c := range calls {
		if slices.Contains(fileWriteTools, c.Tool) {
			continue // what they changed is in the file effects
		}
		var v any
		if json.Unmarshal(c.Args, &v) != nil {
			continue
		}
		walkLeaves("", v, func(key, s string) {
			if looksLikePath(s) {
				if key == "path" && c.Tool == "list_dir" {
					add(&reads, s)
				} else {
					add(&reads, dirOf(s))
				}
			}
		})
	}
	for _, w := range writes {
		fsys = append(fsys, "write:"+w)
	}
	for _, r := range reads {
		if !slices.ContainsFunc(writes, func(w string) bool { return under(r, w) }) {
			fsys = append(fsys, "read:"+r)
		}
	}
	return deps, fsys
}

func looksLikePath(s string) bool {
	if strings.Contains(s, "://") || strings.ContainsAny(s, "\n") {
		return false
	}
	return strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "/") || (len(s) > 2 && s[1] == ':' && (s[2] == '\\' || s[2] == '/'))
}

func dirOf(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if i := strings.LastIndex(strings.TrimRight(p, "/"), "/"); i > 0 {
		return p[:i]
	}
	return p
}

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimRight(dir, "/")+"/")
}

// ---- (e) tests ----

func (au *Author) tests(ctx context.Context, d *Draft, tr *Trace, calls []Action) {
	d.Cases, d.Fixtures = nil, nil
	for _, c := range calls {
		d.Fixtures = append(d.Fixtures, Fixture{Tool: c.Tool, Args: c.Args, Result: c.Result})
	}
	var tools []string
	for _, c := range calls { // the sequence of distinct steps: repeats of one tool count once
		if len(tools) == 0 || tools[len(tools)-1] != c.Tool {
			tools = append(tools, c.Tool)
		}
	}
	var reqVals []string
	for _, in := range d.Inputs {
		if in.FromRequest {
			reqVals = append(reqVals, in.Default)
		}
	}
	recorded := calls
	if d.Tool != nil {
		// The test is that the model calls the tool, once, with the request's values. The run's own
		// calls stay in the fixtures, so a model that lists the folder first still gets an answer.
		d.Fixtures = append([]Fixture{{Tool: d.Tool.Name, Args: d.Tool.Values, Result: d.Tool.Result}}, d.Fixtures...)
		tools = []string{d.Tool.Name}
		recorded = []Action{{Tool: d.Tool.Name, Args: d.Tool.Values}}
	}
	d.Cases = append(d.Cases, Case{Name: "replay of run " + tr.RunID, Input: tr.Request, Network: "recorded", Fixtures: "fixtures/replay.json",
		Expect: Expect{Tools: tools, ArgsContain: argsInCalls(reqVals, recorded, au), Answered: true}})

	var reqInputs []Input
	for _, in := range d.Inputs {
		if in.FromRequest {
			reqInputs = append(reqInputs, in)
		}
	}
	item := map[string]any{"type": "object", "required": []string{"request"}, "properties": map[string]any{
		"request": map[string]any{"type": "string", "maxLength": 400}}}
	prompt := fmt.Sprintf(`A user asked: %q
Write 1 or 2 other requests for the same kind of task, worded differently.
Reply with JSON: {"variants": [{"request": "..."}]}`, clip(tr.Request, 600))
	if len(reqInputs) > 0 {
		// Only with inputs: an object with no properties lets a small model pad it with whitespace
		// until it runs out of tokens (seen with qwen3.5:4b on Ollama).
		props := map[string]any{}
		var required, cur []string
		for _, in := range reqInputs {
			props[in.Name] = map[string]any{"type": "string", "maxLength": maxValueLen}
			required = append(required, in.Name)
			cur = append(cur, fmt.Sprintf("%s = %q", in.Name, in.Default))
		}
		item["required"] = []string{"request", "values"}
		item["properties"].(map[string]any)["values"] = map[string]any{"type": "object", "properties": props, "required": required}
		prompt = fmt.Sprintf(`A user asked: %q
Its changing values: %s.
Write 1 or 2 other requests for the same kind of task with different values, and give the values each uses. Each value must appear word for word in its request.
Reply with JSON: {"variants": [{"request": "...", "values": {...}}]}`, clip(tr.Request, 600), strings.Join(cur, ", "))
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "required": []string{"variants"}, "properties": map[string]any{
		"variants": map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "items": item}}})
	out, err := au.LLM(ctx, prompt, schema, 400)
	var r struct {
		Variants []struct {
			Request string
			Values  map[string]string
		}
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &r)
	}
	if err != nil {
		d.Notes = append(d.Notes, "no variation tests: "+err.Error())
		return
	}
	for i, v := range r.Variants {
		v.Request = strings.TrimSpace(v.Request)
		if v.Request == "" || strings.EqualFold(v.Request, strings.TrimSpace(tr.Request)) {
			continue
		}
		var want []string
		for _, in := range reqInputs {
			val := strings.TrimSpace(v.Values[in.Name])
			if val != "" && strings.Contains(strings.ToLower(v.Request), strings.ToLower(val)) && !strings.EqualFold(val, in.Default) {
				want = append(want, val)
			}
		}
		d.Cases = append(d.Cases, Case{Name: fmt.Sprintf("variation %d", i+1), Input: v.Request, Network: "recorded",
			Fixtures: "fixtures/replay.json", Expect: Expect{Tools: tools[:1], ArgsContain: want, Answered: true}})
	}
}

// argsInCalls keeps the values that really appear in the recorded arguments (a value from the
// request may have been rewritten, e.g. "downloads" → "~/Downloads").
func argsInCalls(vals []string, calls []Action, au *Author) []string {
	var all strings.Builder
	for _, c := range calls {
		all.WriteString(strings.ToLower(au.tilde(string(c.Args))))
		all.WriteString(strings.ToLower(string(c.Args)))
	}
	var out []string
	for _, v := range vals {
		if strings.Contains(all.String(), strings.ToLower(jsonInner(v))) {
			out = append(out, v)
		}
	}
	return out
}

// ---- files ----

// Files renders the draft as a skill folder: SKILL.md, tests/cases.yaml, tests/fixtures/replay.json.
func (d *Draft) Files() (map[string][]byte, error) {
	type perms struct {
		Filesystem []string `yaml:"filesystem,flow"`
	}
	type meta struct {
		Origin      string   `yaml:"origin"`
		Status      string   `yaml:"status"`
		Triggers    []string `yaml:"triggers,omitempty"`
		TaskTypes   []string `yaml:"task_types,omitempty,flow"`
		DependsOn   []string `yaml:"depends_on,omitempty,flow"`
		Permissions perms    `yaml:"permissions"`
		Confirm     []string `yaml:"confirm,omitempty,flow"`
		Tools       []string `yaml:"tools,omitempty"`
	}
	front := struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Noskill     meta   `yaml:"noskill"`
	}{d.Name, d.FullDescription(), meta{Origin: "learned", Status: "draft", Triggers: d.Triggers, TaskTypes: d.TaskTypes,
		DependsOn: d.DependsOn, Permissions: perms{Filesystem: d.Filesystem}}}
	if d.Tool != nil {
		front.Noskill.Tools = []string{"tools/" + d.Tool.Name + ".tool.yaml"}
		if d.Tool.Confirm {
			front.Noskill.Confirm = []string{d.Tool.Name}
		}
	}
	if front.Noskill.Permissions.Filesystem == nil {
		front.Noskill.Permissions.Filesystem = []string{}
	}
	fm, err := yaml.Marshal(front)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n# " + title(d.Name) + "\n\n")
	if len(d.Inputs) > 0 {
		b.WriteString("## Inputs\n")
		for _, in := range d.Inputs {
			from := "default"
			if in.FromRequest {
				from = "from the request; last time"
			}
			fmt.Fprintf(&b, "- `{%s}`: %s %q\n", in.Name, from, in.Default)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Steps\n")
	b.WriteString(numbered(d.Steps))
	fmt.Fprintf(&b, "\n<!-- Learned from run %s; tool pattern %s. -->\n", d.SourceRun, d.Pattern)
	cases, err := yaml.Marshal(d.Cases)
	if err != nil {
		return nil, err
	}
	fix, err := json.MarshalIndent(d.Fixtures, "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"SKILL.md":                   []byte(b.String()),
		"tests/cases.yaml":           cases,
		"tests/fixtures/replay.json": fix,
	}
	if d.Tool != nil {
		for n, c := range d.Tool.files() {
			files[n] = c
		}
	}
	return files, nil
}

// FullDescription is the description with the example request after it.
func (d *Draft) FullDescription() string {
	if d.Example == "" || strings.Contains(d.Description, d.Example) {
		return d.Description
	}
	s := d.Description + " Example request: \"" + d.Example + "\""
	if len(s) > 1000 {
		return d.Description
	}
	return s
}

func title(name string) string {
	w := strings.Split(name, "-")
	if len(w) > 0 && w[0] != "" {
		w[0] = strings.ToUpper(w[0][:1]) + w[0][1:]
	}
	return strings.Join(w, " ")
}

// ReadCases loads tests/cases.yaml and the fixtures it names from a skill folder.
func ReadCases(read func(name string) ([]byte, error)) ([]Case, map[string][]Fixture, error) {
	raw, err := read("tests/cases.yaml")
	if err != nil {
		return nil, nil, err
	}
	var cases []Case
	if err := yaml.Unmarshal(raw, &cases); err != nil {
		return nil, nil, fmt.Errorf("tests/cases.yaml: %w", err)
	}
	fx := map[string][]Fixture{}
	for _, c := range cases {
		if c.Fixtures == "" || fx[c.Fixtures] != nil {
			continue
		}
		b, err := read(path.Join("tests", c.Fixtures))
		if err != nil {
			return nil, nil, err
		}
		var list []Fixture
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, nil, fmt.Errorf("tests/%s: %w", c.Fixtures, err)
		}
		fx[c.Fixtures] = list
	}
	return cases, fx, nil
}

// Repair rewrites the steps after failed tests, from what the replays did (LEARNING.md §4: up to
// 2 self-repair attempts with the test output).
func (au *Author) Repair(ctx context.Context, d *Draft, tr *Trace, results []CaseResult) error {
	if d.Tool != nil {
		if !d.toolRepaired { // first say more plainly that the tool comes first
			d.toolRepaired = true
			d.Steps = toolSteps(d.Tool, true)
			return nil
		}
		// The model doesn't use the tool: keep the skill, without it.
		why := ""
		if i := slices.IndexFunc(results, func(r CaseResult) bool { return !r.Passed }); i >= 0 {
			why = fmt.Sprintf(" (%s; calls: %s)", results[i].Why, clip(strings.ReplaceAll(orNone(results[i].Calls), "\n", " "), 300))
		}
		d.Notes = append(d.Notes, fmt.Sprintf("script tool %s dropped: the tests' runs didn't use it%s", d.Tool.Name, why))
		d.Tool, d.Steps, d.Inputs = nil, d.plainSteps, d.plainInputs
		au.tests(ctx, d, tr, tr.Calls())
		return ctx.Err()
	}
	var fails strings.Builder
	for _, r := range results {
		if !r.Passed {
			fmt.Fprintf(&fails, "Test %q: %s\nCalls made:\n%s\n", r.Case, r.Why, orNone(r.Calls))
		}
	}
	prompt := fmt.Sprintf(`These steps were written for requests like %q:
%s
An assistant following them failed:
%s
These tool calls are what worked originally:
%s
Rewrite the steps so an assistant following them makes those calls. Keep them short, one tool call per step, tool names in backticks, changing values as {name}.
Reply with JSON: {"steps": ["..."]}`, clip(tr.Request, 400), numbered(d.Steps), clip(fails.String(), 1500), au.grouped(tr.Calls(), d.Inputs))
	out, err := au.LLM(ctx, prompt, stepsSchema, 600)
	var r struct{ Steps []string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &r)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var steps []string
	for _, s := range r.Steps {
		if s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "0123456789.) ")); s != "" {
			steps = append(steps, s)
		}
	}
	if err != nil || badSteps(steps, tr.Calls(), d.Inputs, tr.Request) != "" {
		// The model couldn't fix its own steps: fall back to the plain list of calls, once.
		mech := mechanicalSteps(tr.Calls(), d.Inputs, au)
		if slices.Equal(d.Steps, mech) {
			return nil
		}
		d.Notes = append(d.Notes, "repaired steps written from the trace")
		d.Steps = mech
		return nil
	}
	d.Steps = steps[:min(len(steps), maxSteps)]
	return nil
}

// Rewrite changes the description and triggers after a failed retrieval check.
func (au *Author) Rewrite(ctx context.Context, d *Draft, tr *Trace, problem string) error {
	prompt := fmt.Sprintf(`A skill's description decides which requests use it. This one has a problem: %s.
The request it was made from: %q
Current description: %q
Its steps:
%s
Write a better description (one sentence on what it does, then "Use when ...") and up to 4 short trigger phrases.
Reply with JSON: {"name": %q, "description": "...", "triggers": ["..."]}`, problem, clip(tr.Request, 400), d.Description, numbered(d.Steps), d.Name)
	out, err := au.LLM(ctx, prompt, describeSchema, 300)
	var r struct {
		Description string
		Triggers    []string
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &r)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(strings.TrimSpace(r.Description)) < 20 {
		return nil // keep the old one; the check fails again and the draft is rejected with the reason
	}
	d.Description = strings.TrimSpace(r.Description)
	d.Triggers = nil
	for _, t := range r.Triggers {
		if t = strings.TrimSpace(t); t != "" && len(d.Triggers) < 4 {
			d.Triggers = append(d.Triggers, t)
		}
	}
	return nil
}
