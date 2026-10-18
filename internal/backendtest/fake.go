// Package backendtest provides a scripted Backend for tests of the agent loop, router and decide.
package backendtest

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"noskill/internal/backends"
)

// Fake answers Chat calls from a script, in order, and records every request.
type Fake struct {
	mu       sync.Mutex
	Script   []backends.Response
	Requests []backends.ChatRequest
	Models   []backends.ModelInfo
	Resident []backends.LoadedModel
	Unloaded []string // models Load was asked to unload (keep_alive 0)
	// Reply, when set, answers instead of Script (for content-dependent fakes).
	Reply func(backends.ChatRequest) (backends.Response, error)
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) ListModels(context.Context) ([]backends.ModelInfo, error) { return f.Models, nil }

func (f *Fake) Chat(ctx context.Context, req backends.ChatRequest) (backends.ChatStream, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	var r backends.Response
	var err error
	if f.Reply != nil {
		f.mu.Unlock()
		r, err = f.Reply(req)
		f.mu.Lock()
	} else if len(f.Script) == 0 {
		err = errors.New("backendtest: script exhausted")
	} else {
		r, f.Script = f.Script[0], f.Script[1:]
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if r.Usage.Total() == 0 {
		r.Usage = backends.Usage{PromptTokens: 100, CompletionTokens: len(r.Content)/4 + 10}
	}
	return &stream{ctx: ctx, chunks: []backends.ChatChunk{
		{Thinking: r.Thinking},
		{Content: r.Content, ToolCalls: r.ToolCalls, Logprobs: r.Logprobs},
		{Done: true, Usage: r.Usage},
	}}, nil
}

func (f *Fake) Embed(context.Context, backends.EmbedRequest) ([][]float32, error) {
	return nil, errors.New("backendtest: no embeddings")
}

func (f *Fake) Load(_ context.Context, model string, ka time.Duration) error {
	if ka == 0 {
		f.mu.Lock()
		f.Unloaded = append(f.Unloaded, model)
		f.mu.Unlock()
	}
	return nil
}

func (f *Fake) Loaded(context.Context) ([]backends.LoadedModel, error) { return f.Resident, nil }

// Calls returns how many Chat requests were made.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

type stream struct {
	ctx    context.Context
	chunks []backends.ChatChunk
}

func (s *stream) Recv() (backends.ChatChunk, error) {
	if err := s.ctx.Err(); err != nil {
		return backends.ChatChunk{}, err
	}
	if len(s.chunks) == 0 {
		return backends.ChatChunk{}, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *stream) Close() error { return nil }
