// Package memory keeps durable facts and preferences about the user (FR-L6): added by hand
// (`noskill memory add`, "remember that…") or extracted from what they say. Everything is visible
// with `noskill memory list` and removable with `forget`. Credentials are never stored.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"noskill/internal/ids"
	"noskill/internal/store"
)

type Item struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Value      string    `json:"value"`
	Source     string    `json:"source"` // explicit | extracted
	Confidence float64   `json:"confidence"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

type Store struct {
	DB  *store.DB
	Max int // [memory] max_items; the least recently used item goes first when full
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

const (
	maxValue = 500
	maxKey   = 80
)

var ErrLooksSecret = errors.New("that looks like a password, key or token, which noskill won't store in memory (use `noskill secrets set`)")

var secretWords = regexp.MustCompile(`(?i)\b(pass(word|wd|phrase)?|pin|api[ _-]?key|secret|token|private key|credit card|card number|ssn|social security)\b`)

// LooksSecret errs on the side of refusing: a memory is injected into prompts and may be sent to a
// model or shown in a trace, so anything credential-like stays out.
func LooksSecret(text string) bool {
	if secretWords.MatchString(text) {
		return true
	}
	for _, f := range strings.Fields(text) {
		f = strings.Trim(f, `.,;:!?"'()[]`)
		if len(f) >= 20 && entropy(f) > 3.6 && !strings.ContainsAny(f, "/\\") {
			return true // long random-looking run: a key
		}
		if strings.HasPrefix(f, "sk-") || strings.HasPrefix(f, "ghp_") || strings.HasPrefix(f, "xox") || strings.HasPrefix(f, "AKIA") {
			return true
		}
	}
	return false
}

func entropy(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	var h float64
	n := float64(len([]rune(s)))
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

func normKey(k string) string { return strings.ToLower(strings.TrimSpace(k)) }

// Add stores a fact, replacing the value of an existing item with the same key.
func (s *Store) Add(ctx context.Context, key, value, source string, conf float64) (Item, error) {
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch {
	case key == "" || value == "":
		return Item{}, errors.New("memory needs a key and a value")
	case len(key) > maxKey:
		return Item{}, fmt.Errorf("key is longer than %d characters", maxKey)
	case len(value) > maxValue:
		return Item{}, fmt.Errorf("value is longer than %d characters", maxValue)
	case source != "explicit" && source != "extracted":
		return Item{}, fmt.Errorf("source must be explicit or extracted")
	case LooksSecret(key + " " + value):
		return Item{}, ErrLooksSecret
	}
	now := s.now().UnixMilli()
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM memory WHERE lower(key) = ?`, normKey(key)).Scan(&id)
	switch {
	case err == nil:
		_, err = s.DB.ExecContext(ctx, `UPDATE memory SET value = ?, source = ?, confidence = ?, updated_at = ? WHERE id = ?`, value, source, conf, now, id)
	case errors.Is(err, sql.ErrNoRows):
		id = ids.New()
		_, err = s.DB.ExecContext(ctx, `INSERT INTO memory (id, key, value, source, confidence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, key, value, source, conf, now, now)
		if err == nil {
			err = s.trim(ctx)
		}
	}
	if err != nil {
		return Item{}, err
	}
	return s.get(ctx, id)
}

func (s *Store) trim(ctx context.Context) error {
	max := s.Max
	if max <= 0 {
		max = 2000
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM memory WHERE id IN (
		SELECT id FROM memory ORDER BY COALESCE(last_used_at, updated_at) DESC LIMIT -1 OFFSET ?)`, max)
	return err
}

const cols = `id, key, value, source, COALESCE(confidence, 1), created_at, updated_at, last_used_at`

func scan(r interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var c, u int64
	var lu sql.NullInt64
	if err := r.Scan(&it.ID, &it.Key, &it.Value, &it.Source, &it.Confidence, &c, &u, &lu); err != nil {
		return it, err
	}
	it.CreatedAt, it.UpdatedAt = time.UnixMilli(c), time.UnixMilli(u)
	if lu.Valid {
		it.LastUsedAt = time.UnixMilli(lu.Int64)
	}
	return it, nil
}

func (s *Store) get(ctx context.Context, id string) (Item, error) {
	return scan(s.DB.QueryRowContext(ctx, `SELECT `+cols+` FROM memory WHERE id = ?`, id))
}

func (s *Store) List(ctx context.Context) ([]Item, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+cols+` FROM memory ORDER BY lower(key)`)
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

// Forget removes an item by key or id and returns how many were removed.
func (s *Store) Forget(ctx context.Context, keyOrID string) (int, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM memory WHERE id = ? OR lower(key) = ?`, keyOrID, normKey(keyOrID))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ForgetAll removes everything.
func (s *Store) ForgetAll(ctx context.Context) (int, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM memory`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Relevant returns up to k items that share words with the query, best first, and marks them used.
// Embedding retrieval replaces this when the embed model is installed (it uses the same table).
func (s *Store) Relevant(ctx context.Context, query string, k int) ([]Item, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	qw := words(query)
	type scored struct {
		it Item
		sc float64
	}
	var hits []scored
	for _, it := range all {
		iw := words(it.Key + " " + it.Value)
		var common float64
		for w := range qw {
			if iw[w] {
				common++
			}
		}
		if common == 0 {
			continue
		}
		hits = append(hits, scored{it, common/float64(len(iw)+1) + common})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].sc != hits[j].sc {
			return hits[i].sc > hits[j].sc
		}
		return hits[i].it.Key < hits[j].it.Key
	})
	var out []Item
	for _, h := range hits {
		if len(out) >= k {
			break
		}
		out = append(out, h.it)
		s.DB.ExecContext(ctx, `UPDATE memory SET last_used_at = ? WHERE id = ?`, s.now().UnixMilli(), h.it.ID)
	}
	return out, nil
}

var stop = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "to": true, "of": true, "in": true, "is": true,
	"it": true, "i": true, "my": true, "me": true, "for": true, "on": true, "with": true, "that": true, "this": true, "are": true,
	"be": true, "do": true, "you": true, "your": true, "what": true, "how": true, "can": true, "please": true}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 && !stop[w] {
			out[stem(w)] = true
		}
	}
	return out
}

// stem folds the endings that separate "editor" from "editing" and "files" from "file".
func stem(w string) string {
	for _, suf := range []string{"ings", "ing", "ers", "er", "ors", "or", "ed", "es", "s"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}
