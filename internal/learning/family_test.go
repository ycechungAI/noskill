package learning

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The workload's six task families (and the eval's held-out ones): every pair inside a family is one family, no pair across.
func TestFamiliesOfTheWorkload(t *testing.T) {
	var raw []byte
	for _, f := range []string{"../eval/workload/workload.yaml", "../eval/tasks/agent.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
	}
	re := regexp.MustCompile(`prompt: "(.*)"`)
	var reqs []string
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		reqs = append(reqs, strings.ReplaceAll(m[1], "{dir}", "/private/var/folders/x/noskill-sim-123"))
	}
	if len(reqs) < 12 {
		t.Fatalf("%d prompts", len(reqs))
	}
	family := func(r string) string {
		for _, k := range []string{"Tidy", "contains ERROR", "starts with", "Rename", "keeping the originals", "all.txt"} {
			if strings.Contains(r, k) {
				return k
			}
		}
		return r
	}
	for i, a := range reqs {
		for _, b := range reqs[i+1:] {
			same, got := family(a) == family(b), TextOverlap(a, b)
			if same != (got >= FamilyText) || (!same && got > FamilyText-0.1) {
				t.Errorf("%.2f for\n  %s\n  %s", got, a, b)
			}
		}
	}
}
