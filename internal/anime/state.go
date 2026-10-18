package anime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"noskill/internal/store"
)

// SkillName is the skill whose skill_kv rows hold the tracker's state (DATA_MODEL.md).
const SkillName = "anime-tracker"

// Title statuses (SKILL.md "State").
const (
	Pending       = "pending"  // an approval is waiting
	Approved      = "approved" // rule set; episodes are downloaded and filed
	Skipped       = "skipped"  // the user said no
	NotNow        = "not_now"  // the approval timed out; offered again up to approval_retries times
	Finished      = "finished" // aired out and every episode filed; its rule is removed
	NotInterested = "not_interested"
)

// TitleState is one AniList entry the tracker has offered or follows.
type TitleState struct {
	ID         int                 `json:"id"`
	Title      string              `json:"title"`
	Aliases    []string            `json:"aliases"`
	Season     int                 `json:"season,omitempty"` // 0/1 = first season
	Episodes   int                 `json:"episodes,omitempty"`
	Status     string              `json:"status"`
	ApprovalID string              `json:"approval_id,omitempty"`
	Offered    int                 `json:"offered,omitempty"`
	Dest       string              `json:"destination,omitempty"` // portable: volume:RED/ANIME2026 or an absolute path
	Resolution string              `json:"resolution,omitempty"`  // what the rule downloads
	Rule       string              `json:"rule_name,omitempty"`
	Excluded   []float64           `json:"excluded_episodes,omitempty"` // fell back to another resolution: the rule skips them
	Fallback   map[string]string   `json:"fallback,omitempty"`          // episode → resolution added by hand
	Filed      map[string]FiledEpi `json:"filed,omitempty"`             // episode key ("3", "SP1") → file
	Why        string              `json:"why,omitempty"`
	ApprovedAt time.Time           `json:"approved_at,omitzero"`
	Year       int                 `json:"year,omitempty"`
	Format     string              `json:"format,omitempty"`
	AiringDone bool                `json:"airing_done,omitempty"`
	Follow     *Follow             `json:"follow,omitempty"`      // set for a show followed from config (follow.go)
	FollowFrom int                 `json:"follow_from,omitempty"` // a followed show: episodes up to this one are the user's already
	Batch      *BatchOffer         `json:"batch,omitempty"`       // an earlier season offered as a complete 480p batch (batch.go)
}

// FiledEpi is an episode already in the user's folder.
type FiledEpi struct {
	Path       string    `json:"path"`
	Version    int       `json:"version,omitempty"`
	Resolution string    `json:"resolution,omitempty"`
	At         time.Time `json:"at"`
}

// State reads and writes the tracker's skill_kv rows.
type State struct {
	DB  *store.DB
	Now func() time.Time
}

func (s *State) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *State) get(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM skill_kv WHERE skill = ? AND key = ?`, SkillName, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *State) set(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO skill_kv (skill, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(skill, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SkillName, key, string(b), s.now().UnixMilli())
	return err
}

func titleKey(id int) string { return "titles." + strconv.Itoa(id) }

func (s *State) Title(ctx context.Context, id int) (TitleState, bool, error) {
	var t TitleState
	ok, err := s.get(ctx, titleKey(id), &t)
	return t, ok, err
}

func (s *State) PutTitle(ctx context.Context, t TitleState) error {
	return s.set(ctx, titleKey(t.ID), t)
}

// Titles returns every title the tracker knows, by id.
func (s *State) Titles(ctx context.Context) ([]TitleState, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT value FROM skill_kv WHERE skill = ? AND key LIKE 'titles.%' ORDER BY key`, SkillName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TitleState
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var t TitleState
		if json.Unmarshal([]byte(raw), &t) == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// LastDestination is the folder the user picked most recently: the next approval's prefill.
func (s *State) LastDestination(ctx context.Context) string {
	var d string
	s.get(ctx, "last_destination", &d)
	return d
}

func (s *State) SetLastDestination(ctx context.Context, d string) error {
	return s.set(ctx, "last_destination", d)
}

// ApprovedKey is the skill_kv key holding the folders granted through approvals' "dest" field:
// `write:${approved.dest}` in the skill's permissions resolves to these (SECURITY.md).
const ApprovedKey = "approved.dest"

// Grant records a folder the user picked in an approval.
func (s *State) Grant(ctx context.Context, portable string) error {
	var have []string
	if _, err := s.get(ctx, ApprovedKey, &have); err != nil {
		return err
	}
	for _, h := range have {
		if strings.EqualFold(h, portable) {
			return nil
		}
	}
	return s.set(ctx, ApprovedKey, append(have, portable))
}

// Granted returns the folders granted through approvals.
func (s *State) Granted(ctx context.Context) ([]string, error) {
	var have []string
	_, err := s.get(ctx, ApprovedKey, &have)
	return have, err
}

// LibraryMatch caches which AniList entry a folder or rule name in the user's library is (0 = none).
func (s *State) LibraryMatch(ctx context.Context, name string) (int, bool) {
	var id int
	ok, _ := s.get(ctx, "library."+Normalize(name), &id)
	return id, ok
}

func (s *State) SetLibraryMatch(ctx context.Context, name string, id int) error {
	return s.set(ctx, "library."+Normalize(name), id)
}

// LibraryIDs are the AniList ids of shows found in the user's folders (`noskill anime scan`):
// they shape the taste profile.
func (s *State) LibraryIDs(ctx context.Context) []int {
	rows, err := s.DB.QueryContext(ctx, `SELECT value FROM skill_kv WHERE skill = ? AND key LIKE 'library.%'`, SkillName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int
	seen := map[int]bool{}
	for rows.Next() {
		var raw string
		var id int
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &id) == nil && id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Ignored marks a torrent the tracker couldn't place, so it's reported once rather than every run.
func (s *State) Ignored(ctx context.Context, hash string) bool {
	var why string
	ok, _ := s.get(ctx, "ignored."+strings.ToLower(hash), &why)
	return ok
}

func (s *State) Ignore(ctx context.Context, hash, why string) error {
	return s.set(ctx, "ignored."+strings.ToLower(hash), why)
}

// Lock takes the filing lock for ttl: qBittorrent can start `noskill hook qbittorrent-finished` for
// several torrents at once, and a scheduled run may be filing too. Files are moved one at a time.
func (s *State) Lock(ctx context.Context, holder string, ttl time.Duration) (bool, error) {
	now := s.now()
	b, _ := json.Marshal(holder)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO skill_kv (skill, key, value, updated_at) VALUES (?, 'filing_lock', ?, ?)
		ON CONFLICT(skill, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		WHERE skill_kv.updated_at < ? OR skill_kv.value = excluded.value`,
		SkillName, string(b), now.UnixMilli(), now.Add(-ttl).UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *State) Unlock(ctx context.Context, holder string) {
	b, _ := json.Marshal(holder)
	s.DB.ExecContext(ctx, `DELETE FROM skill_kv WHERE skill = ? AND key = 'filing_lock' AND value = ?`, SkillName, string(b))
}
