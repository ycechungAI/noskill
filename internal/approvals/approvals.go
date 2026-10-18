// Package approvals is the queue of things a human has to say yes to (FR-A6): `confirm` tools,
// learned skills on first use, anime picks. Items live in the database so the CLI, the daemon and
// the tray (separate processes) all see the same queue. The default for an unanswered item is
// always "skip": nothing is ever approved by a timeout.
package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"noskill/internal/ids"
	"noskill/internal/store"
)

type Status string

const (
	Pending  Status = "pending"
	Approved Status = "approved"
	Skipped  Status = "skipped"
	Expired  Status = "expired"
)

// Field is a value the user can change while approving, with a prefilled default: anime-tracker's
// "Save to" folder, for example. Type "folder" values are checked before they join a write scope.
type Field struct {
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Type    string `json:"type"` // string | folder | number | bool
	Prefill string `json:"prefill,omitempty"`
	Value   string `json:"value,omitempty"` // set when answered
}

type Item struct {
	ID             string          `json:"id"`
	Source         string          `json:"source"` // job | skill | learning | install | proxy
	Kind           string          `json:"kind"`
	Summary        string          `json:"summary"`
	Details        json.RawMessage `json:"details,omitempty"`
	Fields         []Field         `json:"fields,omitempty"`
	RunID          string          `json:"run_id,omitempty"`
	ConversationFP string          `json:"conversation_fp,omitempty"`
	ResumeToken    string          `json:"resume_token,omitempty"`
	Status         Status          `json:"status"`
	AnsweredVia    string          `json:"answered_via,omitempty"`
	Note           string          `json:"note,omitempty"`
	ExpiresAt      time.Time       `json:"expires_at"`
	CreatedAt      time.Time       `json:"created_at"`
	ResolvedAt     time.Time       `json:"resolved_at,omitempty"`
}

// FieldValues returns each field's answer, or its prefill when the user left it alone.
func (i Item) FieldValues() map[string]string {
	m := map[string]string{}
	for _, f := range i.Fields {
		v := f.Value
		if v == "" {
			v = f.Prefill
		}
		m[f.Name] = v
	}
	return m
}

type New struct {
	Source, Kind, Summary string
	Details               any
	Fields                []Field
	RunID                 string
	ConversationFP        string
	ResumeToken           string
	Timeout               time.Duration // 0 = the queue's default
}

var (
	ErrNotFound = errors.New("no such approval")
	ErrResolved = errors.New("this approval was already answered")
	ErrExpired  = errors.New("this approval expired and was skipped")
)

