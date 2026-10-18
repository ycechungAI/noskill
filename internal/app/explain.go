package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"noskill/internal/orchestrator"
	"noskill/internal/skills"
)

// Preview is what a request would do, without doing it (`route explain`, FR-O6).
type Preview struct {
	Model       string
	Swap        bool
	Pinned      bool
	Class       orchestrator.Classification
	Explanation orchestrator.Explanation
	Skills      []string // selected, with dependencies
	Matches     []SkillMatch
	Tools       []string
}

type SkillMatch struct {
	Name   string
	Score  float64
	Method string
}

// Explain plans a request and reports the plan. It may call the decision model (about 250 ms) but
// loads nothing and writes nothing.
func (a *App) Explain(ctx context.Context, req RunRequest) (*Preview, error) {
	p, err := a.Plan(ctx, req)
	if p == nil {
		return nil, err
	}
	pv := &Preview{Model: p.pick.Name, Swap: p.decision.Swap, Pinned: p.pinned, Class: p.class, Explanation: p.decision.Explanation}
	for _, s := range p.selected {
		pv.Skills = append(pv.Skills, s.Name)
	}
	for _, m := range p.matches {
		pv.Matches = append(pv.Matches, SkillMatch{m.Skill.Name, m.Score, m.Method})
	}
	for _, t := range skills.Tools(p.selected) {
		pv.Tools = append(pv.Tools, t.Name)
	}
	return pv, err
}

// PurgeTraces moves trace files for days before `before` to the trash (never deletes them) and
// clears the run rows' trace path. Run history in the database stays.
func (a *App) PurgeTraces(ctx context.Context, before time.Time) (int, error) {
	root := a.Dirs.Traces()
	n := 0
	years, _ := os.ReadDir(root)
	for _, y := range years {
		months, _ := os.ReadDir(filepath.Join(root, y.Name()))
		for _, m := range months {
			days, _ := os.ReadDir(filepath.Join(root, y.Name(), m.Name()))
			for _, d := range days {
				day, err := time.ParseInLocation("2006/01/02", y.Name()+"/"+m.Name()+"/"+d.Name(), time.UTC)
				if err != nil || !day.Before(before.UTC().Truncate(24*time.Hour)) {
					continue
				}
				dir := filepath.Join(root, y.Name(), m.Name(), d.Name())
				files, _ := os.ReadDir(dir)
				for _, f := range files {
					p := filepath.Join(dir, f.Name())
					if !strings.HasSuffix(f.Name(), ".jsonl") {
						continue
					}
					if _, err := a.Trash.Trash(p); err != nil {
						return n, fmt.Errorf("couldn't move %s to the trash: %w", p, err)
					}
					a.DB.ExecContext(ctx, `UPDATE runs SET trace_path = NULL WHERE trace_path = ?`, p)
					n++
				}
			}
		}
	}
	return n, nil
}
