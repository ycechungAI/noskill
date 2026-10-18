package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"noskill/internal/anime"
	"noskill/internal/config"
	"noskill/internal/ids"
	"noskill/internal/notify"
	"noskill/internal/perms"
	"noskill/internal/platform"
)

// skillValue reads [skills.<name>] values with dotted keys for tables (quality.prefer), ignoring
// any that came from a project config.
func (a *App) skillValue(skill, key string) (any, bool) {
	l := a.Opts.Loaded
	top, rest, _ := strings.Cut(key, ".")
	if l.SkillSources[skill][top] == config.LayerProject {
		return nil, false
	}
	var v any = map[string]any(l.Config.Skills.Settings[skill])
	for _, k := range append([]string{top}, splitDots(rest)...) {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

func splitDots(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ".")
}

func (a *App) animeString(key, def string) string {
	if v, ok := a.skillValue(anime.SkillName, key); ok {
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
			return s
		}
	}
	return def
}

func (a *App) animeList(key string, def []string) []string {
	v, ok := a.skillValue(anime.SkillName, key)
	if !ok {
		return def
	}
	switch x := v.(type) {
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	case []string:
		return x
	case string:
		return splitList(x)
	}
	return def
}

// animeFollows reads the [[skills.anime-tracker.follow]] tables: shows followed by name.
func (a *App) animeFollows() []anime.Follow {
	v, ok := a.skillValue(anime.SkillName, "follow")
	if !ok {
		return nil
	}
	var rows []map[string]any
	switch x := v.(type) {
	case []map[string]any:
		rows = x
	case []any:
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	}
	str := func(m map[string]any, k string) string {
		if s, ok := m[k]; ok {
			return strings.TrimSpace(fmt.Sprint(s))
		}
		return ""
	}
	var out []anime.Follow
	for _, m := range rows {
		f := anime.Follow{Title: str(m, "title"), Search: str(m, "search"), Dest: str(m, "dest"), Name: str(m, "name")}
		switch x := m["match"].(type) {
		case string:
			f.Match = []string{x}
		case []any:
			for _, e := range x {
				f.Match = append(f.Match, fmt.Sprint(e))
			}
		case []string:
			f.Match = x
		}
		if len(f.Match) == 0 && f.Title != "" {
			f.Match = []string{f.Title}
		}
		if n, err := strconv.Atoi(str(m, "window_days")); err == nil {
			f.WindowDays = n
		}
		if n, err := strconv.Atoi(str(m, "from")); err == nil {
			f.From = n
		}
		out = append(out, f)
	}
	return out
}

func (a *App) animeFloat(key string, def float64) float64 {
	if f, err := strconv.ParseFloat(a.animeString(key, ""), 64); err == nil {
		return f
	}
	return def
}

func (a *App) animeDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(a.animeString(key, "")); err == nil && d > 0 {
		return d
	}
	return def
}

// AnimeDetect lists the torrent apps on this machine (installed for this user or for everyone, and
// which have their remote access on).
func (a *App) AnimeDetect(ctx context.Context) []anime.ClientInfo {
	d := anime.Detector{}
	// A host the user set wins over the usual ports.
	for _, k := range []struct{ name, key string }{{"qBittorrent", "qbittorrent_host"}, {"Transmission", "transmission_host"}} {
		if h := a.animeString(k.key, ""); h != "" {
			if u, err := url.Parse("http://" + strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")); err == nil && u.Port() != "" {
				if p, err := strconv.Atoi(u.Port()); err == nil {
					if d.Ports == nil {
						d.Ports = map[string][]int{}
					}
					d.Ports[k.name] = []int{p}
				}
			}
		}
	}
	return d.Detect(ctx)
}

