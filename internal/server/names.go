package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"noskill/internal/app"
)

// presetNames are the model names that route under a preset: noskill/fast for the fast model and
// noskill/best for the quality one (the rules in config.toml say which models those are).
var presetNames = []string{"fast", "best"}

// target is what a model name asks for (PROXY.md "Model names").
type target struct {
	Passthrough bool   // plain model name: straight to the backend
	Model       string // pinned model; "" = route
	Skill       string // forced skill; "" = select
	Preset      string // noskill/fast or noskill/best: route under that preset; "" = the configured one
	Name        string // the name the client used, echoed in responses
}

// resolve maps a client's model name to a target. noskill/auto routes; noskill/<skill> forces a
// skill; noskill/fast and noskill/best route under that preset; noskill/<model> (a name with ':')
// pins the model; anything else is a plain model name.
func (s *Server) resolve(ctx context.Context, name string) (target, error) {
	t := target{Name: name}
	rest, ok := strings.CutPrefix(name, "noskill/")
	if !ok {
		if s.cfg.PassthroughPlainNames {
			t.Passthrough = true
		} else {
			t.Model = name
		}
		return t, nil
	}
	switch {
	case rest == "auto" || rest == "":
		return t, nil
	case strings.Contains(rest, ":"): // skill names can't contain ':'
		t.Model = rest
		return t, nil
	}
	if slices.Contains(s.exposed(ctx), rest) {
		t.Skill = rest
		return t, nil
	}
	if slices.Contains(presetNames, rest) {
		t.Preset = rest
		return t, nil
	}
	ms, _ := s.App.Models(ctx)
	for _, m := range ms {
		if m.Name == rest || m.Name == rest+":latest" {
			t.Model = m.Name
			return t, nil
		}
	}
	return t, &app.NoModelError{Name: name, Installed: s.listNames(ctx)}
}

// exposed returns the skills that appear as noskill/<skill> models: the configured list, limited
// to skills that exist and are enabled.
func (s *Server) exposed(ctx context.Context) []string {
	usable, err := s.App.UsableSkills(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, sk := range usable {
		if slices.Contains(s.cfg.ExposeSkills, sk.Name) {
			out = append(out, sk.Name)
		}
	}
	return out
}

// listNames is every name a client can use, for 404 messages.
func (s *Server) listNames(ctx context.Context) []string {
	names := []string{"noskill/auto"}
	for _, p := range presetNames {
		names = append(names, "noskill/"+p)
	}
	for _, sk := range s.exposed(ctx) {
		names = append(names, "noskill/"+sk)
	}
	ms, _ := s.App.Models(ctx)
	for _, m := range ms {
		names = append(names, m.Name)
	}
	sort.Strings(names[1:])
	return names
}

// conversationFP links the turns of one conversation without the client's help: a chat app resends
// the whole history every time, so the system prompt plus the first user message are stable.
func conversationFP(msgs []chatMessage) string {
	h := sha256.New()
	first := false
	for _, m := range msgs {
		switch m.Role {
		case "system":
			fmt.Fprintf(h, "s:%s\x00", m.Content)
		case "user":
			if !first {
				fmt.Fprintf(h, "u:%s\x00", m.Content)
				first = true
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// syntheticDigest gives noskill/* entries a 64-character digest: `ollama list` crashes on a short
// one (S2).
func syntheticDigest(name string) string {
	h := sha256.Sum256([]byte("noskill:" + name))
	return hex.EncodeToString(h[:])
}
