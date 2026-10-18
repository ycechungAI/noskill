package anime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"noskill/internal/approvals"
)

// BatchOffer is a complete-season release found for an earlier season of a show the user follows:
// offered in an approval card, and added to the torrent app only after the user approves it.
type BatchOffer struct {
	Item  NyaaItem `json:"item"`
	Group string   `json:"group,omitempty"`
	Added bool     `json:"added,omitempty"`
}

// DefaultBatchWindow is how recent a batch upload must be to count as new.
const DefaultBatchWindow = 45 * 24 * time.Hour

type prequelCache struct {
	At    time.Time `json:"at"`
	Chain []Media   `json:"chain"`
}

// prequelChain lists the earlier seasons of an AniList show, nearest first, from AniList (cached for a
// week): a "Season 2" gives its season 1, and that one's own prequels, up to four steps back.
func (t *Tracker) prequelChain(ctx context.Context, id int) ([]Media, error) {
	var c prequelCache
	key := "prequels." + strconv.Itoa(id)
	if ok, _ := t.State.get(ctx, key, &c); ok && t.now().Sub(c.At) < 7*24*time.Hour {
		return c.Chain, nil
	}
	ms, err := t.AniList.ByID(ctx, []int{id})
	if err != nil || len(ms) == 0 {
		return nil, err
	}
	var chain []Media
	seen := map[int]bool{id: true}
	ids := ms[0].Prequels
	for depth := 0; depth < 4 && len(ids) > 0; depth++ {
		next, err := t.AniList.ByID(ctx, ids)
		if err != nil {
			return nil, err
		}
		ids = nil
		for _, m := range next {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			chain = append(chain, m)
			ids = append(ids, m.Prequels...)
		}
	}
	if !t.DryRun {
		t.State.set(ctx, key, prequelCache{At: t.now(), Chain: chain})
	}
	return chain, nil
}

// findBatch picks the best complete-season 480p batch of show p among Nyaa items: English subtitles,
// recent, covering the whole season when AniList says how long it is.
func (t *Tracker) findBatch(items []NyaaItem, p Media) *match {
	window := t.Settings.BatchWindow
	if window <= 0 {
		window = DefaultBatchWindow
	}
	var best *match
	for _, it := range items {
		if it.Remake || it.Published.IsZero() || t.now().Sub(it.Published) > window {
			continue // FreshItems would drop batches; here they are the point
		}
		r, c := ParseNamed(it.Title, p.Aliases(), seasonNumber(p))
		if c < MatchThreshold || r.Kind != "batch" || r.Resolution != "480p" || !EnglishSubs(it.Title) || avoided(it.Title, t.Settings.Avoid) {
			continue
		}
		if p.Episodes > 0 && r.EpisodeEnd > 0 && int(r.EpisodeEnd) < p.Episodes {
			continue // part of a season, not all of it
		}
		m := match{item: it, rel: r, conf: c}
		if best == nil {
			best = &m
			continue
		}
		gm, gb := groupRank(m.rel.Group, t.Settings.PreferGroups), groupRank(best.rel.Group, t.Settings.PreferGroups)
		switch {
		case gm != gb:
			if gm < gb {
				best = &m
			}
		case m.item.Trusted && !best.item.Trusted, m.item.Trusted == best.item.Trusted && m.item.Seeders > best.item.Seeders:
			best = &m
		}
	}
	return best
}

