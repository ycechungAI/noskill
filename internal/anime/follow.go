package anime

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Follow is a show the user asked for by name in config, outside the "new shows this season" flow:
// a long-running donghua, say, where each week the one newest episode is wanted, as the smallest file
// on Nyaa. Putting it in config is the approval: no card is sent. SKILL.md "Followed shows".
type Follow struct {
	Title      string   `json:"title"`
	Search     string   `json:"search,omitempty"`      // Nyaa query; default: Title
	Match      []string `json:"match"`                 // a release must contain one of these (case-insensitive)
	Dest       string   `json:"dest"`                  // the folder the episodes go into (volume:RED/@DONG+ works)
	Name       string   `json:"name,omitempty"`        // files are "<Name> Episode <n>.<ext>"; default: Title
	WindowDays int      `json:"window_days,omitempty"` // only releases this recent count as "this week's"; default 10
	From       int      `json:"from,omitempty"`        // episodes up to this one are yours already (default: the newest in the folder)
}

func (f Follow) search() string {
	if f.Search != "" {
		return f.Search
	}
	return f.Title
}

func (f Follow) name() string {
	if f.Name != "" {
		return f.Name
	}
	return f.Title
}

func (f Follow) window() time.Duration {
	if f.WindowDays > 0 {
		return time.Duration(f.WindowDays) * 24 * time.Hour
	}
	return 10 * 24 * time.Hour
}

// normalize lowercases and unifies apostrophes so "Mortal’s" matches "mortal's".
func normalize(s string) string {
	return strings.ToLower(strings.NewReplacer("’", "'", "‘", "'", "`", "'").Replace(s))
}

// matches says whether a release name mentions the show.
func (f Follow) matches(name string) bool {
	n := normalize(name)
	for _, m := range f.Match {
		if m = normalize(strings.TrimSpace(m)); m != "" && strings.Contains(n, m) {
			return true
		}
	}
	return false
}

// followID is the id of a followed show's TitleState: negative, so it can't clash with AniList's.
func followID(title string) int {
	h := fnv.New32a()
	h.Write([]byte(normalize(title)))
	return -int(h.Sum32()>>1) - 1000
}

var epInName = regexp.MustCompile(`(?i)\b(?:episode|ep)[ ._-]*(\d{1,4})\b`)

// localEpisodes lists the episode numbers of the show's files in dest ("<Name> Episode 161.mkv"; also
// "EP161"), looking only at files that start with the show's name.
func (f Follow) localEpisodes(dest string) map[int]string {
	out := map[int]string{}
	ents, err := os.ReadDir(dest)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() || !videoExt.MatchString(e.Name()) || !strings.HasPrefix(normalize(e.Name()), normalize(f.name())) {
			continue
		}
		if m := epInName.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			out[n] = filepath.Join(dest, e.Name())
		}
	}
	return out
}

// FollowTarget is where a followed episode goes: flat in the folder, named as the user names theirs.
func FollowTarget(dest string, f Follow, episode float64, ext string) string {
	if ext == "" {
		ext = "mkv"
	}
	return filepath.Join(dest, fmt.Sprintf("%s Episode %s.%s", f.name(), strconv.FormatFloat(episode, 'f', -1, 64), ext))
}

// syncFollows makes sure every followed show has a TitleState (approved, with its folder granted) so
// filing finds it. A folder that can't be reached (drive not plugged in) is reported and skipped.
func (t *Tracker) syncFollows(ctx context.Context, rep *Report) []TitleState {
	var out []TitleState
	for _, f := range t.Settings.Follows {
		if f.Title == "" || len(f.Match) == 0 || f.Dest == "" {
			rep.Problems = append(rep.Problems, fmt.Sprintf("follow %q needs a title, match words and a dest", f.Title))
			continue
		}
		abs, err := t.resolve(f.Dest)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: folder %q: %v (plug the drive in; it's tried on the next run)", f.Title, f.Dest, err))
			continue
		}
		portable := abs
		if t.Portable != nil {
			portable = t.Portable(abs)
		}
		id := followID(f.Title)
		ts, ok, err := t.State.Title(ctx, id)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", f.Title, err))
			continue
		}
		if !ok {
			ts = TitleState{ID: id, Title: f.Title, ApprovedAt: t.now()}
		}
		ts.Aliases, ts.Status, ts.Dest, ts.Follow = f.Match, Approved, portable, &f
		if !t.DryRun {
			if err := t.State.Grant(ctx, portable); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", f.Title, err))
				continue
			}
			if err := t.State.PutTitle(ctx, ts); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", f.Title, err))
				continue
			}
		}
		out = append(out, ts)
	}
	return out
}

