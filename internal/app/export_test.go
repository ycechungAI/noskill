package app

import (
	"context"

	"noskill/internal/decide"
	"noskill/internal/store"
)

func (a *App) SetJudge(d decide.Decider) { a.judgeOverride = d }
func (a *App) WaitBackground()           { a.bg.Wait() }

func storeRun(id string) store.Run {
	return store.Run{ID: id, Source: "cli", Status: "done"}
}

func undoEntry(run, op, from, to, hash string) store.UndoEntry {
	return store.UndoEntry{RunID: run, Op: op, From: from, To: to, Hash: hash}
}

func (a *App) SetPower(on bool) { a.powerOverride = func() (bool, bool) { return on, true } }

func (a *App) Reload(ctx context.Context) error { return a.loadSkills(ctx) }

func (a *App) CurateDueForTest(ctx context.Context) bool { return a.curateDue(ctx) }