// AnimeTracker builds the anime tracker from the user's settings (skills/anime-tracker/SKILL.md):
// the torrent app it detected (or the one `client` names), Nyaa's English-translated feed, the
// approvals queue, and a file broker that may only write into folders the user approved.
func (a *App) AnimeTracker(ctx context.Context, dryRun bool) (*anime.Tracker, error) {
	res := a.resolver()
	state := &anime.State{DB: a.DB}
	downloads, err := res.Expand(a.animeString("download_dir", "$DOWNLOADS"))
	if err == nil {
		// The download folder is where noskill may trash finished downloads: it gets the same checks
		// as a folder picked in an approval (not a home folder, a drive root or a system folder).
		downloads, err = perms.ValidateApproved(downloads, a.Dirs.Own())
	}
	if err != nil {
		return nil, fmt.Errorf("download_dir: %w", err)
	}
	label := a.animeString("category", a.animeString("qbittorrent_category", "noskill-anime"))
	t := &anime.Tracker{
		State:   state,
		AniList: &anime.AniList{},
		Nyaa:    &anime.Nyaa{Base: a.animeString("nyaa_url", anime.NyaaURL), TrustedOnly: a.animeString("trusted_only", "false") == "true"},
		Ask:     a.Approvals,
		DryRun:  dryRun,
		ResolveDest: func(p string) (string, error) {
			exp, err := a.resolver().Expand(p)
			if err != nil {
				return "", err
			}
			return perms.ValidateApproved(exp, a.Dirs.Own())
		},
		Portable: a.portable,
		Settings: anime.Settings{
			Resolutions:     a.animeList("quality.prefer", nil),
			Avoid:           a.animeList("quality.avoid", []string{"HEVC 10bit"}),
			PreferGroups:    a.animeList("quality.prefer_groups", []string{"Erai-raws"}),
			HDMaxMB:         int(a.animeFloat("quality.hd_max_size_mb", anime.DefaultHDMaxMB)),
			Follows:         a.animeFollows(),
			FallbackAfter:   a.animeDuration("quality.fallback_after", 0),
			EpisodeMaxAge:   a.animeDuration("max_age", 0),
			MinScore:        a.animeFloat("min_score", 0),
			MaxNew:          int(a.animeFloat("max_new_per_run", 0)),
			ApprovalRetries: int(a.animeFloat("approval_retries", 0)),
			DefaultDest:     a.animeString("default_destination", "$VIDEOS/Anime"),
			LibraryDirs:     a.animeList("library_dirs", nil),
			Lang:            a.animeString("title_language", "english"),
			NotifyOnly:      a.animeString("mode", "approve") == "notify-only",
			ReplaceVersions: a.animeString("replace_versions", "true") != "false",
			AskFirst:        a.animeString("ask_before_download", "false") == "true",
			Naming: anime.Naming{Preset: a.animeString("naming", "simple"),
				Episodes:     strings.ToLower(a.animeString("episode_style", "sxxexx")),
				TitleFolders: a.animeString("title_folders", "true") != "false",
				Sanitize:     sanitizeName},
		},
	}
	if d, err := res.Expand(t.Settings.DefaultDest); err == nil {
		t.Settings.DefaultDest = d
	}
	t.Files = &animeFiler{a: a, state: state, downloads: downloads}
	t.Free = platform.DiskFree
	t.Notify = func(ctx context.Context, title, body string) {
		m := notify.Message{Title: title, Body: body}
		if names := a.animeList("notify", nil); len(names) > 0 {
			a.Hub.SendTo(ctx, names, m)
		} else {
			a.Hub.Send(ctx, m)
		}
	}
	// "Already stored" checks look everywhere the user keeps videos, external drives included.
	t.Settings.LibraryDirs = a.AnimeScanDirs(ctx)

	t.Detected = a.AnimeDetect(ctx)
	want := strings.ToLower(a.animeString("client", "auto"))
	for _, c := range t.Detected {
		if !c.Supported || c.WebUI == "" || (want != "auto" && want != strings.ToLower(c.Name)) {
			continue
		}
		switch c.Name {
		case "qBittorrent":
			user, _ := a.Secrets.Get("QBITTORRENT_USER")
			pass, _ := a.Secrets.Get("QBITTORRENT_PASS")
			t.Client = &anime.Qbit{Base: c.WebUI, User: user, Pass: pass, Category: label, SavePath: downloads}
		case "Transmission":
			user, _ := a.Secrets.Get("TRANSMISSION_USER")
			pass, _ := a.Secrets.Get("TRANSMISSION_PASS")
			t.Client = &anime.Transmission{Base: c.WebUI, User: user, Pass: pass, Label: label, SavePath: downloads}
		}
		break
	}
	return t, nil
}

