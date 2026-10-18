package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"noskill/internal/store"
	"noskill/internal/tools"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.UnixMilli(c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(d.Milliseconds()) }

func newQueue(t *testing.T) (*Queue, *clock) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{}
	c.t.Store(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC).UnixMilli())
	return &Queue{DB: db, Default: time.Hour, Now: c.now}, c
}

var ctx = context.Background()

func TestCreateListResolve(t *testing.T) {
	q, _ := newQueue(t)
	var announced []Item
	q.Announce = func(_ context.Context, it Item) { announced = append(announced, it) }
	it, err := q.Create(ctx, New{Source: "skill", Kind: "confirm", Summary: "files wants to run trash", Details: map[string]string{"tool": "trash"}, RunID: "r1"})
	if err != nil || it.Status != Pending || len(announced) != 1 || announced[0].ID != it.ID {
		t.Fatalf("%+v %v", it, err)
	}
	pending, _ := q.List(ctx, false, 0)
	if len(pending) != 1 || pending[0].ID != it.ID || string(pending[0].Details) != `{"tool":"trash"}` {
		t.Fatalf("pending = %+v", pending)
	}
	got, err := q.Resolve(ctx, it.ID, Answer{Approve: true, Via: "cli", Note: "ok"})
	if err != nil || got.Status != Approved || got.AnsweredVia != "cli" || got.Note != "ok" || got.ResolvedAt.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := q.Resolve(ctx, it.ID, Answer{Approve: false}); !errors.Is(err, ErrResolved) {
		t.Errorf("answered twice: %v", err)
	}
	if p, _ := q.List(ctx, false, 0); len(p) != 0 {
		t.Errorf("answered item still pending: %+v", p)
	}
	if all, _ := q.List(ctx, true, 0); len(all) != 1 {
		t.Errorf("history = %+v", all)
	}
	if _, err := q.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	if _, err := q.Create(ctx, New{Summary: " "}); err == nil {
		t.Error("empty summary accepted")
	}
}

func TestExpiryIsASkipNeverAnApproval(t *testing.T) {
	q, clk := newQueue(t)
	it, _ := q.Create(ctx, New{Source: "job", Kind: "pick", Summary: "new title"})
	clk.advance(2 * time.Hour)
	got, _ := q.Get(ctx, it.ID)
	if got.Status != Expired {
		t.Fatalf("status = %s", got.Status)
	}
	if _, err := q.Resolve(ctx, it.ID, Answer{Approve: true}); !errors.Is(err, ErrExpired) {
		t.Errorf("approving an expired item: %v", err)
	}
	if p, _ := q.List(ctx, false, 0); len(p) != 0 {
		t.Errorf("expired item pending: %+v", p)
	}
}

