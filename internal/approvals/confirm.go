package approvals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"noskill/internal/tools"
)

// Confirmer turns a `confirm` tool call into an approval and decides what the caller does next
// (tools.Executor.Confirm). Interactive callers (a terminal, a chat client) wait for the answer;
// headless callers (jobs) get tools.ErrWaiting and the run parks until someone answers.
type Confirmer struct {
	Q              *Queue
	Source         string // approvals.source: "skill" for runs, "proxy" for chat clients
	Headless       bool
	Wait           time.Duration // interactive: how long to wait (default 2 minutes)
	ConversationFP string
	Client         string          // proxy token name: only that client may answer in chat
	Resume         json.RawMessage // what a parked run needs to be re-run once approved (stored on the item)
	Deny           bool            // scripts: every confirm action is declined at once
	Auto           bool            // eval tasks in a throwaway folder: every confirm action is approved, nothing queued
	// Pre lists calls that were already approved (see CallKey); a resumed run passes them through.
	Pre map[string]bool
	// Prompt, if set, asks the person at this terminal directly instead of waiting for another
	// channel. It must not return an approval the person didn't give.
	Prompt func(ctx context.Context, it Item) (approve bool, err error)
}

// CallKey identifies one confirm call by skill, tool and arguments. An approval covers exactly
// that call: a model that asks for something else needs a new yes.
func CallKey(skill, tool string, args json.RawMessage) string {
	var v any
	if json.Unmarshal(args, &v) == nil {
		if b, err := json.Marshal(v); err == nil { // canonical key order
			args = b
		}
	}
	h := sha256.Sum256(args)
	return skill + "|" + tool + "|" + hex.EncodeToString(h[:8])
}

func (c *Confirmer) Confirm(ctx context.Context, req tools.ConfirmRequest) error {
	key := CallKey(req.Skill, req.Tool, req.Args)
	if c.Pre[key] {
		return nil
	}
	if _, ok := c.Q.UseApproval(ctx, key, c.Client); ok {
		return nil // answered earlier with `noskill approvals approve`
	}
	if c.Deny {
		return tools.ErrDeclined
	}
	if c.Auto {
		return nil
	}
	source := c.Source
	if source == "" {
		source = "skill"
	}
	it, err := c.Q.Create(ctx, New{
		Source: source, Kind: "confirm", Summary: fmt.Sprintf("%s wants to run %s", req.Skill, req.Tool),
		Details: map[string]any{"skill": req.Skill, "tool": req.Tool, "args": req.Args, "key": key, "client": c.Client},
		RunID:   req.RunID, ConversationFP: c.ConversationFP, ResumeToken: string(c.Resume),
	})
	if err != nil {
		return fmt.Errorf("couldn't ask for approval: %w", err)
	}
	if c.Headless {
		return &tools.ErrWaiting{ApprovalID: it.ID}
	}
	if c.Prompt != nil {
		ok, err := c.Prompt(ctx, it)
		if err != nil {
			return err
		}
		return c.finish(ctx, it.ID, ok, "cli")
	}
	wait := c.Wait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	got, err := c.Q.Wait(wctx, it.ID)
	switch {
	case err == nil && got.Status == Approved:
		c.Q.MarkUsed(ctx, it.ID)
		return nil
	case err == nil:
		return tools.ErrDeclined
	case ctx.Err() != nil:
		return ctx.Err()
	}
	// Nobody answered in time: skip it now rather than leaving an approval for a request the
	// client has already given up on.
	c.Q.Resolve(context.WithoutCancel(ctx), it.ID, Answer{Approve: false, Note: "no answer in time", Via: "timeout"})
	return tools.ErrDeclined
}

func (c *Confirmer) finish(ctx context.Context, id string, approve bool, via string) error {
	_, err := c.Q.Resolve(ctx, id, Answer{Approve: approve, Via: via})
	switch {
	case err != nil && !errors.Is(err, ErrResolved):
		return err
	case !approve:
		return tools.ErrDeclined
	}
	c.Q.MarkUsed(ctx, id) // the call proceeds now; the yes is spent
	return nil
}
