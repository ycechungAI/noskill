package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Pass (d), script tools (LEARNING.md §3, ADR-0013). A run that did one deterministic chore (list a
// folder, make a folder, then move, copy or read-and-write once per file) costs a small model a
// turn per step. Pass (d) asks the author model for a Python tool that does the whole chore in one
// call, so a later run takes two turns: call the tool, answer.
//
// The tool runs in the script sandbox on a read-only copy of the folder and only lists the changes
// it wants ("ops"); the broker applies them (internal/tools/ops.go). It is accepted only when,
// run on a copy of the source run's folder rebuilt from the trace, its ops are exactly the file
// changes the run made, and its code names none of that run's files or values.

// ScriptRun runs a program in the script sandbox with no network: stdin holds the arguments and
// inputs maps names in in/ to the real folders copied there. It returns stdout. A nil ScriptRun
// (no OS sandbox) skips pass (d).
type ScriptRun func(ctx context.Context, code string, stdin []byte, inputs map[string]string) ([]byte, error)

// ScriptTool is a learned tool from pass (d).
type ScriptTool struct {
	Name        string
	Description string
	Args        []ScriptArg // Args[0] is the folder
	Code        string
	Confirm     bool            // the run used a tool that asks first (move_many), so this one does too
	Values      json.RawMessage // its arguments for the source request
	Result      string          // what it returned there: the replay fixture
}

type ScriptArg struct {
	Name, Description, Value string
}

const (
	folderArg      = "folder"
	maxScriptArgs  = 4
	maxScriptTries = 4 // the first draft and 3 repairs
	maxCode        = 6000
)

var toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,39}$`)

// script is pass (d). On success the draft's steps become "call the tool once"; otherwise the
// draft keeps its steps and a note says why.
func (au *Author) script(ctx context.Context, d *Draft, tr *Trace, calls []Action) error {
	src, why := au.sourceFolder(tr, calls)
	if why != "" {
		d.Notes = append(d.Notes, "no script tool: "+why)
		return nil
	}
	want, wantText := src.effects(tr, calls, au.Home)
	if len(want) == 0 {
		d.Notes = append(d.Notes, "no script tool: the run changed no files")
		return nil
	}
	tmp, err := os.MkdirTemp("", "noskill-learn-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // noskill's own scratch copy, never a user file
	dir := filepath.Join(tmp, filepath.Base(src.root))
	if err := src.build(dir); err != nil {
		return err
	}
	confirm := false
	for _, c := range calls {
		if au.Confirm != nil && au.Confirm(c.Tool) && !isDryRunArgs(c.Args) {
			confirm = true
		}
	}
	prompt := au.scriptPrompt(tr, calls, src.root)
	var problem string
	for try := 0; try < maxScriptTries; try++ {
		p := prompt
		if problem != "" {
			p += "\n\n" + problem
		}
		out, err := au.LLM(ctx, p, nil, 1500) // plain text: small models loop when code sits in a JSON string
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			problem = "Your last reply couldn't be used: " + err.Error()
			continue
		}
		slog.Debug("pass d reply", "try", try+1, "reply", out)
		t, why := au.parseScript(out, tr, src)
		if why == "" {
			why, err = au.tryScript(ctx, t, dir, src, want, wantText)
			if err != nil {
				return err
			}
		}
		if why == "" {
			t.Confirm = confirm
			d.plainSteps, d.plainInputs = d.Steps, d.Inputs
			d.Tool = t
			d.Steps = toolSteps(t, false)
			d.Inputs = toolInputs(t)
			d.Notes = append(d.Notes, fmt.Sprintf("script tool %s written (attempt %d)", t.Name, try+1))
			return nil
		}
		problem = fmt.Sprintf("Your last reply:\n%s\nProblem: %s\nFix the problem and reply again.", clip(out, 2500), why)
	}
	d.Notes = append(d.Notes, "no script tool: "+lastLine(problem))
	return nil
}

func lastLine(s string) string {
	if i := strings.LastIndex(s, "Problem: "); i >= 0 {
		s = s[i+len("Problem: "):]
	}
	s, _, _ = strings.Cut(s, "\n")
	return clip(s, 200)
}

func isDryRunArgs(raw json.RawMessage) bool {
	var a struct {
		DryRun *bool `json:"dry_run"`
	}
	return json.Unmarshal(raw, &a) == nil && a.DryRun != nil && *a.DryRun
}

func (au *Author) scriptPrompt(tr *Trace, calls []Action, root string) string {
	var b strings.Builder
	for i, g := range groups(calls) {
		args := folderRel(compactJSON(g[0].Args), root)
		res := folderRel(clip(g[0].Result, 300), root)
		fmt.Fprintf(&b, "%d. %s(%s) → %s", i+1, g[0].Tool, clip(args, 300), res)
		if len(g) > 1 {
			fmt.Fprintf(&b, " (and %d more like it)", len(g)-1)
		}
		b.WriteString("\n")
	}
	return fmt.Sprintf(`A user asked: %q
