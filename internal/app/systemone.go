package app

import (
	"context"
	"strings"

	"noskill/internal/backends"
	"noskill/internal/decide"
)

// DecideTarget is where a /v1/systemone request goes (FR-B6b, PROXY.md): Forward to a JEV model's
// Ollama backend, or answer with Decider (a chat model emulating it, FR-B6a).
type DecideTarget struct {
	Forward *NamedBackend
	Decider decide.Decider
}

// DecideFor picks the target for a decision request naming model. A JEV model on Ollama gets the
// request as it is; a chat model emulates it; no name uses the engine's own decider (the
// configured decision model, falling back to a chat model).
func (a *App) DecideFor(ctx context.Context, model string) (DecideTarget, error) {
	ms, _ := a.Models(ctx)
	if model = strings.TrimPrefix(model, "noskill/"); model == "" || model == "auto" {
		if d := a.decider(ctx, ms); d != nil {
			return DecideTarget{Decider: d}, nil
		}
		return DecideTarget{}, &NoModelError{Name: "a chat or decision model", Installed: names(ms)}
	}
	for i, m := range ms {
		if m.Name != model && m.Name != model+":latest" && m.ID != model {
			continue
		}
		switch {
		case m.Has(backends.CapDecide) && m.NB.Kind == "ollama":
			return DecideTarget{Forward: &ms[i].NB}, nil
		case m.chat():
			return DecideTarget{Decider: decide.Emulated{Backend: m.NB.B, Model: m.Name, NumCtx: a.numCtx(m)}}, nil
		}
	}
	return DecideTarget{}, &NoModelError{Name: model, Installed: names(ms)}
}

func names(ms []Model) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
