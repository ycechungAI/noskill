// Package skills embeds the built-in skills. The other skill folders in this directory are specs
// for later phases (SKILL_FORMAT.md); only the ones listed in the embed line ship in the binary.
package skills

import (
	"embed"
	"io/fs"
)

//go:embed web-search research files orchestrator model-manager code-runner anime-tracker watcher local-docs
var embedded embed.FS

// Builtin is the built-in skills, one folder per skill.
func Builtin() fs.FS { return embedded }

// Names lists the embedded skills.
var Names = []string{"web-search", "research", "files", "orchestrator", "model-manager", "code-runner", "anime-tracker", "watcher", "local-docs"}