An assistant did it with these tool calls ({folder} is the folder it worked in):
%s
Write a Python 3 tool that does this kind of task in one call, for any similar request.
- It reads its arguments as one JSON object from stdin. "folder" is always the folder. Add up to 4 other string arguments for what changes between requests (a file extension, a name prefix, a folder name, a word to look for), and give each one's value in this request.
- The folder is a read-only copy: list and read it with os and open(), but never write, move or delete anything yourself.
- Instead print one JSON object whose "ops" list says what to change, in order:
  {"op": "make_dir", "path": P}, {"op": "move", "from": P, "to": P}, {"op": "copy", "from": P, "to": P}, {"op": "write", "path": P, "text": T}, {"op": "trash", "path": P}
  where every P is os.path.join(folder, ...).
- Add other results the answer needs, such as "count".
- Make the same changes the assistant made, to every file that fits, and nothing more.
- Standard library only. Never write this request's file names or values in the code; use the arguments.
Start from:
import json, os, sys
args = json.load(sys.stdin)
folder = args["folder"]
ops = []
for name in sorted(os.listdir(folder)):
    path = os.path.join(folder, name)
    ...
print(json.dumps({"ops": ops}))
Reply in exactly this form, nothing else:
tool_name: <snake_case name>
description: <one sentence>
arg <name>: <its value in this request> | <what it is>
(one arg line for each argument besides "folder")
`+"```python\n<the code>\n```",
		clip(folderRel(tr.Request, root), 600), b.String())
}

// folderRel writes root as {folder}.
func folderRel(s, root string) string {
	if root == "" {
		return s
	}
	s = strings.ReplaceAll(s, jsonInner(root), "{folder}")
	return strings.ReplaceAll(s, root, "{folder}")
}

func (au *Author) parseScript(out string, tr *Trace, src *sourceDir) (*ScriptTool, string) {
	r, why := splitScriptReply(out)
	if why != "" {
		return nil, why
	}
	name := strings.Trim(identRe.ReplaceAllString(strings.ToLower(r.ToolName), "_"), "_")
	if !toolNameRe.MatchString(name) {
		name = "learned_tool"
	}
	if au.SkillOf != nil && au.SkillOf(name) != "" {
		name += "_learned" // never shadow a tool another skill offers
	}
	code := strings.TrimSpace(r.Code)
	switch {
	case code == "":
		return nil, "the code is empty"
	case len(code) > maxCode:
		return nil, "the code is too long; keep it short"
	case !strings.Contains(code, `"ops"`) && !strings.Contains(code, `'ops'`):
		return nil, `the code doesn't print an "ops" list`
	}
	if m := selfWriteRe.FindString(code); m != "" {
		return nil, fmt.Sprintf("the code changes files itself (%s); the folder is read-only, so take that out and append an op for the change instead", strings.TrimSpace(m))
	}
	t := &ScriptTool{Name: name, Code: code + "\n", Description: strings.TrimSpace(r.Description)}
	if len(t.Description) < 10 {
		t.Description = "Do the learned task in one call."
	}
	t.Args = []ScriptArg{{Name: folderArg, Description: "the folder the request names", Value: src.root}}
	req := strings.ToLower(folderRel(tr.Request, src.root))
	lits := stringLiterals(stripArgDefaults(code, r.Arguments))
	var unused []ScriptArg
	for _, a := range r.Arguments {
		a.Name = strings.TrimSpace(a.Name)
		a.Value = strings.TrimSpace(a.Value)
		if a.Name == folderArg || a.Name == "dry_run" || a.Name == "" || a.Value == "" {
			continue // an argument with no value here falls back to the code's default
		}
		if len(t.Args) > maxScriptArgs {
			break
		}
		if !inRequest(req, a.Value) {
			continue // not a value of the request (a result such as a count): not an argument
		}
		if !slices.Contains(lits, a.Name) {
			unused = append(unused, a) // not an argument after all, but its value mustn't be in the code
			continue
		}
		t.Args = append(t.Args, a)
	}
	if !slices.Contains(lits, folderArg) {
		return nil, `the code never reads args["folder"]`
	}
	// Reading an argument with no value here would fail (args[...]) or quietly use its default.
	for _, m := range argReadRe.FindAllStringSubmatch(t.Code, -1) {
		n := m[1]
		if n == "dry_run" || slices.ContainsFunc(t.Args, func(a ScriptArg) bool { return a.Name == n }) {
			continue
		}
		if strings.HasPrefix(m[0], "args[") { // args.get(name, default) is fine: an optional filter
			return nil, fmt.Sprintf(`the code reads args[%q], but no "arg %s: <value> | <what it is>" line gives its value word for word from the request`, n, n)
		}
	}
	// A small model often writes a request value into the code ("all.txt") even after being told
	// not to. A literal that is exactly such a value becomes args[...] here, adding the argument.
	if c, args := liftValues(t.Code, t.Args, RequestValues(req)); c != t.Code {
		t.Code, t.Args = c, args
		lits = stringLiterals(stripArgDefaults(t.Code, t.Args))
	}
	// Hard-coded values only fit the source run's files.
	for _, l := range lits {
		if slices.Contains(protocolWords, l) || slices.ContainsFunc(t.Args, func(a ScriptArg) bool { return a.Name == l }) {
			continue
		}
		for _, a := range append(slices.Clone(t.Args[1:]), unused...) {
			v := a.Value
			if len(v) >= 2 && (l == v || l == "."+v || (len(v) >= 4 && strings.Contains(l, v))) {
				return nil, fmt.Sprintf("the code has %q written in it; use args[%q] instead", l, a.Name)
			}
		}
		for _, v := range RequestValues(req) {
			if len(v) >= 3 && strings.Contains(strings.ToLower(l), v) {
				return nil, fmt.Sprintf("the code has %q written in it, a value from the request; add an argument for it", l)
			}
		}
		for f := range src.files {
			if b := path.Base(f); len(b) >= 3 && strings.ContainsFunc(b, unicode.IsLetter) && strings.Contains(l, b) {
				return nil, fmt.Sprintf("the code names %q, a file of this run; work out names from the folder and the arguments", b)
			}
		}
	}
	vals := map[string]string{}
	for _, a := range t.Args {
		vals[a.Name] = a.Value
	}
	t.Values, _ = json.Marshal(vals)
	return t, ""
}

