// Package mlx is the adapter for `mlx_lm.server` on Apple Silicon (FR-B5, MODEL_ADAPTERS.md
// "Backends"). Chat and tools are the openai client's. mlx_lm.server loads a model on its first
// request by Hugging Face name, lists the MLX models in the local Hugging Face cache, and has no
// JSON-schema output, so constrained steps and emulated decisions fall back to prompting (ADR-0005),
// and its integer logprobs field isn't sent.
package mlx

import (
	"context"
	"net/http"
	"strings"
	"time"

	"noskill/internal/backends"
	"noskill/internal/backends/openai"
)

func init() {
	backends.Register("mlx", func(url, apiKey string) (backends.Backend, error) { return New(url, apiKey), nil })
}

// DefaultURL is mlx_lm.server's default address.
const DefaultURL = "http://127.0.0.1:8080/v1"

type Backend struct{ *openai.Backend }

func New(url, apiKey string) *Backend {
	return &Backend{openai.NewDialect(openai.Dialect{Kind: "mlx", DefaultURL: DefaultURL, NoSchema: true, NoLogprobs: true, PlainList: true}, url, apiKey)}
}

func (b *Backend) ListModels(ctx context.Context) ([]backends.ModelInfo, error) {
	var l struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.GetJSON(ctx, b.Root()+"/models", &l); err != nil {
		return nil, err
	}
	out := make([]backends.ModelInfo, 0, len(l.Data))
	for _, m := range l.Data {
		low := strings.ToLower(m.ID)
		if strings.Contains(low, "embed") {
			continue // mlx_lm.server has no /v1/embeddings
		}
		if strings.Contains(low, "-vl") || strings.Contains(low, "vision") {
			continue // vision models need mlx_vlm, not mlx_lm.server
		}
		caps := append(b.ChatCaps(), backends.CapTools) // the probe decides (ADR-0005)
		out = append(out, backends.ModelInfo{ID: "mlx/" + m.ID, Backend: "mlx", Name: m.ID, Family: family(m.ID), Quant: quant(m.ID), Capabilities: caps})
	}
	return out, nil
}

// Embed isn't served by mlx_lm.server.
func (b *Backend) Embed(context.Context, backends.EmbedRequest) ([][]float32, error) {
	return nil, &backends.Error{Backend: "mlx", Status: http.StatusNotImplemented, Msg: "mlx_lm.server has no embeddings; set orchestrator.embed_model to a model on another backend"}
}

// Load does nothing and Loaded reports nothing: mlx_lm.server loads the model a request names,
// keeps one at a time, and doesn't say which.
func (b *Backend) Load(context.Context, string, time.Duration) error { return nil }

func (b *Backend) Loaded(context.Context) ([]backends.LoadedModel, error) { return nil, nil }

// family reads "qwen3" from "mlx-community/Qwen3-4B-4bit".
func family(id string) string {
	n := strings.ToLower(id[strings.LastIndex(id, "/")+1:])
	if i := strings.IndexAny(n, "-_"); i > 0 {
		return n[:i]
	}
	return n
}

// quant reads "4bit" from "…-4bit" or "…-8bit".
func quant(id string) string {
	low := strings.ToLower(id)
	for _, q := range []string{"3bit", "4bit", "5bit", "6bit", "8bit", "bf16", "fp16"} {
		if strings.HasSuffix(low, q) || strings.Contains(low, "-"+q+"-") {
			return q
		}
	}
	return ""
}
