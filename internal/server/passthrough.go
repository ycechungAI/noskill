package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"noskill/internal/app"
	"noskill/internal/decide"
)

// backendFor picks the backend a passed-through request goes to: /api/* is Ollama's own API, so it
// needs an Ollama backend; /v1/* goes to the first backend that speaks it.
func (s *Server) backendFor(path string) (app.NamedBackend, bool) {
	for _, nb := range s.App.Backends {
		if strings.HasPrefix(path, "/api/") && nb.Kind != "ollama" {
			continue
		}
		return nb, true
	}
	return app.NamedBackend{}, false
}

// forward sends the request to the backend unchanged (body already read). Responses stream through
// as they arrive. The client's noskill token, Origin and Referer are removed: they mean nothing to
// the backend, and an Origin Ollama doesn't know would be refused.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	nb, ok := s.backendFor(r.URL.Path)
	if !ok {
		apiError(w, r, http.StatusNotImplemented, "this endpoint needs an Ollama backend, and none is configured")
		return
	}
	s.forwardTo(w, r, body, nb)
}

// forwardTo is forward to a chosen backend.
func (s *Server) forwardTo(w http.ResponseWriter, r *http.Request, body []byte, nb app.NamedBackend) {
	target, err := url.Parse(nb.URL)
	if err != nil {
		apiError(w, r, http.StatusInternalServerError, "bad backend URL")
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			in := pr.In.URL.Path
			if strings.HasSuffix(strings.TrimRight(target.Path, "/"), "/v1") && strings.HasPrefix(in, "/v1/") {
				pr.Out.URL.Path = strings.TrimRight(target.Path, "/") + strings.TrimPrefix(in, "/v1")
				pr.Out.URL.RawPath = ""
			}
			pr.Out.Host = target.Host
			for _, h := range []string{"Authorization", "Origin", "Referer", "Cookie"} {
				pr.Out.Header.Del(h)
			}
			if nb.Key != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+nb.Key)
			}
			if body != nil {
				pr.Out.Body = io.NopCloser(bytes.NewReader(body))
				pr.Out.ContentLength = int64(len(body))
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client left
			}
			w.Header().Set("Retry-After", "10")
			apiError(w, r, http.StatusServiceUnavailable, "the model backend isn't reachable: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

func (s *Server) passthroughV1(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	s.forward(w, r, body)
}

// systemone serves /v1/systemone (FR-B6b): a JEV model's request goes to its Ollama backend as it
// is; one naming a chat model, or none, is answered by emulation with decide (FR-B6a), in the same
// wire form plus "emulated": true.
func (s *Server) systemone(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req decide.WireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, r, http.StatusBadRequest, "couldn't parse the decision request: "+err.Error())
		return
	}
	t, err := s.App.DecideFor(r.Context(), req.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if t.Forward != nil {
		s.forwardTo(w, r, body, *t.Forward)
		return
	}
	if err := decide.Validate(req.Questions); err != nil {
		apiError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if req.State == nil {
		req.State = map[string]any{}
	}
	ans, err := t.Decider.Decide(r.Context(), req.State, req.Questions)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Model != "" {
		ans.Model = req.Model
	}
	writeJSON(w, http.StatusOK, ans.Wire(req.Questions))
}

func (s *Server) passthroughAPI(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		var ok bool
		if body, ok = readBody(w, r); !ok {
			return
		}
	}
	s.forward(w, r, body)
}

// modelManagement passes pull/delete/create/copy/push through when allowed, then has the engine
// re-read model profiles (PROXY.md).
func (s *Server) modelManagement(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowModelManagement {
		apiError(w, r, http.StatusForbidden, "model management through noskill is turned off (server.allow_model_management)")
		return
	}
	s.passthroughAPI(w, r)
	s.App.InvalidateModels()
}
