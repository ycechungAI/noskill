package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"noskill/internal/app"
)

// modelNames lists what a client can pick: noskill/auto, the exposed skills, then the installed models.
func (s *Server) openAIModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().Unix()
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := []entry{{"noskill/auto", "model", now, "noskill"}}
	for _, p := range presetNames {
		data = append(data, entry{"noskill/" + p, "model", now, "noskill"})
	}
	for _, sk := range s.exposed(ctx) {
		if ClientFrom(ctx).MaySkill(sk) {
			data = append(data, entry{"noskill/" + sk, "model", now, "noskill"})
		}
	}
	ms, errs := s.App.Models(ctx)
	for _, m := range ms {
		data = append(data, entry{m.Name, "model", now, m.NB.ID})
	}
	if len(ms) == 0 && len(errs) > 0 {
		s.fail(w, r, &app.NoBackendError{Errs: errs})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) ollamaTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type details struct {
		ParentModel string   `json:"parent_model"`
		Format      string   `json:"format"`
		Family      string   `json:"family"`
		Families    []string `json:"families"`
		ParamSize   string   `json:"parameter_size"`
		Quant       string   `json:"quantization_level"`
	}
	type entry struct {
		Name       string  `json:"name"`
		Model      string  `json:"model"`
		ModifiedAt string  `json:"modified_at"`
		Size       int64   `json:"size"`
		Digest     string  `json:"digest"`
		Details    details `json:"details"`
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	synth := func(n string) entry {
		return entry{Name: n, Model: n, ModifiedAt: now, Digest: syntheticDigest(n), Details: details{Format: "noskill", Family: "noskill", Families: []string{"noskill"}}}
	}
	models := []entry{synth("noskill/auto")}
	for _, p := range presetNames {
		models = append(models, synth("noskill/"+p))
	}
	for _, sk := range s.exposed(ctx) {
		if ClientFrom(ctx).MaySkill(sk) {
			models = append(models, synth("noskill/"+sk))
		}
	}
	ms, errs := s.App.Models(ctx)
	if len(ms) == 0 && len(errs) > 0 {
		s.fail(w, r, &app.NoBackendError{Errs: errs})
		return
	}
	for _, m := range ms {
		params := ""
		if m.ParamsB > 0 {
			params = fmt.Sprintf("%.1fB", m.ParamsB)
		}
		models = append(models, entry{Name: m.Name, Model: m.Name, ModifiedAt: now, Size: m.SizeBytes, Digest: syntheticDigest(m.ID),
			Details: details{Format: "gguf", Family: m.Family, Families: []string{m.Family}, ParamSize: params, Quant: m.Quant}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// ollamaShow answers for noskill/* names itself, so `ollama run` and model pickers work, and
// passes real models through.
func (s *Server) ollamaShow(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		s.fail(w, r, bad("the request body isn't valid JSON: %v", err))
		return
	}
	name := in.Model
	if name == "" {
		name = in.Name
	}
	if len(name) >= 8 && name[:8] == "noskill/" {
		tgt, err := s.resolve(r.Context(), name)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		_ = tgt
		writeJSON(w, http.StatusOK, map[string]any{
			"modelfile": "# noskill: " + name + "\n", "parameters": "", "template": "{{ .Prompt }}",
			"details":      map[string]any{"parent_model": "", "format": "noskill", "family": "noskill", "families": []string{"noskill"}, "parameter_size": "", "quantization_level": ""},
			"model_info":   map[string]any{"general.architecture": "noskill"},
			"capabilities": []string{"completion", "tools"},
		})
		return
	}
	s.forward(w, r, body)
}
