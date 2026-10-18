package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"noskill/internal/anime"
)

// The status page's Downloads tab (docs/PROXY.md): what the tracker is asking about, what finished
// and waits to be filed, what was filed lately, and adding a download by hand. The questions
// themselves are approvals, so the Approvals tab, the tray and the CLI answer them too.

// FinishedTorrent is a noskill download that has finished and isn't filed yet.
type FinishedTorrent struct {
	Hash    string `json:"hash"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Seeding bool   `json:"seeding"` // not stopped yet
}

// FiledEpisode is one stored file, for the History tab.
type FiledEpisode struct {
	Title      string    `json:"title"`
	Episode    string    `json:"episode"`
	Path       string    `json:"path"`
	Resolution string    `json:"resolution,omitempty"`
	At         time.Time `json:"at"`
}

// DownloadsView is the Downloads and History tabs' data.
type DownloadsView struct {
	AskFirst bool              `json:"ask_first"`
	Client   string            `json:"client,omitempty"`
	Problem  string            `json:"problem,omitempty"` // why there's no torrent app
	Items    []anime.PlanItem  `json:"items"`             // questions and hand-added downloads, newest first
	Finished []FinishedTorrent `json:"finished"`
	Filed    []FiledEpisode    `json:"filed"` // newest first
	Folders  []string          `json:"folders"`
}

// AnimeDownloads reads the downloads data. It talks to the torrent app, so it is slower than a plain
// status call; the page asks for it only while the tab is open.
func (a *App) AnimeDownloads(ctx context.Context) (*DownloadsView, error) {
	t, err := a.AnimeTracker(ctx, true)
	if err != nil {
		return nil, err
	}
	v := &DownloadsView{AskFirst: t.Settings.AskFirst, Items: []anime.PlanItem{}, Finished: []FinishedTorrent{}, Filed: []FiledEpisode{}, Folders: []string{}}
	if items, err := t.State.Plans(ctx); err == nil && items != nil {
		v.Items = items
		if len(v.Items) > 100 {
			v.Items = v.Items[:100]
		}
	}
	if t.Client == nil {
		v.Problem = a.noTorrentApp(t)
	} else {
		v.Client = t.Client.Name()
		if done, err := t.Client.Completed(ctx); err != nil {
			v.Problem = err.Error()
		} else {
			for _, d := range done {
				v.Finished = append(v.Finished, FinishedTorrent{Hash: d.Hash, Name: d.Name, Size: d.Size, Seeding: !d.Stopped()})
			}
		}
	}
	seen := map[string]bool{}
	addFolder := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			v.Folders = append(v.Folders, p)
		}
	}
	addFolder(t.Settings.DefaultDest)
	titles, _ := t.State.Titles(ctx)
	for _, ts := range titles {
		if ts.Status == anime.Approved {
			addFolder(ts.Dest)
		}
		for k, f := range ts.Filed {
			v.Filed = append(v.Filed, FiledEpisode{Title: ts.Title, Episode: k, Path: f.Path, Resolution: f.Resolution, At: f.At})
		}
	}
	for _, p := range v.Items {
		if p.Kind == "manual" {
			addFolder(p.Dest)
			if p.Status == anime.PlanFiled {
				v.Filed = append(v.Filed, FiledEpisode{Title: p.Title, Path: p.Path, At: p.Updated})
			}
		}
	}
	sort.Slice(v.Filed, func(i, j int) bool { return v.Filed[i].At.After(v.Filed[j].At) })
	if len(v.Filed) > 100 {
		v.Filed = v.Filed[:100]
	}
	return v, nil
}

// noTorrentApp says why the tracker has no torrent app: none found, or the client setting names another.
func (a *App) noTorrentApp(t *anime.Tracker) string {
	if want := strings.ToLower(a.animeString("client", "auto")); want != "auto" {
		return `the anime-tracker setting client = "` + want + `" doesn't name a torrent app noskill found`
	}
	return anime.Describe(t.Detected)
}

// AnimeAddDownload adds a download the user chose (a magnet link, a .torrent link or a Nyaa page),
// to be filed in dest under name.
func (a *App) AnimeAddDownload(ctx context.Context, link, dest, name string) (anime.PlanItem, error) {
	t, err := a.AnimeTracker(ctx, false)
	if err != nil {
		return anime.PlanItem{}, &UserError{err.Error()}
	}
	if t.Client == nil {
		return anime.PlanItem{}, &UserError{"no usable torrent app: " + a.noTorrentApp(t)}
	}
	p, err := t.AddManual(ctx, link, dest, name)
	if err != nil {
		return anime.PlanItem{}, &UserError{err.Error()}
	}
	return p, nil
}

// AnimeFileNow stops the finished downloads seeding and files them now, as the next filing run would.
func (a *App) AnimeFileNow(ctx context.Context) (anime.Report, error) {
	t, err := a.AnimeTracker(ctx, false)
	if err != nil {
		return anime.Report{}, &UserError{err.Error()}
	}
	if t.Client == nil {
		return anime.Report{}, &UserError{"no usable torrent app: " + a.noTorrentApp(t)}
	}
	rep, err := t.Run(ctx, "file")
	if err != nil {
		return rep, err
	}
	if strings.Contains(strings.Join(rep.Skipped, " "), anime.FilingBusy) {
		return rep, errors.New("another filing run is in progress; try again in a minute")
	}
	return rep, nil
}
