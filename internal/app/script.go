package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"noskill/internal/learning"
	"noskill/internal/perms"
	"noskill/internal/platform"
	"noskill/internal/sandbox"
	"noskill/internal/skills"
)

// scriptRun runs a pass-d draft script (LEARNING.md §3 pass d, ADR-0013) in the script sandbox:
// plain python, no network, reading only the rebuilt folder it was given. It is nil where no OS
// sandbox enforces file access (ADR-0008), so pass d is skipped there rather than run unsandboxed.
func (a *App) scriptRun() learning.ScriptRun {
	if a.Runner == nil || a.Interp == nil {
		return nil
	}
	sb := a.Runner.Sandbox
	if sb == nil {
		sb = platform.NewSandbox()
	}
	if sb.Strength().Files == platform.None {
		return nil
	}
	return func(ctx context.Context, code string, stdin []byte, inputs map[string]string) ([]byte, error) {
		dir, err := os.MkdirTemp("", "noskill-draft-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir) // noskill's own scratch, not user files
		script := filepath.Join(dir, "tool.py")
		if err := os.WriteFile(script, []byte(code), 0o444); err != nil {
			return nil, err
		}
		cmd, err := a.Interp.Resolve(ctx, &skills.Skill{Name: "draft"}, skills.Tool{Name: "draft", Runtime: "python"}, script)
		if err != nil {
			return nil, err
		}
		var set perms.Set
		for _, p := range inputs {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				set.Files = append(set.Files, perms.Scope{Access: perms.Read, Root: r, Decl: "read:" + r})
			}
		}
		out, err := a.Runner.Run(ctx, sandbox.Call{
			RunID: "draft", CallID: filepath.Base(dir), Argv: cmd.Argv, ReadOnly: append(slices.Clone(cmd.ReadOnly), dir),
			Stdin: stdin, Inputs: inputs, Perms: set, Timeout: 60 * time.Second,
		})
		if out.Workspace != nil {
			defer out.Workspace.Remove()
		}
		if err != nil {
			return nil, err
		}
		if !out.Sandboxed {
			return nil, errors.New("the script sandbox isn't enforcing")
		}
		if k := out.Result.Killed; k != "" {
			return out.Stdout, fmt.Errorf("stopped (%s limit)", k)
		}
		if out.Result.Exit != 0 {
			s := strings.TrimSpace(string(out.Stderr))
			if len(s) > 600 {
				s = "…" + s[len(s)-600:]
			}
			return out.Stdout, fmt.Errorf("exit %d: %s", out.Result.Exit, s)
		}
		return out.Stdout, nil
	}
}

// confirmsTool: the skill that owns tool asks before running it (its confirm: list), so a script
// that does the same work asks too.
func (a *App) confirmsTool(tool string) bool {
	for _, s := range a.Skills.All() {
		for _, t := range s.Tools {
			if t.Name == tool {
				return slices.Contains(s.Meta.Confirm, tool)
			}
		}
	}
	return false
}

// maxUpgrades is how many times one plain learned skill may be redrafted as a script skill.
const maxUpgrades = 2

// family finds the learned skill (active first, then a candidate) that already does the task of
// a draft from tr (LEARNING.md §7, FR-L5): mostly the same tools and the same request once the
// values are taken out.
func (a *App) family(tr *learning.Trace, tools []string) *skills.Skill {
	lib := a.Learned()
	var found *skills.Skill
	for _, s := range a.Skills.All() {
		st := s.Status()
		if s.Source != skills.SourceLearned || (st != "active" && st != "candidate") {
			continue
		}
		dir := lib.SkillDir(s.Name)
		if !learning.SameFamily(learning.ToolsOf(dir), tools, learning.SourceRequest(dir), tr.Request) {
			continue
		}
		if found == nil || (st == "active" && found.Status() != "active") {
			found = s
		}
	}
	return found
}

func traceTools(tr *learning.Trace) []string {
	var out []string
	for _, c := range tr.Calls() {
		if !slices.Contains(out, c.Tool) {
			out = append(out, c.Tool)
		}
	}
	return out
}

// upgradeKey counts the script redrafts of one learned skill.
func upgradeKey(name string) string { return "learning.upgrade." + name }

func (a *App) upgrades(ctx context.Context, name string) int {
	v, _ := a.DB.GetState(ctx, upgradeKey(name))
	n := 0
	fmt.Sscan(v, &n)
	return n
}

// learnedTools are the tools in called that learned skills own (pass-d script tools), including a
// version on trial in <name>/.next: a script redraft brings a tool the current version lacks.
func (a *App) learnedTools(called []string) []string {
	var out []string
	add := func(s *skills.Skill) {
		for _, t := range s.Tools {
			if slices.Contains(called, t.Name) && !slices.Contains(out, t.Name) {
				out = append(out, t.Name)
			}
		}
	}
	for _, s := range a.Skills.All() {
		if s.Source != skills.SourceLearned {
			continue
		}
		add(s)
		if nxt := a.nextSkill(s.Name); nxt != nil {
			add(nxt)
		}
	}
	return out
}
