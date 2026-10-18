package app

import (
	"context"
	"path/filepath"
	"sync"

	"noskill/internal/docs"
)

// The local-docs index (skills/local-docs): docs.db sits beside state.db and can be rebuilt at any
// time. It opens when first used.

var docsMu sync.Mutex

// Docs returns the document index, with the embedding model attached when one is installed.
func (a *App) Docs(ctx context.Context) (*docs.Index, error) {
	docsMu.Lock()
	defer docsMu.Unlock()
	if a.docs == nil {
		ix, err := docs.Open(ctx, filepath.Join(a.Dirs.Data, "docs.db"))
		if err != nil {
			return nil, err
		}
		a.docs = ix
	}
	a.docs.Embedder, a.docs.EmbedModel = nil, ""
	if models, _ := a.Models(ctx); len(models) > 0 {
		if em := a.embedModel(models); em != nil {
			a.docs.Embedder, a.docs.EmbedModel = em.NB.B, em.Name
		}
	}
	return a.docs, nil
}

func (s *runServices) DocsIndex(ctx context.Context) (*docs.Index, error) { return s.a.Docs(ctx) }

// ExpandPath resolves ~, $HOME-style tokens and volume:<label>/… to a real path.
func (a *App) ExpandPath(p string) (string, error) { return a.resolver().Expand(p) }
