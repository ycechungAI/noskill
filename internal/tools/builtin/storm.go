package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"noskill/internal/tools"
)

// Storm is the STORM research tool (skills/storm): the pipeline runs in the application (the
// service), this tool writes its files into a new folder under the skill's write scope.
func Storm() []tools.Builtin {
	return []tools.Builtin{
		{ID: "storm_run", Description: "Research a topic from five viewpoints (practitioner, academic, skeptic, economist, historian), one at a time, and write a cited report into a new folder. It takes a long time (many searches and page fetches). questions is how many each viewpoint asks: 2 for a quick look, 3 by default, 5 for a deep one. Returns the report's path and its summary.",
			Schema: json.RawMessage(`{"type":"object","required":["topic"],"properties":{"topic":{"type":"string","minLength":3,"maxLength":300},"questions":{"type":"integer","default":3,"minimum":1,"maximum":5}}}`),
			Run:    stormRun},
	}
}

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

func stormSlug(topic string) string {
	s := strings.Trim(slugBad.ReplaceAllString(strings.ToLower(topic), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "topic"
	}
	return s
}

func stormRun(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	if err := needSvc(env); err != nil {
		return "", err
	}
	var a struct {
		Topic     string
		Questions int
	}
	json.Unmarshal(raw, &a)
	a.Topic = strings.TrimSpace(a.Topic)
	if len(a.Topic) < 3 {
		return "", errors.New(`storm_run needs a "topic"`)
	}
	root, err := env.Expand("$DOCUMENTS/noskill-research")
	if err != nil {
		return "", err
	}
	// A new folder per run: never into an existing one.
	base := "storm-" + stormSlug(a.Topic) + "-" + time.Now().Format("2006-01-02")
	dir := filepath.Join(root, base)
	for i := 2; ; i++ {
		if _, err := os.Lstat(dir); err != nil {
			break
		}
		dir = filepath.Join(root, fmt.Sprintf("%s-%d", base, i))
	}
	// Check the folder is allowed before the long part, not after it.
	if err := env.Broker.Mkdir(ctx, dir); err != nil {
		return "", friendly(err)
	}
	res, err := env.Svc.Storm(ctx, a.Topic, a.Questions)
	if err != nil {
		return "", err
	}
	for _, f := range res.Files {
		if err := env.Broker.WriteFile(ctx, filepath.Join(dir, f.Name), []byte(f.Content)); err != nil {
			return "", fmt.Errorf("couldn't write %s: %w", f.Name, friendly(err))
		}
	}
	names := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		names = append(names, f.Name)
	}
	report := filepath.Join(dir, "report.md")
	// The summary carries the path and the gaps, so an answer that repeats it keeps them.
	summary := res.Summary + "\n\nFull report: " + report
	if res.Unsupported > 0 {
		summary += fmt.Sprintf("\n(%d of %d questions found no page that supported an answer.)", res.Unsupported, res.Unsupported+res.Answered)
	}
	return jsonOut(map[string]any{"report": report, "folder": dir, "summary": summary,
		"sections": res.Sections, "sources": res.Sources, "answered": res.Answered, "unsupported": res.Unsupported, "files": names})
}
