package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"noskill/internal/anime"
	"noskill/internal/app"
)

// animeAudit is `noskill anime audit`: the week run's morning check (docs/WEEK_RUN.md) as one command.
// Each run is also appended to <data>/logs/week-audit.jsonl, so the week's record is in one file.
func animeAudit(c *cmd, ctx context.Context, a *app.App, args []string) int {
	fs := flag.NewFlagSet("anime audit", flag.ContinueOnError)
	since := fs.String("since", "24h", "how far back to look (24h, 3d)")
	job := fs.String("job", "anime-nightly", "the scheduled job to check")
	if rest, err := c.parse(fs, args); err != nil || len(rest) != 0 {
		return c.usageErr("usage: noskill anime audit [--since 24h] [--job anime-nightly]")
	}
	window, err := parseAge(*since)
	if err != nil {
		return c.usageErr("--since: %v", err)
	}
	r, err := a.WeekAudit(ctx, window, *job)
	if err != nil {
		return c.fail(err)
	}
	logPath := filepath.Join(a.Dirs.Logs(), "week-audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err == nil {
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(r)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if c.g.json {
		c.printJSON(r)
	} else {
		mark := map[string]string{"ok": "✓", "warn": "!", "failed": "✗"}
		for _, ck := range r.Checks {
			c.outf("%s %-18s %s\n", mark[ck.Status], ck.Name, ck.Detail)
			for _, it := range ck.Items {
				c.outf("    %s\n", it)
			}
		}
		if r.Pass {
			c.outf("PASS: the last %s were clean (%d file(s) filed). Logged in %s\n", *since, r.Filed, logPath)
		} else {
			c.outf("FAIL: see the ✗ lines. Follow the stop and fail rules in docs/WEEK_RUN.md. Logged in %s\n", logPath)
		}
	}
	if !r.Pass {
		return ExitError
	}
	return ExitOK
}

// cmdAnime runs the anime tracker (skills/anime-tracker/SKILL.md) without a model: `detect` lists
// the torrent apps found, `run` does one pass of its stages, `status` lists the titles it knows.
func cmdAnime(c *cmd, args []string) int {
	usage := "usage: noskill anime detect | scan [--offline] [folder…] | run [--stage answers|suggest|download|file] [--dry-run] | status | add <link> [--to folder] [--name n] | file-now | audit [--since 24h] [--job anime-nightly]"
	if len(args) == 0 {
		return c.usageErr(usage)
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	switch args[0] {
	case "detect":
		found := a.AnimeDetect(ctx)
		if c.g.json {
			c.printJSON(found)
			return ExitOK
		}
		c.outf("%s\n", anime.Describe(found))
		return ExitOK
	case "run":
		fs := flag.NewFlagSet("anime run", flag.ContinueOnError)
		stage := fs.String("stage", "all", "one stage: "+strings.Join(anime.Stages, ", "))
		dry := fs.Bool("dry-run", false, "say what would happen; change nothing")
		if _, err := c.parse(fs, args[1:]); err != nil {
			return ExitUsage
		}
		t, err := a.AnimeTracker(ctx, *dry)
		if err != nil {
			return c.fail(err)
		}
		rep, err := t.Run(ctx, *stage)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(rep)
			return ExitOK
		}
		c.outf("%s\n", rep)
		return ExitOK
	case "scan":
		fs := flag.NewFlagSet("anime scan", flag.ContinueOnError)
		offline := fs.Bool("offline", false, "don't check titles on AniList: list every show with numbered episodes")
		dirs, err := c.parse(fs, args[1:])
		if err != nil {
			return ExitUsage
		}
		if len(dirs) == 0 {
			dirs = a.AnimeScanDirs(ctx)
		}
		c.infof("scanning %s\n", strings.Join(dirs, ", "))
		shows := anime.ScanLibrary(dirs, 0).Shows()
		unchecked := 0
		if !*offline {
			t, err := a.AnimeTracker(ctx, false)
			if err != nil {
				return c.fail(err)
			}
			shows, unchecked = t.Confirm(ctx, shows, 60)
		}
		if c.g.json {
			c.printJSON(map[string]any{"folders": dirs, "shows": shows, "unchecked": unchecked})
			return ExitOK
		}
		if len(shows) == 0 {
			c.outf("No anime found in %s.\n", strings.Join(dirs, ", "))
		} else {
			rows := [][]string{{"SHOW", "SEASON", "EPISODES", "FOLDER"}}
			for _, sh := range shows {
				name := sh.Title
				if sh.Name != "" && !strings.EqualFold(sh.Name, sh.Title) {
					name += " (" + sh.Name + ")"
				}
				season := "-"
				if sh.Season > 0 {
					season = fmt.Sprint(sh.Season)
				}
				folder := sh.Folders[0]
				if len(sh.Folders) > 1 {
					folder += fmt.Sprintf(" +%d", len(sh.Folders)-1)
				}
				rows = append(rows, []string{name, season, episodeRange(sh.Episodes), folder})
			}
			c.table(rows)
		}
		if unchecked > 0 {
			c.outf("%d more titles weren't checked on AniList this time; run it again to check them.\n", unchecked)
		}
		return ExitOK
	case "audit":
		return animeAudit(c, ctx, a, args[1:])
	case "add":
		fs := flag.NewFlagSet("anime add", flag.ContinueOnError)
		to := fs.String("to", "", "the folder it is filed in when it finishes (default: the tracker's default folder)")
		name := fs.String("name", "", "the file name to keep (a single file only)")
		rest, err := c.parse(fs, args[1:])
		if err != nil || len(rest) != 1 {
			return c.usageErr("usage: noskill anime add <magnet link | https .torrent link | Nyaa page> [--to folder] [--name n]")
		}
		item, err := a.AnimeAddDownload(ctx, rest[0], *to, *name)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(item)
			return ExitOK
		}
		c.outf("Added %q: it is filed in %s when it finishes, and stops seeding then.\n", item.Title, item.Dest)
		return ExitOK
	case "file-now":
		rep, err := a.AnimeFileNow(ctx)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(rep)
			return ExitOK
		}
		c.outf("%s\n", rep.String())
		return ExitOK
	case "status":
		st := &anime.State{DB: a.DB}
		titles, err := st.Titles(ctx)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(titles)
			return ExitOK
		}
		if len(titles) == 0 {
			c.outf("No titles yet. `noskill anime run` suggests this season's new shows for you to approve.\n")
			return ExitOK
		}
		rows := [][]string{{"TITLE", "STATUS", "RES", "FILED", "FOLDER"}}
		sort.Slice(titles, func(i, j int) bool { return titles[i].Title < titles[j].Title })
		for _, t := range titles {
			rows = append(rows, []string{t.Title, t.Status, t.Resolution, fmt.Sprint(len(t.Filed)), t.Dest})
		}
		c.table(rows)
		return ExitOK
	}
	return c.usageErr(usage)
}

// episodeRange shows "1–12" for a run of episodes, "1–3, 5, SP1" otherwise.
func episodeRange(eps []string) string {
	var parts []string
	start, prev := -1, -1
	flush := func() {
		switch {
		case start < 0:
		case start == prev:
			parts = append(parts, fmt.Sprint(start))
		default:
			parts = append(parts, fmt.Sprintf("%d–%d", start, prev))
		}
		start = -1
	}
	for _, e := range eps {
		var n int
		if _, err := fmt.Sscanf(e, "%d", &n); err == nil && fmt.Sprint(n) == e {
			if start >= 0 && n == prev+1 {
				prev = n
				continue
			}
			flush()
			start, prev = n, n
			continue
		}
		flush()
		parts = append(parts, e)
	}
	flush()
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}