type scriptParts struct {
	ToolName, Description, Code string
	Arguments                   []ScriptArg
}

// selfWriteRe finds code that changes files itself instead of asking for an op.
var selfWriteRe = regexp.MustCompile(`open\([^)\n]*,\s*(?:mode\s*=\s*)?["'][wax]b?\+?["']|shutil\.\w+\(|os\.(?:remove|unlink|rename|replace|makedirs|mkdir|rmdir)\(|\.write_(?:text|bytes)\(`)

var argReadRe = regexp.MustCompile(`args(?:\.get\(|\[)\s*["']([A-Za-z_][A-Za-z0-9_]*)["']`)

var headerKeyRe = regexp.MustCompile(`(?i)\b(tool[_ ]name|description|arg(?:ument)?\s+[a-z_][a-z0-9_]*)\s*:`)

// splitScriptReply reads pass (d)'s plain-text reply: "tool_name:", "description:" and
// "arg <name>: <value> | <what it is>" lines, then the code in a ``` block.
func splitScriptReply(out string) (scriptParts, string) {
	var r scriptParts
	head, rest, ok := strings.Cut(out, "```")
	if !ok {
		return r, "the reply has no ```python code block"
	}
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 && !strings.ContainsAny(strings.TrimSpace(rest[:nl]), " (=") {
		rest = rest[nl+1:] // the fence's language tag
	}
	code, _, _ := strings.Cut(rest, "```")
	r.Code = code
	// The keys may share a line (a repair reply often runs them together), so find them anywhere.
	head = strings.NewReplacer("*", "", "`", "").Replace(head)
	keys := headerKeyRe.FindAllStringSubmatchIndex(head, -1)
	for n, k := range keys {
		end := len(head)
		if n+1 < len(keys) {
			end = keys[n+1][0]
		}
		key := strings.Join(strings.Fields(strings.ToLower(head[k[2]:k[3]])), " ")
		v, _, _ := strings.Cut(strings.TrimSpace(head[k[1]:end]), "\n")
		v = strings.TrimRight(strings.TrimSpace(v), " -")
		switch {
		case key == "tool_name" || key == "tool name":
			r.ToolName, _, _ = strings.Cut(v, " ")
		case key == "description":
			r.Description = v
		default: // "arg <name>" or "argument <name>"
			val, desc, _ := strings.Cut(v, "|")
			r.Arguments = append(r.Arguments, ScriptArg{Name: strings.Trim(strings.Fields(key)[1], `"'`),
				Value: strings.Trim(strings.TrimSpace(val), `"'`), Description: strings.TrimSpace(desc)})
		}
	}
	if r.ToolName == "" { // a bare name on the first line
		if f := strings.Fields(head); len(f) > 0 && toolNameRe.MatchString(strings.ToLower(f[0])) {
			r.ToolName = f[0]
		}
	}
	return r, ""
}

