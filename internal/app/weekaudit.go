package app

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"noskill/internal/anime"
	"noskill/internal/scheduler"
)

// The week-run audit (Phase 3 exit, docs/WEEK_RUN.md): "correct naming and zero unexpected writes"
// as one command instead of an eight-row table to check by hand. It reads what noskill recorded
// (job runs, the file-effect log) and what is on disk, and says PASS or FAIL for each check.

// AuditCheck is one line of the audit.
type AuditCheck struct {
	Name   string   `json:"check"`
	Status string   `json:"status"` // ok | warn | failed
	Detail string   `json:"detail"`
	Items  []string `json:"items,omitempty"` // the files or runs behind a warn or failed
}

// AuditReport is what `noskill anime audit` prints and logs.
type AuditReport struct {
	At     time.Time    `json:"at"`
	Since  time.Time    `json:"since"`
	Checks []AuditCheck `json:"checks"`
	Pass   bool         `json:"pass"` // no check failed
	Filed  int          `json:"files_filed"`
}

// epName is what a filed episode's name looks like in every naming style noskill writes: the title,
// the episode (S02E03, EP 3, Episode 3, SP 1, Special 1) and optionally the resolution.
var epName = regexp.MustCompile(`^.+ (?:S\d{2,}E\d+(?:\.\d+)?|EP \d+(?:\.\d+)?|Episode \d+(?:\.\d+)?|SP \d+(?:\.\d+)?|Special \d+(?:\.\d+)?)(?: \[\d{3,4}p\])?\.[A-Za-z0-9]{2,4}$`)
var movieName = regexp.MustCompile(`^.+ \(\d{4}\)\.[A-Za-z0-9]{2,4}$`)
var trailingRes = regexp.MustCompile(` \[\d{3,4}p\]\.[A-Za-z0-9]{2,4}$`)

// releaseLeftovers are marks of a release name that mean the file wasn't renamed. A title may have
// brackets of its own (some AniList titles do), so only the shapes a release name adds count.
var (
	releaseTag  = regexp.MustCompile(`(?i)\b(?:SubsPlease|Erai-raws|HorribleSubs|ToonsHub|Yameii|x264|x265|HEVC|WEB-DL|WEBRip)\b`)
	releaseHash = regexp.MustCompile(`\[[0-9A-Fa-f]{8}\]`)
	releaseRes  = regexp.MustCompile(`\b(?:2160|1080|720|576|480)p\b`)
	groupPrefix = regexp.MustCompile(`^\[[^\]]+\]`)
)

// CheckEpisodeName says what is wrong with a filed file's name, or "" when it's right.
func CheckEpisodeName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".!qb") || strings.HasSuffix(lower, ".crdownload"):
		return "unfinished download"
	case groupPrefix.MatchString(name):
		return "release-group prefix " + groupPrefix.FindString(name) + " still in the name"
	}
	if m := releaseTag.FindString(name); m != "" {
		return "release tag " + m + " still in the name"
	}
	if m := releaseHash.FindString(name); m != "" {
		return "release hash " + m + " still in the name"
	}
	if m := releaseRes.FindString(trailingRes.ReplaceAllString(name, "."+name[strings.LastIndex(name, ".")+1:])); m != "" {
		return "resolution " + m + " outside the final [resolution]"
	}
	if !epName.MatchString(name) && !movieName.MatchString(name) {
		return "no title and episode in the expected form"
	}
	return ""
}

