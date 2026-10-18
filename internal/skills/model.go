// Package skills loads agentskills.io-compatible skills (FR-S1, FR-S2) and selects them for a
// request (FR-S3). A plain SKILL.md with no `noskill:` block always loads: it's instructions
// only, with no tools and no permissions (AGENTS.md).
package skills

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Source says where a skill came from. It decides trust and precedence (see Set).
type Source string

const (
	SourceBuiltin Source = "builtin" // embedded in the binary
	SourceUser    Source = "user"    // <data>/skills
	SourceExtra   Source = "extra"   // [skills] paths in the user's own config
	SourceProject Source = "project" // ./.noskill/skills in a working folder: untrusted until enabled
	SourceLearned Source = "learned" // <data>/learned: written by the learning loop (LEARNING.md)
)

// Skill is one loaded skill. Progressive disclosure (FR-S2): name and description are always in
// the index; Body is read with the file and only injected when the skill is selected.
type Skill struct {
	Name        string
	Description string
	License     string
	Body        string // Markdown after the front-matter
	Source      Source
	Dir         string // on-disk folder; "" for embedded skills
	FS          fs.FS  // rooted at the skill folder
	Hash        string // content hash of every file in the folder
	Meta        Meta   // zero for a plain SKILL.md
	HasNoskill  bool   // the file has a `noskill:` block
	Tools       []Tool
	Warnings    []string
}

// Origin of the skill's content: the declared origin, else derived from where it was found.
func (s *Skill) Origin() string {
	if s.Meta.Origin != "" {
		return s.Meta.Origin
	}
	switch s.Source {
	case SourceBuiltin:
		return "builtin"
	case SourceProject, SourceExtra:
		return "imported"
	case SourceLearned:
		return "learned"
	}
	return "user"
}

// Status defaults to active: built-in and user skills start active (SKILL_FORMAT.md).
func (s *Skill) Status() string {
	if s.Meta.Status == "" {
		return "active"
	}
	return s.Meta.Status
}

// NeedsApproval: project-folder skills are untrusted until the user enables them, because a cloned
// repository could otherwise ship its own permissions.
func (s *Skill) NeedsApproval() bool { return s.Source == SourceProject }

// Meta is the `noskill:` block (SKILL_FORMAT.md).
type Meta struct {
	Version         string               `yaml:"version"`
	Origin          string               `yaml:"origin"`
	Status          string               `yaml:"status"`
	Triggers        []string             `yaml:"triggers"`
	TaskTypes       []string             `yaml:"task_types"`
	MinModel        MinModel             `yaml:"min_model"`
	PreferModels    []string             `yaml:"prefer_models"`
	Permissions     Permissions          `yaml:"permissions"`
	Confirm         []string             `yaml:"confirm"`
	ToolRefs        []ToolRef            `yaml:"tools"`
	DependsOn       []string             `yaml:"depends_on"`
	ScheduleDefault string               `yaml:"schedule_default"`
	Decisions       map[string]any       `yaml:"decisions"`
	Extra           map[string]yaml.Node `yaml:",inline"`
}

type MinModel struct {
	Context int    `yaml:"context"`
	Tools   string `yaml:"tools"` // any | native | none
}

// Permissions are what the skill declares (SECURITY.md "Permissions"). Enforcement is in
// internal/perms and internal/sandbox; this is only the declaration.
type Permissions struct {
	Network    StringList `yaml:"network"`
	Filesystem StringList `yaml:"filesystem"`
	Exec       bool       `yaml:"exec"`
	Subprocess bool       `yaml:"subprocess"`
	Secrets    StringList `yaml:"secrets"` // NAME, or NAME? when optional
}

// Secret is one entry of permissions.secrets.
type Secret struct {
	Name     string
	Optional bool
}

func (p Permissions) SecretList() []Secret {
	var out []Secret
	for _, s := range p.Secrets {
		name, opt := strings.CutSuffix(s, "?")
		out = append(out, Secret{Name: name, Optional: opt})
	}
	return out
}

// StringList accepts a YAML list or a single scalar ("network: localhost").
type StringList []string

func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" || n.Value == "" {
			*l = nil
			return nil
		}
		*l = StringList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: want a list of strings", c.Line)
			}
			out = append(out, c.Value)
		}
		*l = out
		return nil
	}
	return fmt.Errorf("line %d: want a string or a list of strings", n.Line)
}

// ToolRef is an entry of `tools:`: a path to a *.tool.yaml, an MCP server tool, or a tool built
// into noskill (`- builtin: list_dir`), whose name, description and schema come from the Go
// registry.
type ToolRef struct {
	Path    string
	MCP     *MCPRef
	Builtin string
}

type MCPRef struct {
	Server string `yaml:"server"`
	Tool   string `yaml:"tool"`
}

func (r *ToolRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Path = n.Value
		return nil
	}
	var m struct {
		MCP     *MCPRef `yaml:"mcp"`
		Builtin string  `yaml:"builtin"`
	}
	if err := n.Decode(&m); err != nil || (m.MCP == nil && m.Builtin == "") {
		return fmt.Errorf("line %d: a tool is a path, { builtin: <id> } or { mcp: { server, tool } }", n.Line)
	}
	r.MCP, r.Builtin = m.MCP, m.Builtin
	return nil
}

// Tool is a parsed *.tool.yaml (SKILL_FORMAT.md "Tool definition").
type Tool struct {
	Name         string
	Description  string
	Runtime      string // builtin | python | node | bun | sh | pwsh | wasm | mcp
	Entry        string // resolved for this OS: a builtin id, or a path inside the skill folder
	Deps         []string
	Timeout      time.Duration
	MaxOutput    int // bytes
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	OS           []string
	MCP          *MCPRef
	File         string // the .tool.yaml path inside the skill folder; "" for an inline builtin
	Skill        string
	// Inline: declared as `- builtin: <id>`. Description and InputSchema are empty here; the
	// builtin registry fills them in when the tool is offered.
	Inline bool
}

// Def is what the model sees.
func (t Tool) Def(visibleName string) (name, description string, schema json.RawMessage) {
	s := t.InputSchema
	if len(s) == 0 {
		s = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return visibleName, t.Description, s
}

// Issue is a problem found while loading. Errors skip the skill or tool; warnings don't.
type Issue struct {
	Level string `json:"level"` // error | warn
	Skill string `json:"skill,omitempty"`
	Path  string `json:"path,omitempty"`
	Msg   string `json:"msg"`
}

func (i Issue) String() string {
	where := i.Path
	if where == "" {
		where = i.Skill
	}
	if where != "" {
		return fmt.Sprintf("%s: %s: %s", i.Level, where, i.Msg)
	}
	return fmt.Sprintf("%s: %s", i.Level, i.Msg)
}

func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}