// liftValues replaces each quoted literal that is exactly an argument's value, or a request value,
// with args["name"], adding an argument for a request value that has none (up to maxScriptArgs).
func liftValues(code string, args []ScriptArg, values []string) (string, []ScriptArg) {
	args = slices.Clone(args)
	for _, v := range values {
		if len(v) < 2 || slices.Contains(protocolWords, v) {
			continue
		}
		i := slices.IndexFunc(args[1:], func(a ScriptArg) bool { return strings.EqualFold(a.Value, v) })
		var name string
		switch {
		case i >= 0:
			name, v = args[i+1].Name, args[i+1].Value
		case len(args) > maxScriptArgs:
			continue
		default:
			name = valueArgName(v, args)
		}
		re := regexp.MustCompile(`"` + regexp.QuoteMeta(v) + `"|'` + regexp.QuoteMeta(v) + `'`)
		// A declared argument's default (args.get("ext", ".png")) stays: it never applies.
		var kept [][]int
		for _, m := range argDefaultRe.FindAllStringSubmatchIndex(code, -1) {
			n := code[max(m[4], m[6]):max(m[5], m[7])]
			if slices.ContainsFunc(args[1:], func(a ScriptArg) bool { return a.Name == n }) {
				kept = append(kept, m[:2])
			}
		}
		var b strings.Builder
		last, lifted := 0, false
		for _, m := range re.FindAllStringIndex(code, -1) {
			if slices.ContainsFunc(kept, func(k []int) bool { return m[0] >= k[0] && m[1] <= k[1] }) {
				continue
			}
			b.WriteString(code[last:m[0]] + `args["` + name + `"]`)
			last, lifted = m[1], true
		}
		if !lifted {
			continue
		}
		code = b.String() + code[last:]
		if i < 0 {
			args = append(args, ScriptArg{Name: name, Description: "a value the request names", Value: v})
		}
	}
	return code, args
}

func valueArgName(v string, args []ScriptArg) string {
	base := "name"
	switch {
	case strings.HasPrefix(v, ".") && !strings.Contains(v[1:], "."):
		base = "ext"
	case strings.HasSuffix(v, "-") || strings.HasSuffix(v, "_"):
		base = "prefix"
	case strings.ContainsFunc(v, unicode.IsDigit) && !strings.Contains(v, "."):
		base = "value"
	}
	name := base
	for n := 2; slices.ContainsFunc(args, func(a ScriptArg) bool { return a.Name == name }); n++ {
		name = fmt.Sprintf("%s%d", base, n)
	}
	return name
}