// batches looks, for every show the user follows, at its earlier seasons: when a complete season of one
// shows up as a new 480p batch (and the user has none of that show), an approval card offers it. It is
// offered once per season (again only if the card timed out, like new shows).
func (t *Tracker) batches(ctx context.Context, lib Library, rep *Report) error {
	if t.AniList == nil || t.Nyaa == nil {
		return nil
	}
	titles, err := t.State.Titles(ctx)
	if err != nil {
		return err
	}
	known := map[int]TitleState{}
	for _, ts := range titles {
		known[ts.ID] = ts
	}
	for _, ts := range titles {
		if ts.Status != Approved || ts.ID <= 0 || ts.Follow != nil || ts.Batch != nil {
			continue
		}
		chain, err := t.prequelChain(ctx, ts.ID)
		if err != nil {
			rep.Problems = append(rep.Problems, "earlier seasons of "+ts.Title+": "+err.Error())
			continue
		}
		for _, p := range chain {
			if p.Format != "" && p.Format != "TV" && p.Format != "TV_SHORT" {
				continue
			}
			if k, ok := known[p.ID]; ok && !(k.Status == NotNow && k.Offered < t.Settings.ApprovalRetries) {
				continue
			}
			if p.Status != "" && p.Status != "FINISHED" || lib.HasShow(p) {
				continue
			}
			q := p.English
			if q == "" {
				q = p.Romaji
			}
			items, err := t.Nyaa.Search(ctx, q)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("looking for %s: %v", q, err))
				continue
			}
			b := t.findBatch(items, p)
			if b == nil {
				continue
			}
			title := p.Title(t.Settings.Lang)
			line := fmt.Sprintf("complete season: %s · 480p · %s, %s", title, b.item.Size, b.rel.Group)
			if t.DryRun || t.Settings.NotifyOnly {
				note := line + " (dry run: no approval sent)"
				if t.Settings.NotifyOnly && !t.DryRun {
					note = line + " (notify-only: no approval sent)"
				}
				rep.Offered = append(rep.Offered, note)
				continue
			}
			it, err := t.Ask.Create(ctx, approvals.New{Source: "skill", Kind: "anime",
				Summary: fmt.Sprintf("Add the complete season %q (480p batch, %s)", title, b.item.Size),
				Details: map[string]any{"anilist_id": p.ID, "release": b.item.Title, "size": b.item.Size, "group": b.rel.Group,
					"why": "an earlier season of " + ts.Title + ", which you follow; a complete 480p batch was just uploaded", "url": p.URL},
				Fields:  []approvals.Field{{Name: "dest", Label: "Save to", Type: "folder", Prefill: ts.Dest}},
				Timeout: untilSeasonEnd(t.now())})
			if err != nil {
				return err
			}
			k := known[p.ID]
			prev := k.Offered
			nt := TitleState{ID: p.ID, Title: title, Aliases: p.Aliases(), Season: seasonNumber(p), Episodes: p.Episodes,
				Status: Pending, ApprovalID: it.ID, Offered: prev + 1, Resolution: "480p", Year: p.Year, Format: p.Format,
				Why: "complete season of an earlier season of " + ts.Title, Batch: &BatchOffer{Item: b.item, Group: b.rel.Group}}
			if err := t.State.PutTitle(ctx, nt); err != nil {
				return err
			}
			known[p.ID] = nt
			rep.Offered = append(rep.Offered, line)
		}
	}
	return nil
}

// addBatches hands every approved batch to the torrent app once; its files are filed like any download.
func (t *Tracker) addBatches(ctx context.Context, titles []TitleState, all []Torrent, rep *Report) error {
	for _, ts := range titles {
		if ts.Status != Approved || ts.Batch == nil || ts.Batch.Added {
			continue
		}
		label := fmt.Sprintf("%s (complete season)", ts.Title)
		it := ts.Batch.Item
		if err := t.Nyaa.Resolve(ctx, &it); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if Downloading(all, it) {
			rep.Skipped = append(rep.Skipped, label+": already in "+t.Client.Name())
			ts.Batch.Added = true
		} else if t.DryRun {
			rep.Added = append(rep.Added, label+" (dry run)")
			continue
		} else if err := t.Client.AddTorrent(ctx, it.TorrentURL); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
			continue
		} else {
			ts.Batch.Added = true
			rep.Added = append(rep.Added, fmt.Sprintf("%s · %s", label, strings.TrimSpace(it.Size)))
		}
		if err := t.State.PutTitle(ctx, ts); err != nil {
			return err
		}
	}
	return nil
}