// followDownload queues, for each followed show, the newest episode of the week: the smallest file
// among its English-subtitled releases. Shows start from what the user already has in the folder (or,
// with nothing there, from the newest episode out now), so a back catalogue is never fetched.
func (t *Tracker) followDownload(ctx context.Context, rep *Report, all []Torrent) error {
	for _, ts := range t.syncFollows(ctx, rep) {
		f := *ts.Follow
		abs, err := t.resolve(ts.Dest)
		if err != nil {
			continue
		}
		items, err := t.Nyaa.Search(ctx, f.search())
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", f.Title, err))
			continue
		}
		type cand struct {
			item NyaaItem
			rel  Release
			size int64
		}
		var cands []cand
		latest := 0.0
		for _, it := range FreshItems(items, f.window(), t.now()) {
			if !f.matches(it.Title) || !EnglishSubs(it.Title) {
				continue
			}
			r, _ := ParseNamed(it.Title, f.Match, 0)
			if r.Kind != "episode" || r.Episode <= 0 || r.EpisodeEnd > r.Episode {
				continue // batches and collections
			}
			size := ParseSize(it.Size)
			if size <= 0 {
				continue // no size, no way to tell which is smallest
			}
			cands = append(cands, cand{it, r, size})
			latest = max(latest, r.Episode)
		}
		if latest == 0 {
			continue
		}
		local := f.localEpisodes(abs)
		base := max(ts.FollowFrom, f.From)
		if base == 0 {
			for n := range local {
				base = max(base, n)
			}
			if base == 0 {
				base = int(latest) // nothing in the folder: start from what is out now, fetch nothing old
			}
			ts.FollowFrom = base
			if !t.DryRun {
				if err := t.State.PutTitle(ctx, ts); err != nil {
					return err
				}
			}
		}
		n := int(latest)
		key := strconv.Itoa(n)
		if n <= base || local[n] != "" || ts.Fallback[key] != "" {
			continue
		}
		var best *cand
		for i := range cands {
			c := &cands[i]
			if int(c.rel.Episode) == n && (best == nil || c.size < best.size) {
				best = c
			}
		}
		if best == nil {
			continue
		}
		label := fmt.Sprintf("%s episode %s", f.Title, key)
		if err := t.Nyaa.Resolve(ctx, &best.item); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if Downloading(all, best.item) {
			rep.Skipped = append(rep.Skipped, label+": already in "+t.Client.Name())
			continue
		}
		line := fmt.Sprintf("%s (smallest of %d: %s, %s)", label, countEpisode(cands, n, func(c cand) float64 { return c.rel.Episode }), humanBytes(uint64(best.size)), best.rel.Group)
		if t.DryRun {
			rep.Added = append(rep.Added, line+" (dry run)")
			continue
		}
		if err := t.Client.AddTorrent(ctx, best.item.TorrentURL); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if ts.Fallback == nil {
			ts.Fallback = map[string]string{}
		}
		ts.Fallback[key] = "queued"
		if err := t.State.PutTitle(ctx, ts); err != nil {
			return err
		}
		rep.Added = append(rep.Added, line)
	}
	return nil
}

func countEpisode[T any](l []T, n int, ep func(T) float64) int {
	c := 0
	for _, x := range l {
		if int(ep(x)) == n {
			c++
		}
	}
	return c
}
