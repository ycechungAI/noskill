package learning

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// Near-duplicates (LEARNING.md §7, FR-L5). Two learned skills are one family when they use mostly
// the same tools and their source requests say the same thing once the values (paths, file names,
// extensions, prefixes, numbers) are taken out. Tools alone aren't enough: "write the ERROR lines
// to errors.txt" and "combine every .txt into all.txt" both list, read and write.

const (
	FamilyTools = 0.5  // tool overlap (Jaccard) …
	FamilyText  = 0.45 // … and request-word overlap for one family
)

// SameFamily says whether two skills (tool sets and source requests) do the same kind of task.
func SameFamily(toolsA, toolsB []string, reqA, reqB string) bool {
	return Overlap(toolsA, toolsB) >= FamilyTools && TextOverlap(reqA, reqB) >= FamilyText
}

// TextOverlap compares two requests' task words: the mean of the Jaccard similarity of the words
// and of the word pairs in order. The pairs keep "write the lines that contain X" apart from
// "write all of their lines", which share most single words. A request of fewer than 4 task words
// says too little to tell, so its score is scaled down.
func TextOverlap(a, b string) float64 {
	wa, wb := TaskWords(a), TaskWords(b)
	s := (Overlap(uniq(wa), uniq(wb)) + Overlap(pairs(wa), pairs(wb))) / 2
	if n := min(len(uniq(wa)), len(uniq(wb))); n < 4 {
		s *= float64(n) / 4
	}
	return s
}

func uniq(ws []string) []string {
	var out []string
	for _, w := range ws {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func pairs(ws []string) []string {
	var out []string
	for i := 1; i < len(ws); i++ {
		if p := ws[i-1] + " " + ws[i]; !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

var familyStop = []string{"the", "and", "any", "all", "every", "each", "into", "onto", "from", "with", "that", "this",
	"its", "their", "them", "they", "then", "there", "where", "which", "what", "for", "you", "your", "please", "can",
	"could", "would", "just", "only", "also", "new", "are", "was", "were", "has", "have", "how", "many", "much",
	"end", "tell", "but", "not", "one", "out", "inside"}

// valueAfter: the word after these is a value ("a folder named docs").
var valueAfter = []string{"named", "called"}

// TaskWords are the words of a request that say what to do, in order: lower-case, stemmed a
// little, with the values left out (anything with a path separator, a dot, a digit or a trailing
// dash, words in quotes, and the word after "named" or "called").
func TaskWords(req string) []string {
	var out []string
	skip := false
	for _, f := range strings.Fields(req) {
		core := strings.TrimRight(f, ".,:;!?)\"'")
		if skip {
			skip = false
			continue
		}
		if slices.Contains(valueAfter, strings.ToLower(core)) {
			skip = true
			continue
		}
		if strings.ContainsAny(core, `/\.`) || strings.ContainsFunc(core, unicode.IsDigit) || strings.HasSuffix(core, "-") ||
			strings.HasPrefix(core, "\"") || strings.HasPrefix(core, "'") || strings.HasPrefix(core, "{") {
			continue
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(core), func(r rune) bool { return !unicode.IsLetter(r) }) {
			if len(w) < 3 || slices.Contains(familyStop, w) {
				continue
			}
			out = append(out, wordStem(w))
		}
	}
	return out
}

func wordStem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return strings.TrimSuffix(w, "ing")
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return strings.TrimSuffix(w, "s")
	}
	return w
}

// SourceRequest is the request a learned skill was drafted from: its first test case's input.
func SourceRequest(dir string) string {
	cases, _, err := ReadCases(func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(n))) })
	if err != nil || len(cases) == 0 {
		return ""
	}
	return cases[0].Input
}

// DraftTools are the tools a draft's replay uses, as ToolsOf reads them from a published skill.
func (d *Draft) DraftTools() []string {
	var out []string
	for _, f := range d.Fixtures {
		if !slices.Contains(out, f.Tool) {
			out = append(out, f.Tool)
		}
	}
	return out
}

// RequestValues are the values in a request, lower-case: what TaskWords leaves out as values
// (names with a dot, digits or a trailing dash, quoted words, the word after "named"), except
// folder paths. Pass (d) code must take them from arguments, not write them in.
func RequestValues(req string) []string {
	var out []string
	skip := false
	for _, f := range strings.Fields(req) {
		core := strings.Trim(strings.TrimRight(f, ".,:;!?)"), "\"'`")
		quoted := strings.ContainsAny(f[:1], "\"'`")
		if skip || quoted || strings.ContainsAny(core, ".") || strings.ContainsFunc(core, unicode.IsDigit) || strings.HasSuffix(core, "-") {
			if v := strings.ToLower(strings.Trim(core, "*")); v != "" && !strings.ContainsAny(v, "/\\{") && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		skip = slices.Contains(valueAfter, strings.ToLower(core))
	}
	return out
}
