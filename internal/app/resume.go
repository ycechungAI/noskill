package app

import (
	"context"
	"encoding/json"
	"errors"

	"noskill/internal/approvals"
	"noskill/internal/backends"
)

// Continuation is a parked run's state, stored on the approval it waits for (approvals.resume_token)
// so the run picks up where it stopped once someone says yes (FR-A6, ARCHITECTURE.md "Approvals").
// Re-running the whole request instead would let a small model ask for a slightly different call
// the second time, which the yes doesn't cover.
type Continuation struct {
	V        int                   `json:"v"`
	RunID    string                `json:"run_id"` // the parked run
	Model    string                `json:"model"`
	Skills   []string              `json:"skills"` // primary skills; dependencies are added again
	Strategy backends.ToolStrategy `json:"strategy"`
	Messages []backends.Message    `json:"messages"` // as the model last saw them, ending with its tool calls
	Pending  []backends.ToolCall   `json:"pending"`  // the call waiting for the yes, then any after it
	Steps    int                   `json:"steps"`

	Source         string            `json:"source"`
	Client         string            `json:"client,omitempty"`
	ConversationFP string            `json:"conversation_fp,omitempty"`
	Allow          []string          `json:"allow,omitempty"`
	Think          *bool             `json:"think,omitempty"`
	Final          *backends.Options `json:"final,omitempty"`
	NoLearn        bool              `json:"no_learn,omitempty"`
}

// maxContinuation caps what's stored on an approval. A larger parked run can't be resumed; the
// request is asked again instead (and the yes still covers that exact call, via UseApproval).
const maxContinuation = 4 << 20

var (
	ErrNotAnswered    = errors.New("this approval hasn't been answered yet")
	ErrNotApproved    = errors.New("this approval was skipped, so the run it was holding stays stopped")
	ErrCantResume     = errors.New("this run can't be resumed (it was too large to keep); run the task again and that call will go through")
	ErrAlreadyResumed = errors.New("this run was already resumed")
)

// park stores what's needed to resume a run that stopped for an approval.
func (a *App) park(ctx context.Context, approvalID string, c Continuation) {
	c.V = 1
	b, err := json.Marshal(c)
	if err != nil || len(b) > maxContinuation {
		return
	}
	a.Approvals.SetResume(context.WithoutCancel(ctx), approvalID, string(b))
}

func parseContinuation(tok string) (Continuation, bool) {
	var c Continuation
	if tok == "" || json.Unmarshal([]byte(tok), &c) != nil || c.V != 1 || len(c.Pending) == 0 || c.Model == "" {
		return Continuation{}, false
	}
	return c, true
}

// Resumable reports whether an approval holds a parked run.
func Resumable(it approvals.Item) bool {
	_, ok := parseContinuation(it.ResumeToken)
	return ok
}

// ResumeRequest turns an approved item into the request that continues its parked run. It claims
// the approval, so only one caller (daemon, CLI or chat) ever resumes a run. base supplies how the
// caller wants the run delivered (events, streaming options); the rest comes from the parked run.
func (a *App) ResumeRequest(ctx context.Context, approvalID string, base RunRequest) (RunRequest, error) {
	it, err := a.Approvals.Get(ctx, approvalID)
	if err != nil {
		return base, err
	}
	switch it.Status {
	case approvals.Pending:
		return base, ErrNotAnswered
	case approvals.Skipped, approvals.Expired:
		return base, ErrNotApproved
	}
	c, ok := parseContinuation(it.ResumeToken)
	if !ok {
		return base, ErrCantResume
	}
	var d struct{ Key string }
	json.Unmarshal(it.Details, &d)
	if !a.Approvals.Claim(ctx, approvalID) {
		return base, ErrAlreadyResumed
	}
	req := base
	req.Messages = c.Messages
	req.Model, req.Skills, req.NoSkills = c.Model, c.Skills, false
	req.Source, req.Client, req.ConversationFP = c.Source, c.Client, c.ConversationFP
	req.Allow, req.NoLearn, req.ParentID = c.Allow, c.NoLearn, c.RunID
	if base.Think == nil {
		req.Think = c.Think
	}
	if base.Final == nil {
		req.Final = c.Final
	}
	req.PreApproved = map[string]bool{d.Key: true}
	req.cont = &c
	return req, nil
}

// Continue resumes the run parked on an approved item and runs it to its next stop.
func (a *App) Continue(ctx context.Context, approvalID string, base RunRequest) (*RunResult, error) {
	req, err := a.ResumeRequest(ctx, approvalID, base)
	if err != nil {
		return nil, err
	}
	return a.Run(ctx, req)
}
