package anime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/approvals"
)

// Asker creates and reads approvals (*approvals.Queue).
type Asker interface {
	Create(ctx context.Context, n approvals.New) (approvals.Item, error)
	Get(ctx context.Context, id string) (approvals.Item, error)
}

// Filer does the file effects (*perms.Broker): permission-checked, never overwriting, deletes go
// to the OS trash, and everything is in the undo log.
type Filer interface {
	Mkdir(ctx context.Context, p string) error
	// Rename gives a file in the download folder its final name before it is copied; it never replaces.
	Rename(ctx context.Context, from, to string) error
	Copy(ctx context.Context, from, to string) error
	TrashPath(ctx context.Context, p string) error
}

// Settings are the skill's settings (skills/anime-tracker/SKILL.md "Settings").
type Settings struct {
	Resolutions     []string      // preference order; default 480p, 720p, 1080p
	MinScore        float64       // default 0.5
	MaxNew          int           // titles offered per run; default 3
	FallbackAfter   time.Duration // wait this long for the preferred resolution; default 12 h
	EpisodeMaxAge   time.Duration // only uploads this recent; default 100 days (one season)
	ApprovalRetries int           // a timed-out approval is offered again this often; default 3
	DefaultDest     string        // the approval's "Save to" prefill before the user has picked one
	LibraryDirs     []string      // other folders where the user keeps anime (not written to)
	Lang            string        // title language: english (default) | romaji | native
	Avoid           []string      // release words to skip, e.g. "HEVC 10bit"
	PreferGroups    []string      // release groups tried first at the same resolution; default Erai-raws
	HDMaxMB         int           // 720p and above only when the file is this small (MB); 0 = no limit
	BatchWindow     time.Duration // an earlier season's batch counts as new when uploaded this recently; default 45 days
	Follows         []Follow      // shows followed by name, one newest episode a week, smallest file (follow.go)
	Naming          Naming
	// NotifyOnly (mode = "notify-only") sends the list of suggestions as a notification instead of
	// approval cards; nothing is downloaded until the user approves a title some other way.
	NotifyOnly bool
	// ReplaceVersions lets a fixed release (v2) of an episode noskill filed, at the same resolution,
	// replace it: the old file goes to the OS trash first (SKILL.md "Rules").
	ReplaceVersions bool
	// AskFirst (ask_before_download) asks the user about each new release before it is downloaded
	// (plan.go); without it releases of approved titles are added as they appear.
	AskFirst bool
}

func (s Settings) withDefaults() Settings {
	if len(s.Resolutions) == 0 {
		s.Resolutions = []string{"480p", "720p", "1080p"}
	}
	if s.MinScore == 0 {
		s.MinScore = 0.5
	}
	if s.MaxNew == 0 {
		s.MaxNew = 3
	}
	if s.FallbackAfter == 0 {
		s.FallbackAfter = 12 * time.Hour
	}
	if s.EpisodeMaxAge == 0 {
		s.EpisodeMaxAge = 100 * 24 * time.Hour
	}
	if s.ApprovalRetries == 0 {
		s.ApprovalRetries = 3
	}
	return s
}

// Tracker runs the anime tracker's stages. Everything that matters is decided here, in Go, so a
// small model only starts a run and reads the report (AGENTS.md).
type Tracker struct {
	State    *State
	AniList  *AniList
	Nyaa     *Nyaa
	Client   Downloader // nil: no usable torrent app; Detected says why
	Detected []ClientInfo
	Ask      Asker
	Files    Filer
	// ResolveDest turns a stored destination (volume:RED/ANIME2026 or a path) into a folder on this
	// machine; it fails when the drive isn't plugged in. Portable is the reverse, for storing.
	ResolveDest func(string) (string, error)
	Portable    func(string) string
	Settings    Settings
	DryRun      bool
	Now         func() time.Time
	// Free returns the bytes available on the volume holding a path (platform.DiskFree); nil skips
	// the free-space check.
	Free func(path string) (uint64, error)
	// Notify sends a notification (notify-only mode).
	Notify func(ctx context.Context, title, body string)
	// Only limits the file stage to the torrent with this hash: the hook a torrent app runs when a
	// download finishes (`noskill hook`).
	Only string
}

// Report is what a run did, for the user.
type Report struct {
	Client   string   `json:"client,omitempty"`
	Offered  []string `json:"offered,omitempty"`  // new titles sent for approval
	Approved []string `json:"approved,omitempty"` // answered yes since the last run
	Declined []string `json:"declined,omitempty"` // answered no or timed out
	Planned  []string `json:"planned,omitempty"`  // downloads waiting for the user's yes (ask_before_download)
	Added    []string `json:"added,omitempty"`    // torrents handed to the torrent app
	Stopped  []string `json:"stopped,omitempty"`  // finished torrents stopped from seeding
	Filed    []string `json:"filed,omitempty"`    // episodes renamed into the user's folders
	Skipped  []string `json:"skipped,omitempty"`  // with the reason: already have it, not new, …
	Problems []string `json:"problems,omitempty"` // needs the user: no torrent app, drive unplugged, …
	DryRun   bool     `json:"dry_run,omitempty"`
	UndoID   string   `json:"undo_id,omitempty"` // `noskill undo <id>` reverts this run's file changes
}

