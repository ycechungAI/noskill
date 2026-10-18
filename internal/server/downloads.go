package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The Downloads and History tabs of the status page (docs/PROXY.md). Admin only, like the rest of
// what changes things: the unauthenticated local client, or a token with admin = true.

func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	v, err := s.App.AnimeDownloads(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// addDownload: {url, dest?, name?}. The torrent app fetches the link, so only a magnet link, an
// https .torrent link or a Nyaa page is accepted (anime.Tracker.AddManual).
func (s *Server) addDownload(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		URL  string `json:"url"`
		Dest string `json:"dest"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.URL) == "" {
		s.fail(w, r, bad("send {\"url\": a magnet link, a .torrent link or a Nyaa page, \"dest\": a folder, \"name\": optional}"))
		return
	}
	p, err := s.App.AnimeAddDownload(r.Context(), in.URL, in.Dest, in.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": p})
}

// fileNow stops finished downloads seeding and files them.
func (s *Server) fileNow(w http.ResponseWriter, r *http.Request) {
	rep, err := s.App.AnimeFileNow(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep, "text": rep.String()})
}
