package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transmission talks to Transmission's RPC (the Mac app's Settings › Remote, transmission-daemon,
// transmission-gtk/qt's "Allow remote access"). Transmission has no RSS rules, so the tracker reads
// the feeds itself and adds matching episodes here. Labels need Transmission 3.00 or later.
type Transmission struct {
	Base     string // http://127.0.0.1:9091
	User     string
	Pass     string
	Label    string // only torrents with this label are listed for filing or removed
	SavePath string // download folder for noskill's torrents ("" = Transmission's default)
	HTTP     *http.Client

	mu      sync.Mutex
	session string
}

func (t *Transmission) Name() string { return "Transmission" }

// TransmissionError is a failed call, with what to do about it.
type TransmissionError struct {
	Status int
	Result string
}

func (e *TransmissionError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Transmission wants a login: store TRANSMISSION_USER and TRANSMISSION_PASS with `noskill secrets set`, or turn off \"Require authentication\" in Settings › Remote"
	case http.StatusForbidden:
		return "Transmission refused the connection: add 127.0.0.1 to the allowed addresses in Settings › Remote"
	}
	if e.Result != "" {
		return "Transmission: " + e.Result
	}
	return fmt.Sprintf("Transmission: HTTP %d", e.Status)
}

func (t *Transmission) call(ctx context.Context, method string, args, out any) error {
	body, _ := json.Marshal(map[string]any{"method": method, "arguments": args})
	c := t.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.Base, "/")+"/transmission/rpc", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		t.mu.Lock()
		if t.session != "" {
			req.Header.Set("X-Transmission-Session-Id", t.session)
		}
		t.mu.Unlock()
		if t.User != "" {
			req.SetBasicAuth(t.User, t.Pass)
		}
		resp, err := c.Do(req)
		if err != nil {
			return fmt.Errorf("can't reach Transmission at %s (is it running with remote access on?): %w", t.Base, err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict { // CSRF: retry with the session id it hands out
			t.mu.Lock()
			t.session = resp.Header.Get("X-Transmission-Session-Id")
			t.mu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return &TransmissionError{Status: resp.StatusCode}
		}
		var r struct {
			Result    string          `json:"result"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("Transmission %s: unexpected reply: %w", method, err)
		}
		if r.Result != "success" {
			return &TransmissionError{Status: 200, Result: r.Result}
		}
		if out != nil {
			return json.Unmarshal(r.Arguments, out)
		}
		return nil
	}
	return &TransmissionError{Status: http.StatusConflict, Result: "no session id"}
}

// Ensure checks the RPC answers and supports labels.
func (t *Transmission) Ensure(ctx context.Context) error {
	var s struct {
		RPCVersion int    `json:"rpc-version"`
		Version    string `json:"version"`
	}
	if err := t.call(ctx, "session-get", map[string]any{"fields": []string{"rpc-version", "version"}}, &s); err != nil {
		return err
	}
	if s.RPCVersion < 16 {
		return fmt.Errorf("Transmission %s is too old: labels need 3.00 or later", s.Version)
	}
	return nil
}

func (t *Transmission) AddTorrent(ctx context.Context, torrentURL string) error {
	args := map[string]any{"filename": torrentURL, "labels": []string{t.Label}}
	if t.SavePath != "" {
		args["download-dir"] = t.SavePath
	}
	var out struct {
		Added *struct {
			Hash string `json:"hashString"`
		} `json:"torrent-added"`
		Dup *struct {
			Hash string `json:"hashString"`
		} `json:"torrent-duplicate"`
	}
	if err := t.call(ctx, "torrent-add", args, &out); err != nil {
		return err
	}
	if out.Dup != nil {
		return errors.New("Transmission already has this torrent")
	}
	if out.Added != nil { // older versions ignore labels on add
		// Stop seeding the moment the download completes: ratio limit 0 for this torrent (mode 1 = its
		// own limit). Without it a finished torrent seeds until the user's global limit, or for ever,
		// and only stops when a later filing run gets to it.
		return t.call(ctx, "torrent-set", map[string]any{"ids": []string{out.Added.Hash}, "labels": []string{t.Label},
			"seedRatioMode": 1, "seedRatioLimit": 0.0}, nil)
	}
	return nil
}

var trFields = []string{"hashString", "name", "status", "labels", "downloadDir", "percentDone", "totalSize",
	"doneDate", "addedDate", "uploadRatio", "seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode", "files"}

type trTorrent struct {
	Hash           string   `json:"hashString"`
	Name           string   `json:"name"`
	Status         int      `json:"status"` // 0 stopped, 4 downloading, 6 seeding
	Labels         []string `json:"labels"`
	DownloadDir    string   `json:"downloadDir"`
	PercentDone    float64  `json:"percentDone"`
	TotalSize      int64    `json:"totalSize"`
	DoneDate       int64    `json:"doneDate"`
	AddedDate      int64    `json:"addedDate"`
	UploadRatio    float64  `json:"uploadRatio"`
	SeedRatioLimit float64  `json:"seedRatioLimit"`
	SeedRatioMode  int      `json:"seedRatioMode"` // 0 global, 1 this torrent, 2 unlimited
	SeedIdleLimit  int64    `json:"seedIdleLimit"`
	SeedIdleMode   int      `json:"seedIdleMode"`
	Files          []struct {
		Name           string `json:"name"`
		Length         int64  `json:"length"`
		BytesCompleted int64  `json:"bytesCompleted"`
	} `json:"files"`
}

func (t *Transmission) torrent(r trTorrent) Torrent {
	out := Torrent{Hash: strings.ToLower(r.Hash), Name: r.Name, SavePath: r.DownloadDir, Progress: r.PercentDone,
		Size: r.TotalSize, CompletedOn: r.DoneDate, AddedOn: r.AddedDate, Ratio: r.UploadRatio, MaxSeedTime: -1}
	if slices.Contains(r.Labels, t.Label) {
		out.Category = t.Label
	}
	out.ContentPath = filepath.Join(r.DownloadDir, r.Name)
	switch {
	case r.Status == 0 && r.PercentDone >= 1:
		out.State = "stoppedUP"
	case r.Status == 6:
		out.State = "uploading"
		if r.SeedRatioMode == 1 && r.SeedRatioLimit > 0 {
			out.MaxRatio = r.SeedRatioLimit
		}
		if r.SeedIdleMode == 1 && r.SeedIdleLimit > 0 {
			out.MaxSeedTime = r.SeedIdleLimit
		}
	case r.Status == 4:
		out.State = "downloading"
	}
	return out
}

func (t *Transmission) get(ctx context.Context, ids []string) ([]trTorrent, error) {
	args := map[string]any{"fields": trFields}
	if ids != nil {
		args["ids"] = ids
	}
	var out struct {
		Torrents []trTorrent `json:"torrents"`
	}
	err := t.call(ctx, "torrent-get", args, &out)
	return out.Torrents, err
}

func (t *Transmission) All(ctx context.Context) ([]Torrent, error) {
	rs, err := t.get(ctx, nil)
	var out []Torrent
	for _, r := range rs {
		out = append(out, t.torrent(r))
	}
	return out, err
}

func (t *Transmission) Completed(ctx context.Context) ([]Torrent, error) {
	all, err := t.All(ctx)
	if err != nil {
		return nil, err
	}
	var keep []Torrent
	for _, x := range all {
		if t.Label != "" && x.Category == t.Label && x.Progress >= 1 {
			keep = append(keep, x)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].CompletedOn < keep[j].CompletedOn })
	return keep, nil
}

func (t *Transmission) Torrent(ctx context.Context, hash string) (Torrent, error) {
	rs, err := t.get(ctx, []string{strings.ToLower(hash)})
	if err != nil {
		return Torrent{}, err
	}
	if len(rs) == 0 {
		return Torrent{}, fmt.Errorf("Transmission has no torrent %s", hash)
	}
	return t.torrent(rs[0]), nil
}

func (t *Transmission) Files(ctx context.Context, hash string) ([]TorrentFile, error) {
	rs, err := t.get(ctx, []string{strings.ToLower(hash)})
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("Transmission has no torrent %s", hash)
	}
	var out []TorrentFile
	for _, f := range rs[0].Files {
		p := 0.0
		if f.Length > 0 {
			p = float64(f.BytesCompleted) / float64(f.Length)
		}
		// Transmission's file names include the torrent's folder, relative to downloadDir: the same as qBittorrent's.
		out = append(out, TorrentFile{Name: f.Name, Size: f.Length, Progress: p})
	}
	return out, nil
}

// RemoveTorrent removes one of noskill's torrents, keeping its files.
// Stop stops one of noskill's torrents so it stops seeding.
func (t *Transmission) Stop(ctx context.Context, hash string) error {
	x, err := t.Torrent(ctx, hash)
	if err != nil {
		return err
	}
	if t.Label == "" || x.Category != t.Label {
		return fmt.Errorf("torrent %q has no %q label: %w", x.Name, t.Label, ErrNotOurs)
	}
	return t.call(ctx, "torrent-stop", map[string]any{"ids": []string{x.Hash}}, nil)
}

func (t *Transmission) RemoveTorrent(ctx context.Context, hash string) error {
	x, err := t.Torrent(ctx, hash)
	if err != nil {
		return err
	}
	if t.Label == "" || x.Category != t.Label {
		return fmt.Errorf("torrent %q has no %q label: %w", x.Name, t.Label, ErrNotOurs)
	}
	return t.call(ctx, "torrent-remove", map[string]any{"ids": []string{x.Hash}, "delete-local-data": false}, nil)
}
