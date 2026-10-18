package anime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"noskill/internal/approvals"
	"noskill/internal/ids"
)

// The download plan: what the tracker asks the user about before it downloads (ask_before_download),
// and the files the user adds by hand from the status page or `noskill anime add`. Each is one
// skill_kv row, "plan.<id>", so the state needs no migration.

// Plan statuses.
const (
	PlanAsked    = "asked"    // an approval is waiting
	PlanApproved = "approved" // the user said yes; the next download stage adds it
	PlanSkipped  = "skipped"  // the user said no
	PlanAdded    = "added"    // handed to the torrent app
	PlanFiled    = "filed"    // stored in its folder
	PlanFailed   = "failed"
)

// PlanItem is one download the user is asked about, or added by hand.
type PlanItem struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`  // what makes two of them the same download
	Kind       string    `json:"kind"` // release | manual
	Title      string    `json:"title"`
	Episode    string    `json:"episode,omitempty"`
	Name       string    `json:"name,omitempty"` // the name a manual download is filed under
	Size       int64     `json:"size,omitempty"`
	SizeText   string    `json:"size_text,omitempty"`   // as the feed says it, when there is no byte count
	URL        string    `json:"url,omitempty"`         // magnet, .torrent or page link (manual)
	Dest       string    `json:"destination,omitempty"` // portable folder (manual)
	Status     string    `json:"status"`
	ApprovalID string    `json:"approval_id,omitempty"`
	Hash       string    `json:"hash,omitempty"` // the torrent, once added
	Path       string    `json:"path,omitempty"` // where a manual download was filed
	Note       string    `json:"note,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

func planKey(id string) string { return "plan." + id }

// PutPlan saves an item (a new one gets an id).
func (s *State) PutPlan(ctx context.Context, p PlanItem) (PlanItem, error) {
	if p.ID == "" {
		p.ID = ids.New()
	}
	if p.Created.IsZero() {
		p.Created = s.now()
	}
	p.Updated = s.now()
	return p, s.set(ctx, planKey(p.ID), p)
}

// Plans returns every item, newest first.
func (s *State) Plans(ctx context.Context) ([]PlanItem, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT value FROM skill_kv WHERE skill = ? AND key LIKE 'plan.%'`, SkillName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanItem
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p PlanItem
		if json.Unmarshal([]byte(raw), &p) == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, rows.Err()
}

// PlanByKey finds the item for a key.
func (s *State) PlanByKey(ctx context.Context, key string) (PlanItem, bool, error) {
	ps, err := s.Plans(ctx)
	if err != nil {
		return PlanItem{}, false, err
	}
	for _, p := range ps {
		if p.Key == key {
			return p, true, nil
		}
	}
	return PlanItem{}, false, nil
}

// DeletePlan forgets an item (an expired question is asked again on the next run).
func (s *State) DeletePlan(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM skill_kv WHERE skill = ? AND key = ?`, SkillName, planKey(id))
	return err
}

// setPlanStatus moves the item with this key to a new status; it does nothing when there is none.
func (s *State) setPlanStatus(ctx context.Context, key, status string) error {
	p, ok, err := s.PlanByKey(ctx, key)
	if err != nil || !ok {
		return err
	}
	p.Status = status
	_, err = s.PutPlan(ctx, p)
	return err
}

// ReleaseKey is the plan key of a release of a title's episode.
func ReleaseKey(title, episode string) string { return "rel|" + title + "|" + episode }

// ask_before_download values.
const (
	AskNever  = "false"  // releases of approved shows are added as they appear
	AskAll    = "true"   // every release of an approved show is asked about first
	AskUnsure = "unsure" // only when the tracker isn't sure the release is the show's (the default)
)

// ParseAskMode reads the setting: true/all, false/never/off, or unsure; anything else is unsure.
func ParseAskMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "all", "yes", "on", "always":
		return AskAll
	case "false", "never", "no", "off":
		return AskNever
	}
	return AskUnsure
}

