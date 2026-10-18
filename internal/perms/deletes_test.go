package perms

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every place that removes or renames a file outside a test, with how many calls it has. Each was
// reviewed in docs/REVIEW_BLOCK3.md: they touch noskill's own files only (scratch folders, its skill
// folders, its binary, its config with a .prev copy) or are the broker and trash code itself. A new
// call, or a changed count, fails this test until someone has looked at what it can reach and
// updated the list here and the table in the review.
var reviewedDeletes = map[string]int{
	"internal/app/eval.go":                1,
	"internal/app/script.go":              1,
	"internal/app/simulate.go":            1,
	"internal/app/undo.go":                2,
	"internal/cli/cmd_config.go":          2,
	"internal/cli/cmd_doctor.go":          1,
	"internal/cli/cmd_eval.go":            1,
	"internal/cli/cmd_update.go":          1,
	"internal/learning/library.go":        7,
	"internal/learning/script.go":         1,
	"internal/learning/tuned.go":          1,
	"internal/app/train.go":               1,
	"internal/app/registry.go":            1,
	"internal/perms/broker.go":            2,
	"internal/platform/trash_linux.go":    4,
	"internal/sandbox/workspace.go":       1,
	"internal/service/service_windows.go": 1,
	"internal/tools/script.go":            2,
	"internal/update/update.go":           8,
}

func TestEveryDeleteAndRenameIsReviewed(t *testing.T) {
	root := filepath.Join("..", "..")
	re := regexp.MustCompile(`\bos\.(Remove|RemoveAll|Rename)\(`)
	got := map[string]int{}
	for _, top := range []string{"internal", "cmd"} {
		filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			if n := len(re.FindAll(b, -1)); n > 0 {
				rel, _ := filepath.Rel(root, p)
				got[filepath.ToSlash(rel)] = n
			}
			return nil
		})
	}
	for f, n := range got {
		if reviewedDeletes[f] != n {
			t.Errorf("%s has %d os.Remove/RemoveAll/Rename calls, the reviewed list says %d: check what they can reach (docs/REVIEW_BLOCK3.md), then update reviewedDeletes", f, n, reviewedDeletes[f])
		}
	}
	for f := range reviewedDeletes {
		if got[f] == 0 {
			t.Errorf("%s no longer has any; remove it from reviewedDeletes", f)
		}
	}
}
