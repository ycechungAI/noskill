package app

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"noskill/internal/backends"
	"noskill/internal/orchestrator"
)

// Prompt-cache cap (ADR-0006, ADR-0012). Ollama's MLX runner keeps the prompts it has seen, so a
// model it runs grows while loaded: S3 measured up to ~8.5 GB, and in the block 2c simulation
// qwen3.5:4b went from 5.0 to 12.7 GB at the same context. GGUF models keep a fixed size. After a
// run, any of noskill's models that has grown more than decision_cache_gb past its fresh size is
// unloaded; its next use loads it fresh. Only models noskill uses (the decision and embedding
// models and the run's driver) are touched, never others the user has loaded.

// freshGuess is the fresh size assumed for a model noskill hasn't seen loaded yet: 1.5 × its file.
// Every model measured in S4 at 8k context loads under that (qwen3:4b: 2.5 GB file, 3.9 GB loaded).
func freshGuess(file int64) int64 { return file * 3 / 2 }

func floorKey(backend, name string, numCtx int) string {
	return "model_fresh_size:" + backend + "/" + name + "@" + strconv.Itoa(numCtx)
}

// capCaches runs in the background after a run.
func (a *App) capCaches(driver string) {
	limit := int64(a.Cfg.Orchestrator.DecisionCacheGB * orchestrator.GB)
	if limit <= 0 {
		return
	}
	ours := map[string]bool{a.Cfg.Orchestrator.DecisionModel: true, a.Cfg.Orchestrator.EmbedModel: true, driver: true}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		ms, _ := a.Models(ctx)
		for _, nb := range a.Backends {
			loaded, err := nb.B.Loaded(ctx)
			if err != nil {
				continue
			}
			for _, l := range loaded {
				if !ours[l.Name] || l.SizeBytes <= 0 {
					continue
				}
				var file int64
				for _, m := range ms {
					if m.NB.ID == nb.ID && m.Name == l.Name {
						file = m.SizeBytes
					}
				}
				if a.grown(ctx, nb.ID, l, file, limit) {
					slog.Info("model grew past its cache cap; unloading it", "model", l.Name, "size_gb", float64(l.SizeBytes)/orchestrator.GB)
					nb.B.Load(ctx, l.Name, 0) // keep_alive 0: Ollama unloads it once no request is using it
					a.invalidateModels()
				}
			}
		}
	}()
}

// grown says whether a loaded model is more than limit over its fresh size: the smallest size seen
// after a run at this context, once one under freshGuess of its file has been seen; until then the
// guess. Sizes are only taken after a run: right after the load call Ollama hasn't allocated its
// working buffers yet (qwen3.5:4b showed 3.95 GB, then 4.7 GB after one request), and a floor taken
// there unloaded the model after nearly every run in the block 2c simulation.
func (a *App) grown(ctx context.Context, backend string, l backends.LoadedModel, file, limit int64) bool {
	key := floorKey(backend, l.Name, l.Context)
	v, _ := a.DB.GetState(ctx, key)
	fresh, err := strconv.ParseInt(v, 10, 64)
	if err != nil || fresh <= 0 {
		if file <= 0 {
			a.DB.SetState(ctx, key, strconv.FormatInt(l.SizeBytes, 10), time.Now())
			return false
		}
		fresh = freshGuess(file)
		if l.SizeBytes <= fresh { // looks fresh: remember it (a grown one, like tev1:0.8b at 3.1 GB
			// from a 1.5 GB file, isn't remembered and is measured against the guess)
			a.DB.SetState(ctx, key, strconv.FormatInt(l.SizeBytes, 10), time.Now())
		}
	} else if l.SizeBytes < fresh {
		a.DB.SetState(ctx, key, strconv.FormatInt(l.SizeBytes, 10), time.Now())
		fresh = l.SizeBytes
	}
	return l.SizeBytes > fresh+limit
}