// ask decides whether a release may be added now. With ask_before_download = "false" it always
// may. Otherwise the first time a release is found that needs a question (every release for "true";
// for "unsure" only one whose name isn't exactly the show's, conf < 1) it becomes an approval, which
// the status page, the tray, the CLI and notifications all show, and it is added on the run after
// the user says yes. A no is remembered, so it isn't asked again.
func (t *Tracker) ask(ctx context.Context, rep *Report, conf float64, p PlanItem) (bool, error) {
	switch ParseAskMode(t.Settings.AskMode) {
	case AskNever:
		return true, nil
	case AskUnsure:
		if conf >= 1 {
			return true, nil
		}
		p.Note = fmt.Sprintf("not sure this is the right show: the release is named %q", p.Name)
	}
	label := strings.TrimSpace(p.Title + " " + p.Episode)
	cur, found, err := t.State.PlanByKey(ctx, p.Key)
	if err != nil {
		return false, err
	}
	if !found {
		if t.DryRun {
			rep.Planned = append(rep.Planned, label+" (dry run: it would ask first)")
			return false, nil
		}
		summary := fmt.Sprintf("Download %s", label)
		switch {
		case p.Size > 0:
			summary += fmt.Sprintf(" (%s)", humanBytes(uint64(p.Size)))
		case p.SizeText != "":
			summary += fmt.Sprintf(" (%s)", p.SizeText)
		}
		if p.Note != "" {
			summary = fmt.Sprintf("%s? Not sure it's the right show: the release is named %q", summary, p.Name)
		} else {
			summary += "?"
		}
		it, err := t.Ask.Create(ctx, approvals.New{Source: "skill", Kind: "download", Summary: summary,
			Details: map[string]any{"title": p.Title, "episode": p.Episode, "name": p.Name, "size": firstNonEmpty(p.SizeText, humanBytesIf(p.Size)), "why": p.Note},
			Timeout: 7 * 24 * time.Hour})
		if err != nil {
			return false, err
		}
		p.Status, p.ApprovalID = PlanAsked, it.ID
		if _, err := t.State.PutPlan(ctx, p); err != nil {
			return false, err
		}
		rep.Planned = append(rep.Planned, label)
		return false, nil
	}
	switch cur.Status {
	case PlanApproved, PlanFailed:
		return true, nil
	case PlanAsked:
		it, err := t.Ask.Get(ctx, cur.ApprovalID)
		if errors.Is(err, approvals.ErrNotFound) {
			it.Status = approvals.Expired
		} else if err != nil {
			return false, err
		}
		switch it.Status {
		case approvals.Approved:
			if !t.DryRun {
				cur.Status = PlanApproved
				if _, err := t.State.PutPlan(ctx, cur); err != nil {
					return false, err
				}
			}
			return true, nil
		case approvals.Skipped:
			if !t.DryRun {
				cur.Status = PlanSkipped
				if _, err := t.State.PutPlan(ctx, cur); err != nil {
					return false, err
				}
			}
			rep.Declined = append(rep.Declined, label)
		case approvals.Expired:
			if !t.DryRun {
				if err := t.State.DeletePlan(ctx, cur.ID); err != nil {
					return false, err
				}
			}
			// Nobody answered in time: ask again now, so the question is always on the page.
			if !t.DryRun {
				return t.ask(ctx, rep, conf, p)
			}
		default:
			rep.Planned = append(rep.Planned, label+" (waiting for you)")
		}
	}
	return false, nil
}

// planAdded records that a release the user approved went to the torrent app.
func (t *Tracker) planAdded(ctx context.Context, key string) {
	if ParseAskMode(t.Settings.AskMode) != AskNever && !t.DryRun {
		t.State.setPlanStatus(ctx, key, PlanAdded)
	}
}

var (
	btihRe     = regexp.MustCompile(`(?i)xt=urn:btih:([0-9a-f]{40}|[a-z2-7]{32})`)
	torrentExt = regexp.MustCompile(`(?i)\.torrent$`)
)

// manualLink says what the user pasted: a magnet link, a .torrent link, or a Nyaa page (whose
// magnet link is read from the page). Anything else is refused, so the torrent app is never sent
// to a file: URL or a plain web page.
func (t *Tracker) manualLink(ctx context.Context, raw string) (link, title string, err error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return "", "", errors.New("paste a magnet link, a link to a .torrent file, or a Nyaa page")
	}
	switch {
	case u.Scheme == "magnet":
		if !btihRe.MatchString(raw) {
			return "", "", errors.New("that magnet link has no torrent hash in it")
		}
		return raw, u.Query().Get("dn"), nil
	case u.Scheme == "https" && torrentExt.MatchString(u.Path):
		return raw, strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path)), nil
	case u.Scheme == "https" && t.Nyaa != nil && viewIDRe.MatchString(u.Path):
		it := NyaaItem{Page: raw, Title: raw}
		if err := t.Nyaa.Resolve(ctx, &it); err != nil {
			return "", "", err
		}
		return it.TorrentURL, "", nil
	}
	return "", "", errors.New("paste a magnet link, a link to a .torrent file (https), or a Nyaa page")
}

