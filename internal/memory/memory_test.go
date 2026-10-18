package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"noskill/internal/store"
)

func newStore(t *testing.T, max int) *Store {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return &Store{DB: db, Max: max, Now: func() time.Time { clk = clk.Add(time.Second); return clk }}
}

var ctx = context.Background()

func TestAddListForget(t *testing.T) {
	s := newStore(t, 0)
	it, err := s.Add(ctx, "Editor", "Prefers Neovim over VS Code", "explicit", 1)
	if err != nil || it.ID == "" || it.Source != "explicit" {
		t.Fatalf("%+v %v", it, err)
	}
	// Same key (any case) replaces the value instead of adding a duplicate.
	it2, _ := s.Add(ctx, "editor", "Prefers Helix", "explicit", 1)
	if it2.ID != it.ID || it2.Value != "Prefers Helix" {
		t.Errorf("upsert: %+v", it2)
	}
	s.Add(ctx, "city", "Lives in Lisbon", "extracted", 0.6)
	all, _ := s.List(ctx)
	if len(all) != 2 || all[0].Key != "city" {
		t.Fatalf("list = %+v", all)
	}
	if n, _ := s.Forget(ctx, "CITY"); n != 1 {
		t.Errorf("forget by key: %d", n)
	}
	if n, _ := s.Forget(ctx, it.ID); n != 1 {
		t.Errorf("forget by id: %d", n)
	}
	if n, _ := s.Forget(ctx, "nothing"); n != 0 {
		t.Errorf("forget nothing: %d", n)
	}
	for _, bad := range [][2]string{{"", "v"}, {"k", ""}, {strings.Repeat("k", 81), "v"}, {"k", strings.Repeat("v", 501)}} {
		if _, err := s.Add(ctx, bad[0], bad[1], "explicit", 1); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := s.Add(ctx, "k", "v", "guess", 1); err == nil {
		t.Error("bad source accepted")
	}
}

func TestCredentialsAreNeverStored(t *testing.T) {
	s := newStore(t, 0)
	for _, v := range []string{
		"my password is hunter2",
		"the api key for brave is BSA1234",
		"token: ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"sk-proj-abc",
		"AKIAIOSFODNN7EXAMPLE",
		"use qB7x9Zk2Pq8Lm4Vn1Rt6Yw3Hs5 as the login",
		"my credit card number is 4111",
	} {
		if _, err := s.Add(ctx, "note", v, "explicit", 1); !errors.Is(err, ErrLooksSecret) {
			t.Errorf("%q stored (err %v)", v, err)
		}
	}
	for _, ok := range []string{"Prefers dark mode", "Lives in /Users/me/Documents/projects/noskill-long-folder-name", "Works on the anime-tracker skill"} {
		if _, err := s.Add(ctx, "note", ok, "explicit", 1); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

func TestRelevantAndEviction(t *testing.T) {
	s := newStore(t, 3)
	s.Add(ctx, "editor", "Prefers Neovim for editing code", "explicit", 1)
	s.Add(ctx, "language", "Writes mostly Go and Python code", "explicit", 1)
	s.Add(ctx, "pets", "Has a cat named Miso", "explicit", 1)
	got, _ := s.Relevant(ctx, "Which editor should I use for this code?", 2)
	if len(got) != 2 || got[0].Key != "editor" {
		t.Fatalf("relevant = %+v", got)
	}
	if got, _ := s.Relevant(ctx, "weather tomorrow", 3); len(got) != 0 {
		t.Errorf("unrelated query matched: %+v", got)
	}
	// "pets" was never used, so it's the first to go when a fourth fact arrives.
	s.Add(ctx, "city", "Lives in Lisbon", "explicit", 1)
	all, _ := s.List(ctx)
	keys := []string{}
	for _, it := range all {
		keys = append(keys, it.Key)
	}
	if len(all) != 3 || strings.Contains(strings.Join(keys, ","), "pets") {
		t.Errorf("after eviction: %v", keys)
	}
	if n, _ := s.ForgetAll(ctx); n != 3 {
		t.Errorf("forget all: %d", n)
	}
	_ = fmt.Sprint()
}

func TestExtract(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantKey string
		wantSrc string
	}{
		{"Remember that I use Neovim", "i use neovim", "explicit"},
		{"please remember: the build server is called atlas", "the build server is called", "explicit"},
		{"My name is Dana Reyes.", "name", "extracted"},
		{"I prefer short answers.", "i prefer short answers", "extracted"},
		{"Always answer in French", "always answer in french", "extracted"},
		{"I live in Lisbon. What's the weather?", "i live in lisbon", "extracted"},
	} {
		got := Extract(tc.in)
		if len(got) == 0 || got[0].Key != tc.wantKey || got[0].Source != tc.wantSrc {
			t.Errorf("%q → %+v", tc.in, got)
		}
	}
	for _, none := range []string{
		"What is the capital of France?", "I think the answer is 4.", "Write a function that sorts a list",
		"remember my password is hunter2", "My name is hunter2 and my api key is sk-abc123",
		"I use token ghp_abcdefghijklmnopqrstuvwxyz0123456789 for GitHub",
	} {
		for _, c := range Extract(none) {
			if c.Source == "explicit" || LooksSecret(c.Value) {
				t.Errorf("%q → %+v", none, c)
			}
		}
	}
	if got := Extract("I prefer tabs.\nAlways use Go modules. Never use cgo outside the tray"); len(got) != 3 {
		t.Errorf("several facts: %+v", got)
	}
}
