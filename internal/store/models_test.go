package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"noskill/internal/backends"
)

func TestModelProfilesKeepMeasurements(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	if err := db.UpsertBackend(ctx, "ollama", "ollama", "http://127.0.0.1:11434", "0.40.3", now); err != nil {
		t.Fatal(err)
	}
	ms := []backends.ModelInfo{{ID: "ollama/qwen3:4b", Backend: "ollama", Name: "qwen3:4b", ParamsB: 4, Context: 262144,
		Capabilities: []backends.Capability{backends.CapChat, backends.CapTools}}}
	if err := db.UpsertModels(ctx, "ollama", ms, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.ToolStrategy(ctx, "ollama/qwen3:4b"); ok {
		t.Error("strategy before any probe")
	}
	if err := db.SetToolStrategy(ctx, "ollama/qwen3:4b", backends.StrategyNative); err != nil {
		t.Fatal(err)
	}
	if err := db.SetToolStrategy(ctx, "ollama/ghost", backends.StrategyNative); err == nil {
		t.Error("strategy set on an unknown model")
	}
	// A refresh from the listing must not wipe the probe's verdict.
	ms[0].Context = 32768
	if err := db.UpsertModels(ctx, "ollama", ms, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, ok, err := db.ToolStrategy(ctx, "ollama/qwen3:4b")
	if err != nil || !ok || s != backends.StrategyNative {
		t.Errorf("strategy = %q %v %v", s, ok, err)
	}
	got, err := db.Models(ctx)
	if err != nil || len(got) != 1 || got[0].Context != 32768 || !got[0].Has(backends.CapTools) || got[0].ToolStrategy != backends.StrategyNative {
		t.Errorf("models = %+v %v", got, err)
	}
	// Backend version survives a refresh that doesn't know it.
	db.UpsertBackend(ctx, "ollama", "ollama", "http://127.0.0.1:11434", "", now)
	var v string
	db.QueryRowContext(ctx, `SELECT version FROM backends WHERE id = 'ollama'`).Scan(&v)
	if v != "0.40.3" {
		t.Errorf("version = %q", v)
	}
}