func (r Report) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("Dry run: nothing was changed.\n")
	}
	if r.Client != "" {
		fmt.Fprintf(&b, "Torrent app: %s\n", r.Client)
	}
	for _, s := range []struct {
		name string
		l    []string
	}{{"Problems", r.Problems}, {"Offered for approval", r.Offered}, {"Waiting for your yes", r.Planned}, {"Approved", r.Approved}, {"Declined", r.Declined},
		{"Downloading", r.Added}, {"Stopped seeding", r.Stopped}, {"Filed", r.Filed}, {"Skipped", r.Skipped}} {
		if len(s.l) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s:\n", s.name)
		for _, x := range s.l {
			fmt.Fprintf(&b, "  - %s\n", x)
		}
	}
	if r.UndoID != "" && len(r.Filed) > 0 {
		fmt.Fprintf(&b, "To put the files back: noskill undo %s\n", r.UndoID)
	}
	if b.Len() == 0 {
		return "Nothing to do."
	}
	return strings.TrimRight(b.String(), "\n")
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Stages, in run order.
var Stages = []string{"answers", "suggest", "batches", "download", "file"}

// Run runs one stage, or all of them ("" or "all"), and reports.
func (t *Tracker) Run(ctx context.Context, stage string) (Report, error) {
	rep, err := t.run(ctx, stage)
	if u, ok := t.Files.(interface{ UndoID() string }); ok {
		rep.UndoID = u.UndoID()
	}
	return rep, err
}

func (t *Tracker) run(ctx context.Context, stage string) (Report, error) {
	t.Settings = t.Settings.withDefaults()
	rep := Report{DryRun: t.DryRun}
	if t.Client != nil {
		rep.Client = t.Client.Name()
	}
	run := func(s string) bool { return stage == "" || stage == "all" || stage == s }
	known := false
	for _, s := range Stages {
		known = known || stage == s
	}
	if !known && stage != "" && stage != "all" {
		return rep, fmt.Errorf("unknown stage %q (one of %s, all)", stage, strings.Join(Stages, ", "))
	}
	lib := t.library(ctx)
	for _, s := range lib.Slow {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%s didn't answer in time and was left out of the library scan (a hung network drive?)", s))
	}
	if run("answers") {
		if err := t.answers(ctx, &rep); err != nil {
			return rep, err
		}
	}
	if run("suggest") {
		if err := t.suggest(ctx, lib, &rep); err != nil {
			rep.Problems = append(rep.Problems, "suggesting: "+err.Error())
		}
	}
	if (run("download") || run("file")) && t.Client == nil {
		note := NoClientNote
		for _, c := range t.Detected {
			if c.Supported && c.Note != "" {
				note = c.Note
				break
			}
		}
		rep.Problems = append(rep.Problems, note)
		return rep, nil
	}
	if run("batches") && t.Client != nil {
		if err := t.batches(ctx, lib, &rep); err != nil {
			rep.Problems = append(rep.Problems, "earlier seasons: "+err.Error())
		}
	}
	if run("download") {
		if err := t.download(ctx, lib, &rep); err != nil {
			rep.Problems = append(rep.Problems, "downloading: "+err.Error())
		}
	}
	if run("file") {
		if err := t.file(ctx, lib, &rep); err != nil {
			rep.Problems = append(rep.Problems, "filing: "+err.Error())
		}
	}
	return rep, nil
}

// library scans every folder the user keeps anime in: approved destinations that are plugged in,
// and library_dirs.
func (t *Tracker) library(ctx context.Context) Library {
	var dirs []string
	granted, _ := t.State.Granted(ctx)
	for _, g := range append(granted, t.Settings.LibraryDirs...) {
		if d, err := t.resolve(g); err == nil {
			dirs = append(dirs, d)
		}
	}
	return ScanLibrary(dirs, 0)
}

func (t *Tracker) resolve(dest string) (string, error) {
	if t.ResolveDest != nil {
		return t.ResolveDest(dest)
	}
	if !filepath.IsAbs(dest) {
		return "", fmt.Errorf("%q isn't a full path", dest)
	}
	return dest, nil
}

// ---- answers ----