// AnimeScanDirs are the folders searched for anime the user already has: Videos, Downloads and
// Desktop, library_dirs, the folders granted in approvals, and every external drive plugged in.
// Missing folders are skipped. They are only read.
func (a *App) AnimeScanDirs(ctx context.Context) []string {
	res := a.resolver()
	var out []string
	add := func(p string) {
		if exp, err := res.Expand(p); err == nil {
			for _, o := range out {
				if o == exp {
					return
				}
			}
			out = append(out, exp)
		}
	}
	for _, p := range []string{"$VIDEOS", "$DOWNLOADS", "$DESKTOP"} {
		add(p)
	}
	for _, p := range a.animeList("library_dirs", nil) {
		add(p)
	}
	granted, _ := (&anime.State{DB: a.DB}).Granted(ctx)
	for _, g := range granted {
		add(g)
	}
	if vols, err := platform.ListVolumes(); err == nil {
		for _, v := range vols {
			if v.Removable && v.Mount != "" {
				add(v.Mount)
			}
		}
	}
	return out
}

// sanitizeName makes a title safe as a file or folder name on every OS and on exFAT/NTFS drives:
// no separators or characters Windows refuses, no control characters, no leading dot (".hack//SIGN"
// would be a hidden folder), no trailing dot or space, a Windows device name gets an underscore,
// and at most 200 bytes (file systems stop at 255 with the episode part still to come).
func sanitizeName(s, _ string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	r := strings.NewReplacer(":", " -", "/", "-", `\`, "-", "?", "", "*", "", `"`, "'", "<", "", ">", "", "|", "-")
	s = strings.Join(strings.Fields(r.Replace(s)), " ")
	s = strings.Trim(s, ". ")
	if len(s) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.Trim(s[:cut], ". ")
	}
	stem, _, _ := strings.Cut(s, ".")
	switch strings.ToUpper(strings.TrimSpace(stem)) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s += "_"
	}
	if s == "" {
		s = "_"
	}
	return s
}