// WeekAudit checks the last `window` of the named job (default anime-nightly) and of the files it
// wrote. It changes nothing: undo is only planned (dry run), and the log is the caller's to write.
func (a *App) WeekAudit(ctx context.Context, window time.Duration, job string) (*AuditReport, error) {
	if job == "" {
		job = "anime-nightly"
	}
	now := time.Now()
	r := &AuditReport{At: now, Since: now.Add(-window)}
	add := func(c AuditCheck) { r.Checks = append(r.Checks, c) }

	// 1. The job ran, and nothing failed.
	st := &scheduler.Store{DB: a.DB}
	j, err := st.Get(ctx, job)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	runs, err := st.Runs(ctx, j.ID, 100)
	if err != nil {
		return nil, err
	}
	var ok, bad, other []string
	for _, jr := range runs {
		if jr.ScheduledFor.Before(r.Since) {
			continue
		}
		label := fmt.Sprintf("%s %s", jr.ScheduledFor.Local().Format("Mon 15:04"), jr.Status)
		switch jr.Status {
		case scheduler.StatusDone:
			ok = append(ok, label)
		case scheduler.StatusFailed, scheduler.StatusTimeout:
			bad = append(bad, label+": "+jr.Error)
		default:
			other = append(other, label)
		}
	}
	switch {
	case len(bad) > 0:
		add(AuditCheck{Name: "job " + job, Status: "failed", Detail: fmt.Sprintf("%d run(s) failed", len(bad)), Items: bad})
	case len(ok) == 0:
		add(AuditCheck{Name: "job " + job, Status: "failed", Detail: "no completed run in this window (a night skipped is noted; two in a row is a fail)", Items: other})
	default:
		d := fmt.Sprintf("%d run(s) done", len(ok))
		if len(other) > 0 {
			d += fmt.Sprintf(", %d not finished", len(other))
		}
		add(AuditCheck{Name: "job " + job, Status: "ok", Detail: d, Items: other})
	}

	// 2. The files noskill wrote.
	effects, err := a.DB.UndoSince(ctx, r.Since)
	if err != nil {
		return nil, err
	}
	wrote := map[string]bool{}
	var written []string
	var badNames []string
	runIDs := map[string]bool{}
	for _, e := range effects {
		runIDs[e.RunID] = true
		if e.To == "" || (e.Op != "copy" && e.Op != "create" && e.Op != "move") {
			continue
		}
		wrote[e.To] = true
		fi, err := os.Lstat(e.To)
		if err != nil || fi.IsDir() {
			continue // moved on since (an undo, a later rename) or a folder: not a name to judge
		}
		written = append(written, e.To)
		if why := CheckEpisodeName(filepath.Base(e.To)); why != "" {
			badNames = append(badNames, filepath.Base(e.To)+": "+why)
		}
	}
	r.Filed = len(written)
	if len(written) == 0 {
		add(AuditCheck{Name: "names", Status: "ok", Detail: "no file was filed in this window, so there is nothing to name"})
	} else if len(badNames) > 0 {
		add(AuditCheck{Name: "names", Status: "failed", Detail: fmt.Sprintf("%d of %d filed file(s) are named wrongly: fix the naming and restart the week", len(badNames), len(written)), Items: badNames})
	} else {
		add(AuditCheck{Name: "names", Status: "ok", Detail: fmt.Sprintf("%d filed file(s), all named title + episode (+ resolution), no release leftovers", len(written))})
	}

	// 3. Nothing unexpected: files that appeared in the folders noskill files into, other than the ones it recorded.
	roots := a.auditRoots(ctx, wrote)
	var stray []string
	scanned := 0
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if scanned++; scanned > 200000 {
				return filepath.SkipAll
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if n := d.Name(); n == ".DS_Store" || strings.HasPrefix(n, "._") || n == "Thumbs.db" || n == "desktop.ini" {
				return nil
			}
			if fi, err := d.Info(); err == nil && !fi.ModTime().Before(r.Since) && !wrote[p] {
				stray = append(stray, p)
			}
			return nil
		})
	}
	sort.Strings(stray)
	switch {
	case len(roots) == 0:
		add(AuditCheck{Name: "unexpected writes", Status: "warn", Detail: "no destination folder to look in (no title has one yet)"})
	case len(stray) > 0:
		add(AuditCheck{Name: "unexpected writes", Status: "failed", Detail: fmt.Sprintf("%d file(s) changed in noskill's destination folders that noskill didn't record: stop the job (`noskill schedule pause %s`) and look", len(stray), job), Items: first(stray, 15)})
	default:
		add(AuditCheck{Name: "unexpected writes", Status: "ok", Detail: fmt.Sprintf("%d destination folder(s) looked at: every file changed in the window is one noskill recorded", len(roots))})
	}

	// 4. Undo would work for every run that wrote files.
	var ids []string
	for id := range runIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var cannot []string
	for _, id := range ids {
		steps, err := a.Undo(ctx, id, true)
		if err != nil {
			cannot = append(cannot, id+": "+err.Error())
			continue
		}
		for _, s := range steps {
			if s.Note != "" {
				cannot = append(cannot, fmt.Sprintf("%s step %d (%s): %s", shortRun(id), s.Seq, s.Op, s.Note))
			}
		}
	}
	switch {
	case len(ids) == 0:
		add(AuditCheck{Name: "undo", Status: "ok", Detail: "no run wrote files in this window"})
	case len(cannot) > 0:
		// A dry run checks each step against the disk as it is now, so a step that depends on an earlier
		// one in the same run (a file moved, then copied, then trashed) shows a note that isn't a problem.
		add(AuditCheck{Name: "undo", Status: "warn", Detail: fmt.Sprintf("%d step(s) of %d run(s) show a note in a dry run (files already gone or moved on are normal for old runs); check a recent run with `noskill undo <run> --dry-run`", len(cannot), len(ids)), Items: first(cannot, 5)})
	default:
		add(AuditCheck{Name: "undo", Status: "ok", Detail: fmt.Sprintf("`noskill undo --dry-run` plans every step of %d run(s)", len(ids))})
	}

	r.Pass = true
	for _, c := range r.Checks {
		if c.Status == "failed" {
			r.Pass = false
		}
	}
	return r, nil
}

// auditRoots are the folders to look for strays in: each title's destination and the folder of every
// file noskill just wrote.
func (a *App) auditRoots(ctx context.Context, wrote map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	titles, _ := (&anime.State{DB: a.DB}).Titles(ctx)
	res := a.resolver()
	// The download folder is the torrent app's: it writes there all day, which isn't noskill's doing.
	downloads, _ := res.Expand(a.animeString("download_dir", "$DOWNLOADS"))
	inDownloads := func(p string) bool {
		return downloads != "" && (p == downloads || strings.HasPrefix(p, downloads+string(filepath.Separator)))
	}
	for _, t := range titles {
		if t.Dest == "" || t.Status != "approved" {
			continue
		}
		if p, err := res.Expand(t.Dest); err == nil && !inDownloads(p) {
			add(p)
		}
	}
	for p := range wrote {
		d := filepath.Dir(p)
		if inDownloads(d) {
			continue
		}
		// A title's own folder sits under its destination, which is already looked at.
		if !slices.ContainsFunc(out, func(r string) bool { return d == r || strings.HasPrefix(d, r+string(filepath.Separator)) }) {
			add(d)
		}
	}
	sort.Strings(out)
	return out
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(slices.Clone(s[:n]), fmt.Sprintf("… and %d more", len(s)-n))
	}
	return s
}

func shortRun(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}
