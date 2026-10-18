// Package vllm is the adapter for vLLM's OpenAI-compatible server on Linux (FR-B5,
// MODEL_ADAPTERS.md "Backends"). Chat, tools and json_schema output are the openai client's; vLLM
// adds each model's context length to /v1/models, serves only the models it was started with
// (so they are always loaded), and reports its version at /version.
package vllm

import (
	"context"
	"strings"
	"time"

	"noskill/internal/backends"
	"noskill/internal/backends/openai"
)

func init() {
	backends.Register("vllm", func(url, apiKey string) (backends.Backend, error) { return New(url, apiKey), nil })
}

// DefaultURL is `vllm serve`'s default address.
const DefaultURL = "http://127.0.0.1:8000/v1"

type Backend struct{ *openai.Backend }

func New(url, apiKey string) *Backend {
	return &Backend{openai.NewDialect(openai.Dialect{Kind: "vllm", DefaultURL: DefaultURL, PlainList: true}, url, apiKey)}
}

type modelList struct {
	Data []struct {
		ID          string `json:"id"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

func (b *Backend) ListModels(ctx context.Context) ([]backends.ModelInfo, error) {
	var l modelList
	if err := b.GetJSON(ctx, b.Root()+"/models", &l); err != nil {
		return nil, err
	}
	out := make([]backends.ModelInfo, 0, len(l.Data))
	for _, m := range l.Data {
		caps := []backends.Capability{backends.CapEmbed}
		if !strings.Contains(strings.ToLower(m.ID), "embed") {
			// Tool calls need the server started with --enable-auto-tool-choice; the probe (ADR-0005)
			// finds out per model, so offer them.
			caps = append(b.ChatCaps(), backends.CapTools)
		}
		out = append(out, backends.ModelInfo{ID: "vllm/" + m.ID, Backend: "vllm", Name: m.ID, Context: m.MaxModelLen, Capabilities: caps})
	}
	return out, nil
}

// Loaded is every model the server serves: vLLM loads them at start and keeps them. Their memory
// isn't reported (vLLM reserves a share of the GPU up front, outside noskill's budget).
func (b *Backend) Loaded(ctx context.Context) ([]backends.LoadedModel, error) {
	var l modelList
	if err := b.GetJSON(ctx, b.Root()+"/models", &l); err != nil {
		return nil, err
	}
	out := make([]backends.LoadedModel, 0, len(l.Data))
	for _, m := range l.Data {
		out = append(out, backends.LoadedModel{Name: m.ID, Context: m.MaxModelLen})
	}
	return out, nil
}

// Load does nothing: a vLLM server serves a fixed set of models, chosen when it starts.
func (b *Backend) Load(context.Context, string, time.Duration) error { return nil }

func (b *Backend) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := b.GetJSON(ctx, b.Origin()+"/version", &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