// portable stores a folder on an external drive as volume:<label>/rest, so it still resolves when
// the drive mounts elsewhere (/Volumes/RED 1, another drive letter). The label is bound to the
// drive's UUID on first use.
func (a *App) portable(abs string) string {
	vols, err := platform.ListVolumes()
	if err != nil {
		return abs
	}
	for _, v := range vols {
		if v.Label == "" || v.Mount == "" {
			continue
		}
		rel, err := filepath.Rel(v.Mount, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if _, err := a.Volumes().Mount(context.Background(), v.Label, ""); err != nil {
			return abs
		}
		if rel == "." {
			return "volume:" + v.Label
		}
		return "volume:" + v.Label + "/" + filepath.ToSlash(rel)
	}
	return abs
}

// animeFiler is the broker for the tracker, with scopes computed per call: write into the folders
// granted in approvals so far, read and trash in the download folder (SKILL.md permissions).
type animeFiler struct {
	a         *App
	state     *anime.State
	downloads string
	runID     string
}

func (f *animeFiler) broker(ctx context.Context) (*perms.Broker, error) {
	decls := []string{"write:" + f.downloads}
	granted, err := f.state.Granted(ctx)
	if err != nil {
		return nil, err
	}
	res := f.a.resolver()
	for _, g := range granted {
		exp, err := res.Expand(g)
		if err != nil {
			continue // drive not plugged in: nothing may be written there
		}
		if c, err := perms.ValidateApproved(exp, f.a.Dirs.Own()); err == nil {
			decls = append(decls, "write:"+c)
		}
	}
	scopes, err := res.Compile(decls)
	if err != nil {
		return nil, err
	}
	if f.runID == "" {
		f.runID = "anime-" + ids.New()
	}
	return &perms.Broker{RunID: f.runID, Perms: perms.Set{Files: scopes}, Trash: f.a.Trash, Undo: f.a.DB}, nil
}

// UndoID is the id `noskill undo` takes to revert the files this filer changed ("" before any).
func (f *animeFiler) UndoID() string { return f.runID }

func (f *animeFiler) Mkdir(ctx context.Context, p string) error {
	b, err := f.broker(ctx)
	if err != nil {
		return err
	}
	return b.Mkdir(ctx, p)
}

func (f *animeFiler) Rename(ctx context.Context, from, to string) error {
	b, err := f.broker(ctx)
	if err != nil {
		return err
	}
	return b.Move(ctx, from, to)
}

func (f *animeFiler) Copy(ctx context.Context, from, to string) error {
	b, err := f.broker(ctx)
	if err != nil {
		return err
	}
	return b.Copy(ctx, from, to)
}

func (f *animeFiler) TrashPath(ctx context.Context, p string) error {
	b, err := f.broker(ctx)
	if err != nil {
		return err
	}
	return b.TrashPath(ctx, p)
}

// The anime tools' service (internal/tools/builtin/anime.go): a model starts a run and reads its
// report; the steps run in Go.

func (s *runServices) AnimeRun(ctx context.Context, stage string, dryRun bool) (string, error) {
	if stage == "" {
		stage = "all"
	}
	t, err := s.a.AnimeTracker(ctx, dryRun)
	if err != nil {
		return "", err
	}
	rep, err := t.Run(ctx, stage)
	if err != nil {
		return "", err
	}
	return rep.String(), nil
}

func (s *runServices) AnimeStatus(ctx context.Context) (string, error) {
	titles, err := (&anime.State{DB: s.a.DB}).Titles(ctx)
	if err != nil {
		return "", err
	}
	type row struct {
		Title      string `json:"title"`
		Status     string `json:"status"`
		Resolution string `json:"resolution,omitempty"`
		Filed      int    `json:"episodes_filed"`
		Folder     string `json:"folder,omitempty"`
	}
	out := make([]row, 0, len(titles))
	for _, t := range titles {
		out = append(out, row{t.Title, t.Status, t.Resolution, len(t.Filed), t.Dest})
	}
	b, err := json.Marshal(out)
	return string(b), err
}

func (s *runServices) AnimeDetect(ctx context.Context) (string, error) {
	b, err := json.Marshal(s.a.AnimeDetect(ctx))
	return string(b), err
}

// Waiting for the filing lock: a torrent app can report several downloads finished at once, and a
// scheduled run may be filing too. The hook waits its turn rather than leaving the file for later.
var (
	hookTries = 9
	hookWait  = 20 * time.Second
)

// AnimeFileFinished files the finished torrent with this hash, as the torrent app's "run when a
// download finishes" hook asks (`noskill hook`). Only noskill's own torrents are touched; any other
// hash is reported and left alone. It waits for the filing lock for a few minutes.
func (a *App) AnimeFileFinished(ctx context.Context, hash string, dryRun bool) (anime.Report, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return anime.Report{}, errors.New("no torrent hash given")
	}
	t, err := a.AnimeTracker(ctx, dryRun)
	if err != nil {
		return anime.Report{}, err
	}
	t.Only = hash
	for try := 1; ; try++ {
		rep, err := t.Run(ctx, "file")
		if err != nil || !slices.Contains(rep.Skipped, anime.FilingBusy) || try >= hookTries {
			return rep, err
		}
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(hookWait):
		}
	}
}