func TestEditableFields(t *testing.T) {
	q, _ := newQueue(t)
	q.Validate = func(f Field, v string) (string, error) {
		if f.Type == "folder" && strings.Contains(v, "..") {
			return "", errors.New("not allowed")
		}
		return strings.TrimRight(v, "/"), nil
	}
	mk := func() Item {
		it, err := q.Create(ctx, New{Source: "job", Kind: "pick", Summary: "Frieren S2",
			Fields: []Field{{Name: "dest", Type: "folder", Prefill: "/Volumes/RED/ANIME2026"}, {Name: "quality", Type: "string", Prefill: "480p"}}})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	it := mk()
	got, err := q.Resolve(ctx, it.ID, Answer{Approve: true, Set: map[string]string{"dest": "/Volumes/RED/ANIME2026-AUTUMN/"}})
	if err != nil {
		t.Fatal(err)
	}
	v := got.FieldValues()
	if v["dest"] != "/Volumes/RED/ANIME2026-AUTUMN" || v["quality"] != "480p" {
		t.Errorf("values = %v (a field left alone keeps its prefill)", v)
	}
	it = mk()
	if _, err := q.Resolve(ctx, it.ID, Answer{Approve: true, Set: map[string]string{"dest": "/a/../etc"}}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("invalid folder accepted: %v", err)
	}
	if _, err := q.Resolve(ctx, it.ID, Answer{Approve: true, Set: map[string]string{"nope": "x"}}); err == nil || !strings.Contains(err.Error(), "fields: dest, quality") {
		t.Errorf("unknown field: %v", err)
	}
	if g, _ := q.Get(ctx, it.ID); g.Status != Pending {
		t.Errorf("a refused answer must leave the item pending: %s", g.Status)
	}
	got, _ = q.Resolve(ctx, it.ID, Answer{Approve: false, Set: map[string]string{"dest": "/x"}})
	if got.Status != Skipped || got.FieldValues()["dest"] != "/Volumes/RED/ANIME2026" {
		t.Errorf("skip must not store edited values: %+v", got)
	}
	if _, err := q.Create(ctx, New{Summary: "x", Fields: []Field{{Name: "a", Type: "str"}}}); err == nil {
		t.Error("bad field type accepted")
	}
}

func TestWaitSeesAnswerFromAnotherProcess(t *testing.T) {
	q, _ := newQueue(t)
	it, _ := q.Create(ctx, New{Source: "skill", Kind: "confirm", Summary: "x"})
	done := make(chan Item, 1)
	go func() { got, _ := q.Wait(ctx, it.ID); done <- got }()
	time.Sleep(100 * time.Millisecond)
	// A second Queue on the same database stands in for `noskill approvals approve` in another process.
	other := &Queue{DB: q.DB, Now: q.Now}
	if _, err := other.Resolve(ctx, it.ID, Answer{Approve: true, Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Status != Approved {
			t.Errorf("status = %s", got.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait never saw the answer")
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	it2, _ := q.Create(ctx, New{Source: "skill", Kind: "confirm", Summary: "y"})
	if _, err := q.Wait(cctx, it2.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait without answer: %v", err)
	}
}

func req() tools.ConfirmRequest {
	return tools.ConfirmRequest{RunID: "r1", Skill: "files", Tool: "move_many", Args: json.RawMessage(`{"plan":[]}`)}
}

func TestConfirmerModes(t *testing.T) {
	q, _ := newQueue(t)
	// Headless: park.
	c := &Confirmer{Q: q, Headless: true}
	err := c.Confirm(ctx, req())
	w, ok := tools.IsWaiting(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if it, _ := q.Get(ctx, w.ApprovalID); it.Status != Pending || it.RunID != "r1" || !strings.Contains(it.Summary, "files wants to run move_many") {
		t.Errorf("parked item = %+v", it)
	}
	// Terminal prompt.
	c = &Confirmer{Q: q, Prompt: func(context.Context, Item) (bool, error) { return true, nil }}
	if err := c.Confirm(ctx, req()); err != nil {
		t.Errorf("approved at the terminal: %v", err)
	}
	c = &Confirmer{Q: q, Prompt: func(context.Context, Item) (bool, error) { return false, nil }}
	if err := c.Confirm(ctx, req()); !errors.Is(err, tools.ErrDeclined) {
		t.Errorf("declined at the terminal: %v", err)
	}
	// Chat client: waits for someone else, then gives up and skips.
	c = &Confirmer{Q: q, Wait: 150 * time.Millisecond}
	if err := c.Confirm(ctx, req()); !errors.Is(err, tools.ErrDeclined) {
		t.Errorf("no answer in time: %v", err)
	}
	all, _ := q.List(ctx, true, 1)
	if all[0].Status != Skipped || all[0].AnsweredVia != "timeout" {
		t.Errorf("a timed-out wait must leave a skipped item: %+v", all[0])
	}
	// Answered from elsewhere while waiting.
	q.Announce = func(_ context.Context, it Item) {
		go func() { time.Sleep(50 * time.Millisecond); q.Resolve(ctx, it.ID, Answer{Approve: true, Via: "tray"}) }()
	}
	c = &Confirmer{Q: q, Wait: 3 * time.Second}
	if err := c.Confirm(ctx, req()); err != nil {
		t.Errorf("approved via another channel: %v", err)
	}
}

func TestAnEarlierYesCoversExactlyOneMatchingCall(t *testing.T) {
	q, clk := newQueue(t)
	args := json.RawMessage(`{"plan":[{"from":"/a","to":"/b"}],"dry_run":false}`)
	// The run parks (headless), then a person approves it from the CLI.
	c := &Confirmer{Q: q, Headless: true}
	err := c.Confirm(ctx, tools.ConfirmRequest{RunID: "r1", Skill: "files", Tool: "move_many", Args: args})
	w, _ := tools.IsWaiting(err)
	if w == nil {
		t.Fatalf("%v", err)
	}
	if _, err := q.Resolve(ctx, w.ApprovalID, Answer{Approve: true, Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	// Same call again (arguments in another key order): goes through once.
	reordered := json.RawMessage(`{"dry_run":false,"plan":[{"to":"/b","from":"/a"}]}`)
	req := tools.ConfirmRequest{RunID: "r2", Skill: "files", Tool: "move_many", Args: reordered}
	if err := c.Confirm(ctx, req); err != nil {
		t.Fatalf("the approved call was asked about again: %v", err)
	}
	if _, waiting := tools.IsWaiting(c.Confirm(ctx, req)); !waiting {
		t.Error("one approval was used twice")
	}
	// A different call, tool or client never rides on it.
	pending, _ := q.List(ctx, false, 0)
	for _, p := range pending {
		q.Resolve(ctx, p.ID, Answer{Approve: false})
	}
	other := tools.ConfirmRequest{RunID: "r3", Skill: "files", Tool: "move_many", Args: json.RawMessage(`{"plan":[{"from":"/a","to":"/c"}]}`)}
	if _, waiting := tools.IsWaiting(c.Confirm(ctx, other)); !waiting {
		t.Error("an approval covered different arguments")
	}
	// An old approval expires.
	c2 := &Confirmer{Q: q, Headless: true}
	e2 := c2.Confirm(ctx, tools.ConfirmRequest{RunID: "r4", Skill: "files", Tool: "trash", Args: json.RawMessage(`{"path":"/x"}`)})
	w2, _ := tools.IsWaiting(e2)
	q.Resolve(ctx, w2.ApprovalID, Answer{Approve: true})
	clk.advance(25 * time.Hour)
	if _, waiting := tools.IsWaiting(c2.Confirm(ctx, tools.ConfirmRequest{RunID: "r5", Skill: "files", Tool: "trash", Args: json.RawMessage(`{"path":"/x"}`)})); !waiting {
		t.Error("a day-old approval was still honoured")
	}
	// Another client's approval isn't mine.
	cA := &Confirmer{Q: q, Headless: true, Client: "alice"}
	eA := cA.Confirm(ctx, tools.ConfirmRequest{RunID: "r6", Skill: "files", Tool: "trash", Args: json.RawMessage(`{"path":"/y"}`)})
	wA, _ := tools.IsWaiting(eA)
	q.Resolve(ctx, wA.ApprovalID, Answer{Approve: true})
	cB := &Confirmer{Q: q, Headless: true, Client: "bob"}
	if _, waiting := tools.IsWaiting(cB.Confirm(ctx, tools.ConfirmRequest{RunID: "r7", Skill: "files", Tool: "trash", Args: json.RawMessage(`{"path":"/y"}`)})); !waiting {
		t.Error("bob used alice's approval")
	}
}