// AddManual adds a download the user chose: the link, the folder it is filed in, and optionally the
// name it is filed under. The torrent goes to the torrent app with noskill's label, so the file
// stage stops it seeding when it finishes and files it in the folder.
func (t *Tracker) AddManual(ctx context.Context, rawURL, dest, name string) (PlanItem, error) {
	if t.Client == nil {
		return PlanItem{}, errors.New("no usable torrent app: " + Describe(t.Detected) + " (check the anime-tracker setting `client`)")
	}
	link, title, err := t.manualLink(ctx, rawURL)
	if err != nil {
		return PlanItem{}, err
	}
	if strings.TrimSpace(dest) == "" {
		dest = t.Settings.DefaultDest
	}
	abs, err := t.resolve(dest)
	if err != nil {
		return PlanItem{}, fmt.Errorf("folder %q: %w", dest, err)
	}
	portable := abs
	if t.Portable != nil {
		portable = t.Portable(abs)
	}
	name = strings.TrimSpace(name)
	if name != "" && t.Settings.Naming.Sanitize != nil {
		name = t.Settings.Naming.Sanitize(name, t.Settings.Naming.FS)
	}
	key := "man|" + link
	if m := btihRe.FindStringSubmatch(link); m != nil {
		key = "man|btih:" + strings.ToLower(m[1]) // the same torrent whatever else the link says
	}
	p := PlanItem{Kind: "manual", Key: key, Title: firstNonEmpty(name, title, "download"), Name: name, URL: link, Dest: portable}
	if _, dup, err := t.State.PlanByKey(ctx, p.Key); err != nil {
		return PlanItem{}, err
	} else if dup {
		return PlanItem{}, errors.New("that download was already added")
	}
	if t.DryRun {
		p.Status = "dry run"
		return p, nil
	}
	if err := t.Client.Ensure(ctx); err != nil {
		return PlanItem{}, err
	}
	before, err := t.Client.All(ctx)
	if err != nil {
		return PlanItem{}, err
	}
	if err := t.State.Grant(ctx, portable); err != nil {
		return PlanItem{}, err
	}
	if err := t.Client.AddTorrent(ctx, link); err != nil {
		return PlanItem{}, err
	}
	p.Status = PlanAdded
	if m := btihRe.FindStringSubmatch(link); m != nil && len(m[1]) == 40 {
		p.Hash = strings.ToLower(m[1])
	} else {
		p.Hash = t.newTorrent(ctx, before)
	}
	return t.State.PutPlan(ctx, p)
}

// newTorrent finds the torrent that appeared since before (a .torrent link doesn't say its hash).
func (t *Tracker) newTorrent(ctx context.Context, before []Torrent) string {
	known := map[string]bool{}
	for _, b := range before {
		known[strings.ToLower(b.Hash)] = true
	}
	for i := 0; i < 6; i++ {
		if now, err := t.Client.All(ctx); err == nil {
			for _, n := range now {
				if !known[strings.ToLower(n.Hash)] {
					return strings.ToLower(n.Hash)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(500 * time.Millisecond):
		}
	}
	return ""
}

func humanBytesIf(n int64) string {
	if n <= 0 {
		return ""
	}
	return humanBytes(uint64(n))
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// manualFor finds the added-by-hand download a finished torrent belongs to.
func manualFor(plans []PlanItem, hash string) (PlanItem, bool) {
	for _, p := range plans {
		if p.Kind == "manual" && p.Status == PlanAdded && p.Hash != "" && strings.EqualFold(p.Hash, hash) {
			return p, true
		}
	}
	return PlanItem{}, false
}

// fileManual files a finished download the user added: every finished file, under its folder, in
// the folder the user chose. It never overwrites and never leaves the folder. A single file takes
// the name the user gave; a torrent of several keeps its own folder and names. It returns the files
// stored, and whether the download may be removed (everything was stored).
func (t *Tracker) fileManual(ctx context.Context, tor Torrent, item PlanItem, rep *Report) (placed []string, all bool) {
	dest, err := t.resolve(item.Dest)
	if err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v (plug the drive in; it's filed on the next run)", item.Title, err))
		return nil, false
	}
	files, err := t.Client.Files(ctx, tor.Hash)
	if err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", item.Title, err))
		return nil, false
	}
	var done []TorrentFile
	for _, f := range files {
		if f.Progress >= 1 {
			done = append(done, f)
		}
	}
	if len(done) == 0 || len(done) < len(files) {
		return nil, false
	}
	all = true
	var firstTarget string
	for _, f := range done {
		rel := filepath.FromSlash(f.Name)
		target := filepath.Join(dest, rel)
		if len(done) == 1 {
			base := filepath.Base(rel)
			if item.Name != "" {
				base = item.Name + filepath.Ext(rel)
			}
			target = filepath.Join(dest, base)
		}
		if !strictlyInside(dest, target) {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %s would leave the folder, so it wasn't filed", item.Title, f.Name))
			all = false
			continue
		}
		src := filepath.Join(tor.SavePath, rel)
		if t.DryRun {
			rep.Filed = append(rep.Filed, fmt.Sprintf("%s → %s (dry run)", filepath.Base(f.Name), target))
			all = false
			continue
		}
		if err := t.space(src, filepath.Dir(target)); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v (nothing was copied; it's filed on a later run)", filepath.Base(f.Name), err))
			all = false
			break
		}
		if err := t.Files.Mkdir(ctx, filepath.Dir(target)); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", filepath.Base(f.Name), err))
			all = false
			break
		}
		if err := t.Files.Copy(ctx, src, target); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %v", filepath.Base(f.Name), err))
			all = false
			continue
		}
		placed = append(placed, src)
		if firstTarget == "" {
			firstTarget = target
		}
		rep.Filed = append(rep.Filed, fmt.Sprintf("%s → %s", filepath.Base(f.Name), target))
	}
	if all && !t.DryRun {
		item.Status, item.Path = PlanFiled, firstTarget
		if len(done) > 1 {
			item.Path = dest
		}
		t.State.PutPlan(ctx, item)
	}
	return placed, all
}