// protocolWords are literals every tool has (the ops' keys and names), whatever its values.
var protocolWords = []string{"ops", "op", "path", "from", "to", "text", "make_dir", "move", "copy", "write", "trash", "folder", "count", "done", "summary", "r", "w", "utf-8"}

var argDefaultRe = regexp.MustCompile(`(args\.get\(\s*(?:"([^"\n]*)"|'([^'\n]*)'))\s*,\s*(?:"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*')\s*\)`)

// stripArgDefaults drops the default from args.get("name", default) for the arguments the reply
// declares with a value: the call always passes one, so the default never applies.
func stripArgDefaults(code string, declared []ScriptArg) string {
	return argDefaultRe.ReplaceAllStringFunc(code, func(m string) string {
		g := argDefaultRe.FindStringSubmatch(m)
		if slices.ContainsFunc(declared, func(a ScriptArg) bool {
			return strings.TrimSpace(a.Value) != "" && strings.TrimSpace(a.Name) == g[2]+g[3]
		}) {
			return g[1] + ")"
		}
		return m
	})
}

var strLitRe = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'`)

func stringLiterals(code string) []string {
	var out []string
	for _, m := range strLitRe.FindAllString(code, -1) {
		out = append(out, m[1:len(m)-1])
	}
	return out
}

// tryScript runs the tool on the rebuilt folder and compares its ops with the run's file changes.
// It returns what didn't match, or "".
func (au *Author) tryScript(ctx context.Context, t *ScriptTool, dir string, src *sourceDir, want []string, wantText map[string]string) (string, error) {
	base := filepath.Base(dir)
	vals := map[string]string{}
	for _, a := range t.Args {
		vals[a.Name] = a.Value
	}
	vals[folderArg] = "in/" + base
	stdin, _ := json.Marshal(vals)
	out, err := au.Script(ctx, t.Code, stdin, map[string]string{base: dir})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "running it failed: " + clip(err.Error(), 600), nil
	}
	var res map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &res) != nil || res["ops"] == nil {
		return fmt.Sprintf("it printed %q, not one JSON object with \"ops\"", clip(string(out), 300)), nil
	}
	var ops []struct{ Op, Path, From, To, Text string }
	if err := json.Unmarshal(res["ops"], &ops); err != nil {
		return `"ops" must be a list of objects like {"op": "move", "from": ..., "to": ...}`, nil
	}
	rel := func(p string) (string, bool) {
		s := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(p), "./"), "/")
		rest, ok := strings.CutPrefix(s, "in/"+base)
		if !ok || (rest != "" && rest[0] != '/') {
			return "", false
		}
		r := path.Clean("/" + rest)[1:]
		return r, !strings.HasPrefix(r, "..")
	}
	var got []string
	gotText := map[string]string{}
	var done []string
	for _, op := range ops {
		var key string
		switch op.Op {
		case "make_dir":
			r, ok := rel(op.Path)
			if !ok {
				return fmt.Sprintf("op path %q isn't inside the folder", op.Path), nil
			}
			done = append(done, "make folder "+path.Join(base, r))
			continue
		case "move", "copy":
			f, ok1 := rel(op.From)
			to, ok2 := rel(op.To)
			if !ok1 || !ok2 {
				return fmt.Sprintf("op %s %q → %q leaves the folder", op.Op, op.From, op.To), nil
			}
			key = op.Op + " " + f + " → " + to
			got = append(got, key)
			done = append(done, op.Op+" "+path.Join(base, f)+" → "+path.Join(base, to))
			continue
		case "write", "trash":
			r, ok := rel(op.Path)
			if !ok {
				return fmt.Sprintf("op path %q isn't inside the folder", op.Path), nil
			}
			key = op.Op + " " + r
			if op.Op == "write" {
				gotText[r] = op.Text
			}
		default:
			return fmt.Sprintf("unknown op %q", op.Op), nil
		}
		got = append(got, key)
		done = append(done, op.Op+" "+path.Join(base, strings.TrimPrefix(key, op.Op+" ")))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return fmt.Sprintf("on this request's folder it would %s, but the task needs %s", orNone(strings.Join(got, "; ")), strings.Join(want, "; ")), nil
	}
	for p, w := range wantText {
		if g := lines(gotText[p]); !slices.Equal(g, lines(w)) {
			return fmt.Sprintf("it writes %s with %q, but it should hold %q", p, clip(gotText[p], 200), clip(w, 200)), nil
		}
	}
	delete(res, "ops")
	res["done"], _ = json.Marshal(done)
	b, _ := json.Marshal(res)
	t.Result = string(b)
	return "", nil
}