// answers reads the user's replies to earlier approvals.
func (t *Tracker) answers(ctx context.Context, rep *Report) error {
	titles, err := t.State.Titles(ctx)
	if err != nil {
		return err
	}
	for _, ts := range titles {
		if ts.Status != Pending || ts.ApprovalID == "" {
			continue
		}
		it, err := t.Ask.Get(ctx, ts.ApprovalID)
		if errors.Is(err, approvals.ErrNotFound) {
			ts.Status, ts.ApprovalID = NotNow, ""
		} else if err != nil {
			return err
		}
		switch it.Status {
		case approvals.Pending:
			continue
		case approvals.Approved:
			dest := it.FieldValues()["dest"]
			abs, err := t.resolve(dest)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: folder %q: %v", ts.Title, dest, err))
				continue
			}
			portable := abs
			if t.Portable != nil {
				portable = t.Portable(abs)
			}
			if !t.DryRun {
				if err := t.State.Grant(ctx, portable); err != nil {
					return err
				}
				t.State.SetLastDestination(ctx, portable)
			}
			ts.Status, ts.Dest, ts.ApprovedAt = Approved, portable, t.now()
			rep.Approved = append(rep.Approved, fmt.Sprintf("%s → %s", ts.Title, abs))
		case approvals.Skipped:
			ts.Status = Skipped
			rep.Declined = append(rep.Declined, ts.Title)
		case approvals.Expired:
			ts.Status = NotNow
			if ts.Offered >= t.Settings.ApprovalRetries {
				ts.Status = Skipped
			}
			rep.Declined = append(rep.Declined, ts.Title+" (no answer)")
		}
		ts.ApprovalID = ""
		if !t.DryRun {
			if err := t.State.PutTitle(ctx, ts); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- suggest ----

func (t *Tracker) suggest(ctx context.Context, lib Library, rep *Report) error {
	known := map[int]TitleState{}
	titles, err := t.State.Titles(ctx)
	if err != nil {
		return err
	}
	var liked []int
	for _, ts := range titles {
		known[ts.ID] = ts
		if ts.Status == Approved || ts.Status == Finished {
			liked = append(liked, ts.ID)
		}
	}
	now := t.now()
	cur, y, next, ny := SeasonOf(now)
	prev, py := prevSeason(cur, y) // two-cour shows that started last season and still air
	var cands []Media
	for _, s := range []struct {
		season string
		year   int
	}{{cur, y}, {next, ny}, {prev, py}} {
		ms, err := t.AniList.Season(ctx, s.season, s.year, 50)
		if err != nil {
			return err
		}
		cands = append(cands, ms...)
	}
	// Taste: what the user approved plus what they already keep (SKILL.md "Taste").
	owned := t.State.LibraryIDs(ctx)
	for _, id := range owned {
		if !slices.Contains(liked, id) {
			liked = append(liked, id)
		}
	}
	var profile []Media
	if len(liked) > 0 {
		profile, _ = t.AniList.ByID(ctx, liked[:min(len(liked), 50)])
	}
	type scored struct {
		m     Media
		score float64
		why   string
	}
	var keep []scored
	seen := map[int]bool{}
	for _, m := range cands {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if !IsNew(m, now) {
			continue // finished or older shows are never suggested
		}
		if k, ok := known[m.ID]; ok && !(k.Status == NotNow && k.Offered < t.Settings.ApprovalRetries) {
			continue
		}
		if slices.Contains(owned, m.ID) || lib.HasShow(m) {
			rep.Skipped = append(rep.Skipped, m.Title(t.Settings.Lang)+": already in your folders")
			continue
		}
		s, why := Taste(m, profile)
		if s >= t.Settings.MinScore {
			keep = append(keep, scored{m, s, why})
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].score > keep[j].score })
	if len(keep) > t.Settings.MaxNew {
		keep = keep[:t.Settings.MaxNew]
	}
	prefill := t.State.LastDestination(ctx)
	if prefill == "" {
		prefill = t.Settings.DefaultDest
	}
	if p, err := t.resolve(prefill); err == nil {
		prefill = p
	}
	for _, k := range keep {
		title := k.m.Title(t.Settings.Lang)
		ts := known[k.m.ID]
		ts.ID, ts.Title, ts.Aliases, ts.Episodes, ts.Year, ts.Format = k.m.ID, title, k.m.Aliases(), k.m.Episodes, k.m.Year, k.m.Format
		ts.Season = seasonNumber(k.m)
		ts.Why = k.why
		ts.Resolution = t.Settings.Resolutions[0]
		avail := t.available(ctx, ts)
		if len(avail) > 0 && !contains(avail, ts.Resolution) {
			for _, r := range t.Settings.Resolutions {
				if contains(avail, r) {
					ts.Resolution = r
					break
				}
			}
		}
		line := fmt.Sprintf("%s · %s", title, ts.Resolution)
		if t.Settings.NotifyOnly {
			note := line
			if k.why != "" {
				note += ": " + k.why
			}
			rep.Offered = append(rep.Offered, note+" (notify-only: no approval sent)")
			continue
		}
		if t.DryRun {
			rep.Offered = append(rep.Offered, line+" (dry run: no approval sent)")
			continue
		}
		releases := "no English-subbed releases yet"
		if len(avail) > 0 {
			releases = "on Nyaa in " + strings.Join(avail, ", ")
		}
		it, err := t.Ask.Create(ctx, approvals.New{Source: "skill", Kind: "anime",
			Summary: fmt.Sprintf("Add %q · %s", title, ts.Resolution),
			Details: map[string]any{"anilist_id": k.m.ID, "why": k.why, "releases": releases, "url": k.m.URL,
				"season": strings.ToLower(k.m.Season) + " " + fmt.Sprint(k.m.Year), "synopsis": k.m.Synopsis},
			Fields: []approvals.Field{{Name: "dest", Label: "Save to", Type: "folder", Prefill: prefill}},
			// A card stays until the season ends (Fall: 31 December), not a day: the user can come back
			// to a "maybe" any time this season.
			Timeout: untilSeasonEnd(t.now())})
		if err != nil {
			return err
		}
		ts.Status, ts.ApprovalID = Pending, it.ID
		ts.Offered++
		if err := t.State.PutTitle(ctx, ts); err != nil {
			return err
		}
		rep.Offered = append(rep.Offered, line)
	}
	if t.Settings.NotifyOnly && !t.DryRun && len(rep.Offered) > 0 && t.Notify != nil {
		t.Notify(ctx, "New anime this season", strings.Join(rep.Offered, "\n"))
	}
	return nil
}

func prevSeason(cur string, y int) (string, int) {
	names := []string{"WINTER", "SPRING", "SUMMER", "FALL"}
	i := seasonIndex[cur]
	if i == 0 {
		return names[3], y - 1
	}
	return names[i-1], y
}

// seasonNumber reads "Season 2" / "2nd Season" from the show's own names; 0 = first season.
func seasonNumber(m Media) int {
	for _, a := range m.Aliases() {
		if _, s := splitSeason(Normalize(a)); s > 1 {
			return s
		}
	}
	return 0
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Taste scores a show against the shows the user approved (0–1) and says why, deterministically:
// shared genres and tags, AniList recommendations from liked shows, and its rating. With nothing
// approved yet it goes by rating and popularity.
func Taste(m Media, liked []Media) (float64, string) {
	rating := float64(m.Score) / 100
	if m.Score == 0 {
		rating = 0.6 // not aired yet: no rating, so neither a plus nor a minus
	}
	if len(liked) == 0 {
		pop := min(float64(m.Popular)/100000, 1)
		return 0.6*rating + 0.4*pop, fmt.Sprintf("popular this season (rated %d%%)", m.Score)
	}
	genres, tags := map[string]int{}, map[string]int{}
	rec := ""
	for _, l := range liked {
		for _, g := range l.Genres {
			genres[g]++
		}
		for _, tg := range l.Tags {
			if tg.Rank >= 60 {
				tags[tg.Name]++
			}
		}
		if l.Recs[m.ID] > 0 && rec == "" {
			rec = l.Title("")
		}
	}
	var shared []string
	g := 0.0
	for _, x := range m.Genres {
		if genres[x] > 0 {
			g++
			shared = append(shared, x)
		}
	}
	if len(m.Genres) > 0 {
		g /= float64(len(m.Genres))
	}
	tg, n := 0.0, 0
	for _, x := range m.Tags {
		if x.Rank >= 60 {
			n++
			if tags[x.Name] > 0 {
				tg++
			}
		}
	}
	if n > 0 {
		tg /= float64(n)
	}
	r := 0.0
	if rec != "" {
		r = 1
	}
	score := 0.4*g + 0.25*tg + 0.2*r + 0.15*rating
	var why []string
	if len(shared) > 0 {
		why = append(why, strings.Join(shared[:min(3, len(shared))], ", "))
	}
	if rec != "" {
		why = append(why, "recommended by fans of "+rec)
	}
	if len(m.Studios) > 0 {
		why = append(why, "by "+m.Studios[0])
	}
	return score, strings.Join(why, "; ")
}

// ---- download ----

// match is one feed item that is an episode of a title.
type match struct {
	item NyaaItem
	rel  Release
}

// releases returns a title's fresh English-subbed episodes on Nyaa, by episode key.
func (t *Tracker) releases(ctx context.Context, ts TitleState) (map[string][]match, error) {
	query := ts.Title
	if len(ts.Aliases) > 1 {
		query = ts.Aliases[1] // romaji: what release groups use
	}
	items, err := t.Nyaa.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	out := map[string][]match{}
	for _, it := range FreshItems(items, t.Settings.EpisodeMaxAge, t.now()) {
		r, c := ParseNamed(it.Title, ts.Aliases, ts.Season)
		if c < MatchThreshold || avoided(it.Title, t.Settings.Avoid) {
			continue
		}
		if !EnglishSubs(it.Title) || tooBig(r.Resolution, it.Size, t.Settings.HDMaxMB) {
			continue // not English subtitles, or a big 720p-or-higher file
		}
		if r.Kind == "episode" && ts.Episodes > 0 && r.Episode > float64(ts.Episodes) {
			continue // numbered across seasons ("S03E49"): it would be misnamed
		}
		out[EpisodeKey(r)] = append(out[EpisodeKey(r)], match{it, r})
	}
	return out, nil
}

// ParseNamed reads a feed title that may give several names for one release, joined by " | "
// ("[G] Show Season 3 - 02 [1080p] | Romaji - 50 | Show - S03E02"): the part that best matches the
// title's aliases wins, and episode and resolution come from the first part when it lacks them.
func ParseNamed(name string, aliases []string, season int) (Release, float64) {
	parts := strings.Split(name, " | ")
	first := ParseRelease(parts[0])
	best, bestC := first, Confidence(first.Title, first.Season, aliases, season)
	for _, p := range parts[1:] {
		r := ParseRelease(p)
		if r.Episode == 0 && r.Kind != "movie" {
			r.Episode, r.EpisodeEnd, r.Kind = first.Episode, first.EpisodeEnd, first.Kind
			if r.Season == 0 {
				r.Season = first.Season
			}
		}
		if r.Resolution == "" {
			r.Resolution = first.Resolution
		}
		if c := Confidence(r.Title, r.Season, aliases, season); c > bestC {
			best, bestC = r, c
		}
	}
	best.Name = name
	return best, bestC
}

func avoided(name string, avoid []string) bool {
	n := Normalize(name)
	for _, a := range avoid {
		if a = Normalize(a); a != "" && strings.Contains(n, a) {
			return true
		}
	}
	return false
}

// available lists the resolutions a title's episodes come in on Nyaa.
func (t *Tracker) available(ctx context.Context, ts TitleState) []string {
	eps, err := t.releases(ctx, ts)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ms := range eps {
		for _, m := range ms {
			if r := m.rel.Resolution; r != "" && !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// download hands each approved title's new episodes to the torrent app: the preferred resolution,
// or after fallback_after the best other one. Episodes the user already has, or that the torrent
// app already has, are skipped.
func (t *Tracker) download(ctx context.Context, lib Library, rep *Report) error {
	titles, err := t.State.Titles(ctx)
	if err != nil {
		return err
	}
	all, err := t.Client.All(ctx)
	if err != nil {
		return err
	}
	if !t.DryRun {
		if err := t.Client.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := t.followDownload(ctx, rep, all); err != nil {
		return err
	}
	if err := t.addBatches(ctx, titles, all, rep); err != nil {
		return err
	}
	for _, ts := range titles {
		if ts.Status != Approved || ts.Follow != nil || ts.Batch != nil {
			continue
		}
		eps, err := t.releases(ctx, ts)
		if err != nil {
			return err
		}
		have := lib.Has(ts)
		keys := make([]string, 0, len(eps))
		for k := range eps {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return episodeLess(keys[i], keys[j]) })
		changed := false
		for _, k := range keys {
			label := fmt.Sprintf("%s %s", ts.Title, k)
			if _, queued := ts.Fallback[k]; queued {
				continue
			}
			var pick *match
			var why string
			if p := have[k]; p != "" {
				// Stored already; only a fixed release (v2) of what noskill filed is worth fetching.
				if pick = t.newer(ts, k, p, eps[k]); pick == nil {
					continue
				}
				label += fmt.Sprintf(" v%d", pick.rel.Version)
			} else {
				pick, why = t.pick(eps[k], ts.Resolution)
			}
			if pick == nil {
				if why != "" {
					rep.Skipped = append(rep.Skipped, label+": "+why)
				}
				continue
			}
			if err := t.Nyaa.Resolve(ctx, &pick.item); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			if Downloading(all, pick.item) {
				rep.Skipped = append(rep.Skipped, label+": already in "+t.Client.Name())
				continue
			}
			line := fmt.Sprintf("%s (%s)", label, pick.rel.Resolution)
			planKey := ReleaseKey(ts.Title, k)
			if v := pick.rel.Version; v > 1 {
				planKey += fmt.Sprintf(" v%d", v)
			}
			if yes, err := t.ask(ctx, rep, PlanItem{Kind: "release", Key: planKey, Title: ts.Title, Episode: k, Name: pick.item.Title, SizeText: pick.item.Size, URL: pick.item.TorrentURL}); err != nil {
				return err
			} else if !yes {
				continue
			}
			if t.DryRun {
				rep.Added = append(rep.Added, line+" (dry run)")
				continue
			}
			if err := t.Client.AddTorrent(ctx, pick.item.TorrentURL); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", label, err))
				continue
			}
			t.planAdded(ctx, planKey)
			if ts.Fallback == nil {
				ts.Fallback = map[string]string{}
			}
			ts.Fallback[k] = pick.rel.Resolution
			changed = true
			rep.Added = append(rep.Added, line)
		}
		if changed {
			if err := t.State.PutTitle(ctx, ts); err != nil {
				return err
			}
		}
	}
	return nil
}

// pick chooses one release of an episode: the preferred resolution (trusted uploaders, then most
// seeders), else once the episode has been out fallback_after, the best other resolution.
func (t *Tracker) pick(ms []match, want string) (*match, string) {
	best := func(res string) *match {
		var b *match
		for i := range ms {
			m := &ms[i]
			if m.rel.Resolution != res {
				continue
			}
			if b == nil {
				b = m
				continue
			}
			gm, gb := groupRank(m.rel.Group, t.Settings.PreferGroups), groupRank(b.rel.Group, t.Settings.PreferGroups)
			switch {
			case gm != gb:
				if gm < gb {
					b = m
				}
			case m.item.Trusted && !b.item.Trusted, m.item.Trusted == b.item.Trusted && m.item.Seeders > b.item.Seeders:
				b = m
			}
		}
		return b
	}
	if m := best(want); m != nil {
		return m, ""
	}
	oldest := t.now()
	for _, m := range ms {
		if m.item.Published.Before(oldest) {
			oldest = m.item.Published
		}
	}
	if t.now().Sub(oldest) < t.Settings.FallbackAfter {
		return nil, "waiting for " + want
	}
	for _, r := range t.Settings.Resolutions {
		if m := best(r); m != nil {
			return m, ""
		}
	}
	return nil, "no release in " + strings.Join(t.Settings.Resolutions, ", ")
}

// episodeLess orders "2" < "10" < "12.5" < "SP1" < "movie".
func episodeLess(a, b string) bool {
	var x, y float64
	_, ea := fmt.Sscan(a, &x)
	_, eb := fmt.Sscan(b, &y)
	switch {
	case ea == nil && eb == nil:
		return x < y
	case ea == nil:
		return true
	case eb == nil:
		return false
	}
	return a < b
}

// ---- file ----

// FilingBusy is what a run reports when another run holds the filing lock.
const FilingBusy = "filing: another run is filing"

// file renames finished downloads into the title's folder, one at a time: copy (verified), then
// remove the torrent keeping its files, then trash the download. Nothing is overwritten.
func (t *Tracker) file(ctx context.Context, lib Library, rep *Report) error {
	holder := fmt.Sprintf("run-%d", t.now().UnixNano())
	if !t.DryRun {
		ok, err := t.State.Lock(ctx, holder, 30*time.Minute)
		if err != nil {
			return err
		}
		if !ok {
			rep.Skipped = append(rep.Skipped, FilingBusy)
			return nil
		}
		defer t.State.Unlock(ctx, holder)
	}
	done, err := t.Client.Completed(ctx)
	if err != nil {
		return err
	}
	titles, err := t.State.Titles(ctx)
	if err != nil {
		return err
	}
	plans, err := t.State.Plans(ctx)
	if err != nil {
		return err
	}
	if t.Only != "" {
		found := slices.ContainsFunc(done, func(tor Torrent) bool { return strings.EqualFold(tor.Hash, t.Only) })
		done = slices.DeleteFunc(done, func(tor Torrent) bool { return !strings.EqualFold(tor.Hash, t.Only) })
		if !found {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: not a finished noskill download (it's filed on a later run if it is)", t.Only))
		}
	}
	for _, tor := range done {
		// A finished download stops seeding now: filing doesn't wait for a share limit, and the
		// torrent is removed (files kept) once its episodes are stored.
		if !tor.Stopped() {
			switch {
			case t.DryRun:
				rep.Stopped = append(rep.Stopped, tor.Name+" (dry run)")
			case t.Client.Stop(ctx, tor.Hash) != nil:
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: couldn't stop it seeding", tor.Name))
			default:
				rep.Stopped = append(rep.Stopped, tor.Name)
			}
		}
		if item, ok := manualFor(plans, tor.Hash); ok {
			// A download the user added: every file goes to the folder they chose.
			if placed, all := t.fileManual(ctx, tor, item, rep); all {
				t.removeAndTrash(ctx, tor, placed, rep)
			}
			continue
		}
		if t.State.Ignored(ctx, tor.Hash) {
			continue
		}
		files, err := t.Client.Files(ctx, tor.Hash)
		if err != nil {
			return err
		}
		// The download folder is not the library: the episode being filed sits there (the scan looks
		// in Downloads for shows the user already has), so files under the torrent's save path don't
		// count as "already have it". Without this a finished download looked stored and was trashed
		// unfiled.
		own := Library{Files: slices.DeleteFunc(slices.Clone(lib.Files), func(o Owned) bool {
			return strictlyInside(tor.SavePath, o.Path)
		})}
		filed, unplaced, stop := 0, 0, false
		var placed []string // the videos of this torrent that are now stored
		for _, f := range files {
			if !videoExt.MatchString(f.Name) || f.Progress < 1 {
				continue
			}
			r := ParseRelease(filepath.Base(f.Name))
			ts, ok := whose(titles, r)
			if !ok {
				unplaced++
				why := "doesn't match an approved title"
				rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %s", filepath.Base(f.Name), why))
				if !t.DryRun {
					t.State.Ignore(ctx, tor.Hash, why)
				}
				continue
			}
			dest, err := t.resolve(ts.Dest)
			if err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v (plug the drive in; it's filed on the next run)", ts.Title, err))
				stop = true
				break
			}
			src := filepath.Join(tor.SavePath, filepath.FromSlash(f.Name))
			target := t.Settings.Naming.PlanName(dest, ts, r)
			if ts.Follow != nil {
				target = FollowTarget(dest, *ts.Follow, r.Episode, r.Ext)
			}
			key := EpisodeKey(r)
			// The file is renamed in the download folder before it is moved. If an earlier run did that
			// and then failed to copy, the file is under its new name already.
			planned := filepath.Join(filepath.Dir(src), filepath.Base(target))
			if _, err := os.Lstat(src); err != nil {
				if _, err2 := os.Lstat(planned); err2 == nil {
					src = planned
				}
			}
			old := ""
			have := own.Has(ts)
			if ts.Follow != nil {
				have = map[string]string{}
				if p := ts.Follow.localEpisodes(dest)[int(r.Episode)]; p != "" {
					have[key] = p
				}
			}
			if p := have[key]; p != "" {
				switch {
				case !t.replaces(ts, key, p, r):
					rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s %s: already have %s", ts.Title, key, p))
				case changedSince(p, ts.Filed[key].At):
					rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s %s: %s was changed after noskill filed it, so the fixed release isn't swapped in", ts.Title, key, p))
				default:
					old = p
				}
				if old == "" {
					placed = append(placed, src)
					filed++
					continue
				}
			}
			if t.DryRun {
				note := " (dry run)"
				if old != "" {
					note = fmt.Sprintf(" (dry run; replaces %s)", filepath.Base(old))
				}
				rep.Filed = append(rep.Filed, fmt.Sprintf("%s → %s%s", filepath.Base(f.Name), target, note))
				filed++
				continue
			}
			if err := t.space(src, filepath.Dir(target)); err != nil {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v (nothing was copied; it's filed on a later run)", filepath.Base(f.Name), err))
				stop = true
				break
			}
			if err := t.Files.Mkdir(ctx, filepath.Dir(target)); err != nil {
				return err
			}
			if old != "" {
				// The fixed release replaces the old file: it goes to the trash first, and is gone
				// from the filed list so a failed copy is retried instead of counted as stored.
				if err := t.Files.TrashPath(ctx, old); err != nil {
					rep.Problems = append(rep.Problems, fmt.Sprintf("%s: couldn't trash %s to replace it: %v", filepath.Base(f.Name), filepath.Base(old), err))
					stop = true
					break
				}
				delete(ts.Filed, key)
				if err := t.State.PutTitle(ctx, ts); err != nil {
					return err
				}
				lib.Files = slices.DeleteFunc(lib.Files, func(o Owned) bool { return o.Path == old })
				own.Files = slices.DeleteFunc(own.Files, func(o Owned) bool { return o.Path == old })
			}
			if src != planned {
				// Not fatal: if the name is taken or the rename fails, the copy gives the file its name.
				if err := t.Files.Rename(ctx, src, planned); err == nil {
					src = planned
				}
			}
			if err := t.Files.Copy(ctx, src, target); err != nil {
				msg := fmt.Sprintf("%s: %v", filepath.Base(f.Name), err)
				if old != "" {
					msg += fmt.Sprintf(" (the earlier release, %s, is in the trash; the fixed one is retried on the next run)", filepath.Base(old))
				}
				rep.Problems = append(rep.Problems, msg)
				stop = true
				break
			}
			placed = append(placed, src)
			if ts.Filed == nil {
				ts.Filed = map[string]FiledEpi{}
			}
			ts.Filed[key] = FiledEpi{Path: target, Version: r.Version, Resolution: r.Resolution, At: t.now()}
			delete(ts.Fallback, key)
			if err := t.State.PutTitle(ctx, ts); err != nil {
				return err
			}
			for i := range titles {
				if titles[i].ID == ts.ID {
					titles[i] = ts
				}
			}
			lib.Files = append(lib.Files, Owned{Path: target, Release: r})
			own.Files = append(own.Files, Owned{Path: target, Release: r})
			line := fmt.Sprintf("%s → %s", filepath.Base(f.Name), target)
			if old != "" {
				line += " (replaced the earlier release; the old file is in the trash)"
			}
			rep.Filed = append(rep.Filed, line)
			filed++
		}
		if stop {
			break
		}
		if filed == 0 || unplaced > 0 || t.DryRun {
			continue // a file it couldn't place stays in the download, and so does the torrent
		}
		t.removeAndTrash(ctx, tor, placed, rep)
	}
	return nil
}

// removeAndTrash takes a filed download out of the torrent app (its files stay) and moves what was
// stored from to the trash.
func (t *Tracker) removeAndTrash(ctx context.Context, tor Torrent, placed []string, rep *Report) {
	if err := t.Client.RemoveTorrent(ctx, tor.Hash); err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", tor.Name, err))
		return
	}
	content := tor.ContentPath
	if content == "" {
		content = filepath.Join(tor.SavePath, tor.Name)
	}
	// Only a download with a folder or file of its own goes to the trash as a whole. A torrent
	// made without a folder (qBittorrent's "don't create subfolder") reports the download
	// folder itself as its content, and that must never be trashed: only the videos placed are.
	// A single-file torrent's own path is gone once its file was renamed; the renamed one is
	// the download then.
	victims := placed
	if _, err := os.Lstat(content); err == nil && strictlyInside(tor.SavePath, content) {
		victims = []string{content}
	}
	for _, v := range victims {
		if _, err := os.Lstat(v); err != nil {
			continue
		}
		if err := t.Files.TrashPath(ctx, v); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("filed %s but couldn't move the download to the trash: %v", tor.Name, err))
			break
		}
	}
}

// strictlyInside reports whether p is below base (not base itself, not outside it).
func strictlyInside(base, p string) bool {
	if base == "" || p == "" {
		return false
	}
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// changedSince says whether the user touched the file after noskill filed it at "at" (a re-encode,
// a remux, added subtitles): it is theirs now and a fixed release must not replace it. Files copied
// by noskill carry the download's own modification time, which is older than the filing.
func changedSince(path string, at time.Time) bool {
	fi, err := os.Stat(path)
	return err == nil && !at.IsZero() && fi.ModTime().After(at.Add(time.Minute))
}

// newer returns the release of episode key that should replace the file noskill filed at path: the
// same resolution, a higher version. nil when replacing is off or the file isn't one noskill filed.
func (t *Tracker) newer(ts TitleState, key, path string, ms []match) *match {
	f, ok := ts.Filed[key]
	if !t.Settings.ReplaceVersions || !ok || f.Path != path {
		return nil
	}
	var best *match
	for i := range ms {
		if ms[i].rel.Resolution == f.Resolution && ms[i].rel.Version > max(f.Version, 1) &&
			(best == nil || ms[i].rel.Version > best.rel.Version) {
			best = &ms[i]
		}
	}
	return best
}

// replaces says whether release r is a fixed version of the file noskill filed at path.
func (t *Tracker) replaces(ts TitleState, key, path string, r Release) bool {
	f, ok := ts.Filed[key]
	return t.Settings.ReplaceVersions && ok && f.Path == path && r.Resolution == f.Resolution && r.Version > max(f.Version, 1)
}

// freeReserve is the share of a file's size that must stay free besides the file itself.
const freeReserve = 0.05

// space checks that the folder's drive has room for the file and a 5% reserve. It looks at the
// nearest folder that exists, since the title folder is made after the check.
func (t *Tracker) space(src, dir string) error {
	if t.Free == nil {
		return nil
	}
	st, err := os.Stat(src)
	if err != nil {
		return nil // the copy reports a missing source
	}
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		up := filepath.Dir(probe)
		if up == probe {
			return nil
		}
		probe = up
	}
	free, err := t.Free(probe)
	if err != nil {
		return nil // can't tell: let the copy decide
	}
	need := uint64(math.Ceil(float64(st.Size()) * (1 + freeReserve)))
	if free < need {
		return fmt.Errorf("not enough space on %s: needs %s, %s free", probe, humanBytes(need), humanBytes(free))
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// whose finds the approved title a release belongs to.
func whose(titles []TitleState, r Release) (TitleState, bool) {
	best, bestC := TitleState{}, 0.0
	for _, ts := range titles {
		if ts.Status != Approved && ts.Status != Finished {
			continue
		}
		if ts.Follow != nil {
			if ts.Follow.matches(r.Name) && bestC < 1 {
				best, bestC = ts, 1
			}
			continue
		}
		if c := Confidence(r.Title, r.Season, ts.Aliases, ts.Season); c > bestC {
			best, bestC = ts, c
		}
	}
	return best, bestC >= MatchThreshold
}

// Confirm looks shows up on AniList (at most limit lookups, cached in skill_kv) and keeps the
// ones that are anime: a search hit whose names match at MatchThreshold. Anything else (TV series,
// films, home videos) is dropped.
func (t *Tracker) Confirm(ctx context.Context, shows []Show, limit int) ([]Show, int) {
	var out []Show
	unchecked := 0
	for _, sh := range shows {
		key := sh.Title
		if sh.Season > 1 {
			key = fmt.Sprintf("%s S%d", sh.Title, sh.Season)
		}
		if id, ok := t.State.LibraryMatch(ctx, key); ok {
			if id > 0 {
				sh.AniList = id
				out = append(out, sh)
			}
			continue
		}
		if limit <= 0 {
			unchecked++
			continue
		}
		limit--
		ms, err := t.AniList.Search(ctx, key, 5)
		if err != nil {
			unchecked++
			continue
		}
		id := 0
		for _, m := range ms {
			if Confidence(sh.Title, sh.Season, m.Aliases(), seasonNumber(m)) >= MatchThreshold {
				id, sh.Name = m.ID, m.Title(t.Settings.Lang)
				break
			}
		}
		if !t.DryRun {
			t.State.SetLibraryMatch(ctx, key, id)
		}
		if id > 0 {
			sh.AniList = id
			out = append(out, sh)
		}
	}
	return out, unchecked
}