// Queue is the approvals store plus its fan-out.
type Queue struct {
	DB      *store.DB
	Default time.Duration // [approvals] default_timeout
	// Announce is called after an item is created: notification channels hook in here. Errors are
	// the channel's to report; the item exists either way and `noskill approvals` always shows it.
	Announce func(ctx context.Context, it Item)
	// Validate checks and normalises a field value before it's stored (folder fields go through
	// perms.ValidateApproved). nil accepts any value.
	Validate func(f Field, value string) (string, error)
	Now      func() time.Time

	mu   sync.Mutex
	wake map[string][]chan struct{}
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Create adds a pending item.
func (q *Queue) Create(ctx context.Context, n New) (Item, error) {
	if strings.TrimSpace(n.Summary) == "" {
		return Item{}, errors.New("approval needs a summary")
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = q.Default
	}
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	details := json.RawMessage("{}")
	if n.Details != nil {
		b, err := json.Marshal(n.Details)
		if err != nil {
			return Item{}, err
		}
		details = b
	}
	for _, f := range n.Fields {
		if f.Name == "" || !contains([]string{"string", "folder", "number", "bool"}, f.Type) {
			return Item{}, fmt.Errorf("field %q: type must be string, folder, number or bool", f.Name)
		}
	}
	now := q.now()
	it := Item{ID: ids.New(), Source: n.Source, Kind: n.Kind, Summary: n.Summary, Details: details, Fields: n.Fields,
		RunID: n.RunID, ConversationFP: n.ConversationFP, ResumeToken: n.ResumeToken,
		Status: Pending, CreatedAt: now, ExpiresAt: now.Add(timeout)}
	fields, _ := json.Marshal(it.Fields)
	if len(it.Fields) == 0 {
		fields = []byte("[]")
	}
	_, err := q.DB.ExecContext(ctx, `INSERT INTO approvals (id, source, kind, summary, details, fields, run_id, conversation_fp, resume_token, status, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		it.ID, it.Source, it.Kind, it.Summary, string(details), string(fields), nullable(it.RunID), nullable(it.ConversationFP),
		nullable(it.ResumeToken), it.ExpiresAt.UnixMilli(), it.CreatedAt.UnixMilli())
	if err != nil {
		return Item{}, err
	}
	if q.Announce != nil {
		q.Announce(ctx, it)
	}
	return it, nil
}

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

const cols = `id, source, kind, summary, details, fields, run_id, conversation_fp, resume_token, status, answered_via, note, expires_at, created_at, resolved_at`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Item, error) {
	var it Item
	var details, fields, status string
	var run, fp, tok, via, note sql.NullString
	var exp, created int64
	var resolved sql.NullInt64
	if err := r.Scan(&it.ID, &it.Source, &it.Kind, &it.Summary, &details, &fields, &run, &fp, &tok, &status, &via, &note, &exp, &created, &resolved); err != nil {
		return Item{}, err
	}
	it.Details = json.RawMessage(details)
	json.Unmarshal([]byte(fields), &it.Fields)
	it.RunID, it.ConversationFP, it.ResumeToken, it.AnsweredVia, it.Note = run.String, fp.String, tok.String, via.String, note.String
	it.Status = Status(status)
	it.ExpiresAt, it.CreatedAt = time.UnixMilli(exp), time.UnixMilli(created)
	if resolved.Valid {
		it.ResolvedAt = time.UnixMilli(resolved.Int64)
	}
	return it, nil
}

// Get returns one item, expiring it first if its time is up.
func (q *Queue) Get(ctx context.Context, id string) (Item, error) {
	if _, err := q.ExpireDue(ctx); err != nil {
		return Item{}, err
	}
	it, err := scan(q.DB.QueryRowContext(ctx, `SELECT `+cols+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return it, err
}

// List returns pending items (oldest first), or every item newest first when all is set.
func (q *Queue) List(ctx context.Context, all bool, limit int) ([]Item, error) {
	if _, err := q.ExpireDue(ctx); err != nil {
		return nil, err
	}
	query := `SELECT ` + cols + ` FROM approvals WHERE status = 'pending' ORDER BY created_at, id`
	if all {
		query = `SELECT ` + cols + ` FROM approvals ORDER BY created_at DESC, id DESC`
	}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := q.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ExpireDue marks pending items past their time as expired (= skipped) and wakes waiters.
func (q *Queue) ExpireDue(ctx context.Context) (int, error) {
	res, err := q.DB.ExecContext(ctx, `UPDATE approvals SET status = 'expired', resolved_at = ? WHERE status = 'pending' AND expires_at <= ?`,
		q.now().UnixMilli(), q.now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		q.wakeAll()
	}
	return int(n), nil
}

// Answer is a human's decision.
type Answer struct {
	Approve bool
	Set     map[string]string // field name → value
	Note    string
	Via     string // cli | notification | tray | chat | status-page
}

// Resolve records the answer. An item can be answered once.
func (q *Queue) Resolve(ctx context.Context, id string, a Answer) (Item, error) {
	it, err := q.Get(ctx, id)
	if err != nil {
		return Item{}, err
	}
	switch it.Status {
	case Expired:
		return it, ErrExpired
	case Approved, Skipped:
		return it, ErrResolved
	}
	if a.Approve {
		names := make([]string, len(it.Fields))
		for i, f := range it.Fields {
			names[i] = f.Name
		}
		for k := range a.Set {
			if !contains(names, k) {
				sort.Strings(names)
				return it, fmt.Errorf("no field %q on this approval (fields: %s)", k, strings.Join(names, ", "))
			}
		}
		for i, f := range it.Fields {
			v, set := a.Set[f.Name]
			if !set {
				v = f.Prefill
			}
			if v != "" && q.Validate != nil {
				nv, err := q.Validate(f, v)
				if err != nil {
					return it, fmt.Errorf("field %q: %w", f.Name, err)
				}
				v = nv
			}
			it.Fields[i].Value = v
		}
	}
	status := Skipped
	if a.Approve {
		status = Approved
	}
	fields, _ := json.Marshal(it.Fields)
	if len(it.Fields) == 0 {
		fields = []byte("[]")
	}
	res, err := q.DB.ExecContext(ctx, `UPDATE approvals SET status = ?, fields = ?, answered_via = ?, note = ?, resolved_at = ? WHERE id = ? AND status = 'pending'`,
		string(status), string(fields), nullable(a.Via), nullable(a.Note), q.now().UnixMilli(), id)
	if err != nil {
		return it, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return it, ErrResolved // someone else answered first
	}
	q.wakeAll()
	return q.Get(ctx, id)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (q *Queue) wakeAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, chs := range q.wake {
		for _, c := range chs {
			select {
			case c <- struct{}{}:
			default:
			}
		}
		delete(q.wake, id)
	}
}

// Wait blocks until the item is answered or expires, or ctx ends. Other processes answer through
// the database, so it also polls.
func (q *Queue) Wait(ctx context.Context, id string) (Item, error) {
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		it, err := q.Get(ctx, id)
		if err != nil {
			return it, err
		}
		if it.Status != Pending {
			return it, nil
		}
		ch := make(chan struct{}, 1)
		q.mu.Lock()
		if q.wake == nil {
			q.wake = map[string][]chan struct{}{}
		}
		q.wake[id] = append(q.wake[id], ch)
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return it, ctx.Err()
		case <-ch:
		case <-poll.C:
		}
	}
}

// ApprovalUseWindow is how long after being answered an approval can still be used by a re-run.
const ApprovalUseWindow = 24 * time.Hour

// UseApproval consumes an earlier "yes" for exactly this call (skill, tool and arguments, as
// CallKey computes) from the same client: `noskill approvals approve` answers a parked run, and the
// run is then started again. One approval covers one call, once.
func (q *Queue) UseApproval(ctx context.Context, key, client string) (Item, bool) {
	tx, err := q.DB.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, false
	}
	defer tx.Rollback()
	cutoff := q.now().Add(-ApprovalUseWindow).UnixMilli()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM approvals
		WHERE status = 'approved' AND kind = 'confirm' AND resolved_at >= ?
		  AND json_extract(details, '$.key') = ? AND COALESCE(json_extract(details, '$.client'), '') = ?
		  AND json_extract(details, '$.used') IS NULL
		ORDER BY resolved_at DESC LIMIT 1`, cutoff, key, client).Scan(&id)
	if err != nil {
		return Item{}, false
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET details = json_set(details, '$.used', 1) WHERE id = ?`, id); err != nil {
		return Item{}, false
	}
	if err := tx.Commit(); err != nil {
		return Item{}, false
	}
	it, err := q.Get(ctx, id)
	return it, err == nil
}

// SetResume stores what a parked run needs to continue (app.Continuation) on its item.
func (q *Queue) SetResume(ctx context.Context, id, token string) error {
	_, err := q.DB.ExecContext(ctx, `UPDATE approvals SET resume_token = ? WHERE id = ?`, nullable(token), id)
	return err
}

// Claim marks an approved item used and reports whether this caller was the one to do it. The
// daemon, the CLI and a chat client may all try to resume the same parked run; one wins.
func (q *Queue) Claim(ctx context.Context, id string) bool {
	res, err := q.DB.ExecContext(ctx, `UPDATE approvals SET details = json_set(details, '$.used', 1)
		WHERE id = ? AND status = 'approved' AND json_extract(details, '$.used') IS NULL`, id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// MarkUsed records that an approval has been acted on, so it can't cover a second call.
func (q *Queue) MarkUsed(ctx context.Context, id string) {
	q.DB.ExecContext(ctx, `UPDATE approvals SET details = json_set(details, '$.used', 1) WHERE id = ?`, id)
}