// lines are a text's non-empty lines, trimmed and sorted: the order a tool joins files in isn't
// what the check is about.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

// toolSteps are the steps of a skill with a script tool: one call, then the answer.
func toolSteps(t *ScriptTool, strict bool) []string {
	var parts []string
	for _, a := range t.Args[1:] {
		parts = append(parts, fmt.Sprintf("`%s` (%s)", a.Name, strings.TrimSuffix(a.Description, ".")))
	}
	s := fmt.Sprintf("Call `%s` once, with `folder` set to the folder the request names", t.Name)
	if len(parts) > 0 {
		s += " and " + strings.Join(parts, ", ") + " taken from the request"
	}
	s += ". That one call does the whole task."
	steps := []string{s}
	if strict {
		steps = append(steps, "Call it first: don't list or read the folder before it, and don't call it with a dry run.")
	}
	return append(steps, "Answer from its result: what it did, and any number it returned. Call other tools only if it reports a failure.")
}

func toolInputs(t *ScriptTool) []Input {
	var out []Input
	for _, a := range t.Args {
		out = append(out, Input{Name: a.Name, Default: a.Value, FromRequest: true})
	}
	return out
}

// ---- the source run's folder ----

type sourceDir struct {
	root  string            // the folder the run worked in, as recorded
	files map[string]string // relative path → content ("" when the run never read it)
	dirs  []string
}

type listing struct {
	Path    string `json:"path"`
	Entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"entries"`
}

// sourceFolder rebuilds what the run's folder held before the run, from its listings, the files
// it read and its file changes. why says why it can't, or "".
func (au *Author) sourceFolder(tr *Trace, calls []Action) (*sourceDir, string) {
	expand := func(p string) string {
		if au.Home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
			return au.Home + p[1:]
		}
		return p
	}
	src := &sourceDir{files: map[string]string{}}
	for _, c := range calls {
		if c.Tool != "list_dir" {
			continue
		}
		var l listing
		if json.Unmarshal([]byte(c.Result), &l) != nil || l.Path == "" {
			continue
		}
		if src.root == "" {
			src.root = strings.TrimRight(l.Path, "/")
		}
		pre, ok := relTo(l.Path, src.root)
		if !ok {
			continue
		}
		for _, e := range l.Entries {
			rel := path.Join(pre, e.Name)
			switch e.Type {
			case "file":
				if _, ok := src.files[rel]; !ok {
					src.files[rel] = ""
				}
			case "folder":
				src.dirs = append(src.dirs, rel)
			}
		}
	}
	type read struct{ Path, Text string }
	var reads []read
	for _, c := range calls {
		if c.Tool != "read_text" {
			continue
		}
		var r read
		if json.Unmarshal([]byte(c.Result), &r) == nil && r.Path != "" {
			reads = append(reads, r)
		}
	}
	if src.root == "" {
		switch {
		case len(reads) > 0:
			src.root = dirOf(reads[0].Path)
		case len(tr.Effects) > 0 && tr.Effects[0].From != "":
			src.root = dirOf(tr.Effects[0].From)
		case len(tr.Effects) > 0:
			src.root = dirOf(tr.Effects[0].To)
		default:
			return nil, "the run didn't work in a folder"
		}
	}
	for _, r := range reads {
		rel, ok := relTo(expand(r.Path), src.root)
		if !ok {
			return nil, "the run read files outside one folder"
		}
		src.files[rel] = r.Text
	}
	made := map[string]bool{}
	for _, e := range tr.Effects {
		for _, p := range []string{e.From, e.To} {
			if p == "" || (e.Op == "trash" && p == e.To) {
				continue
			}
			if _, ok := relTo(p, src.root); !ok {
				return nil, "the run changed files outside one folder"
			}
		}
		if e.From != "" && (e.Op == "move" || e.Op == "copy" || e.Op == "trash") {
			rel, _ := relTo(e.From, src.root)
			if _, ok := src.files[rel]; !ok && !made[rel] {
				src.files[rel] = ""
			}
		}
		if e.To != "" && e.Op != "trash" {
			rel, _ := relTo(e.To, src.root)
			made[rel] = true
		}
	}
	if len(src.files) == 0 {
		return nil, "the run's folder had no files it recorded"
	}
	return src, ""
}

// relTo is p relative to root, if p is inside it.
func relTo(p, root string) (string, bool) {
	p, root = strings.ReplaceAll(p, `\`, "/"), strings.TrimRight(strings.ReplaceAll(root, `\`, "/"), "/")
	if p == root || p == root+"/" {
		return "", true
	}
	rest, ok := strings.CutPrefix(p, root+"/")
	if !ok {
		return "", false
	}
	r := path.Clean(rest)
	return r, r != "." && !strings.HasPrefix(r, "..")
}

// build writes the rebuilt folder at dir. Files the run never read get a placeholder line.
func (s *sourceDir) build(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, d := range s.dirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o700); err != nil {
			return err
		}
	}
	for rel, text := range s.files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if text == "" {
			text = "(contents not recorded)\n"
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// effects are the run's file changes as ops relative to the folder, sorted ("move a.txt →
// old/a.txt", "write all.txt"), with the text of each file it wrote. Making folders isn't
// compared: a move makes the folder it needs.
func (s *sourceDir) effects(tr *Trace, calls []Action, home string) ([]string, map[string]string) {
	written := map[string]string{}
	for _, c := range calls {
		if c.Tool != "write_text" {
			continue
		}
		var a struct{ Path, Content string }
		if json.Unmarshal(c.Args, &a) != nil {
			continue
		}
		p := a.Path
		if home != "" && strings.HasPrefix(p, "~/") {
			p = home + p[1:]
		}
		written[path.Clean(strings.ReplaceAll(p, `\`, "/"))] = a.Content
	}
	var out []string
	text := map[string]string{}
	for _, e := range tr.Effects {
		from, _ := relTo(e.From, s.root)
		to, _ := relTo(e.To, s.root)
		switch e.Op {
		case "move", "copy":
			out = append(out, e.Op+" "+from+" → "+to)
		case "trash":
			out = append(out, "trash "+from)
		case "create":
			out = append(out, "write "+to)
			t, ok := written[path.Clean(strings.ReplaceAll(e.To, `\`, "/"))]
			if !ok { // the call named the file another way (a symlinked temp folder): match by name
				for p, c := range written {
					if path.Base(p) == path.Base(e.To) {
						t = c
					}
				}
			}
			text[to] = t
		}
	}
	slices.Sort(out)
	return out, text
}

// ---- the files ----

func (t *ScriptTool) files() map[string][]byte {
	props := map[string]any{}
	var required []string
	for i, a := range t.Args {
		p := map[string]any{"type": "string", "description": a.Description}
		if i == 0 {
			p["format"] = "path"
		}
		props[a.Name] = p
		required = append(required, a.Name)
	}
	def := map[string]any{
		"name":        t.Name,
		"description": t.Description + " Does the whole task in one call.",
		"runtime":     "python",
		"entry":       t.Name + ".py",
		"timeout":     "60s",
		"input_schema": map[string]any{
			"type": "object", "required": required, "properties": props,
		},
	}
	b, _ := json.MarshalIndent(def, "", "  ") // JSON is YAML
	return map[string][]byte{
		"tools/" + t.Name + ".tool.yaml": append(b, '\n'),
		"tools/" + t.Name + ".py":        []byte(t.Code),
	}
}
