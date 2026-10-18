package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"noskill/internal/agent"
	"noskill/internal/app"
	"noskill/internal/approvals"
)

func (s *Server) openAIChat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	cr, err := parseOpenAI(body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.chat(w, r, cr, wireOpenAI, body)
}

func (s *Server) ollamaChat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	cr, err := parseOllamaChat(body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.chat(w, r, cr, wireOllamaChat, body)
}

func (s *Server) ollamaGenerate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	cr, err := parseOllamaGenerate(body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.chat(w, r, cr, wireOllamaGenerate, body)
}

// approvalMode reads X-Noskill-Approval (PROXY.md "Approvals in chat"): chat (default), wait, deny.
func (s *Server) approvalMode(r *http.Request) string {
	switch m := strings.ToLower(r.Header.Get("X-Noskill-Approval")); m {
	case "wait", "deny":
		return m
	}
	if s.cfg.ApprovalsInChat {
		return "chat"
	}
	return "deny"
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, cr chatRequest, kind wireKind, body []byte) {
	ctx := r.Context()
	tgt, err := s.resolve(ctx, cr.Model)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tgt.Passthrough || cr.Raw {
		s.forward(w, r, body)
		return
	}
	client := ClientFrom(ctx)
	if tgt.Skill != "" && !client.MaySkill(tgt.Skill) {
		apiError(w, r, http.StatusForbidden, "this token may not use skill "+tgt.Skill)
		return
	}
	mode := s.approvalMode(r)
	fp := conversationFP(cr.Messages)
	rsp := newResponder(w, kind, cr.Stream, cr.Model, s.cfg.Progress)
	rsp.includeUsage = cr.IncludeUsage
	if cr.HasTools {
		// Client-side tools aren't forwarded yet (PROXY.md "Client-side tools"): skills still run
		// inside noskill, the client's own tool definitions are dropped for this request.
		rsp.SetHeader("X-Noskill-Tools-Dropped", "client tools are not supported yet")
	}

	rr := app.RunRequest{
		Messages: backendMessages(cr.Messages), Source: "proxy", Client: client.Name, ClientSkills: client.MaySkill,
		ConversationFP: fp, Model: tgt.Model, Preset: tgt.Preset, Think: cr.Think, Final: cr.options(), ApprovalMode: mode,
		ApprovalWait: time.Duration(s.cfg.ApprovalWaitSeconds) * time.Second,
	}
	if tgt.Skill != "" {
		rr.Skills = []string{tgt.Skill}
	}
	if !cr.Stream {
		rr.QueueWait = 60 * time.Second // streaming requests wait with keepalives instead
	}

	// Open WebUI sends titles, tags and follow-ups as separate requests with the chat's model name.
	// They run straight on the cheapest model with nothing attached (PROXY.md "Measured with Open WebUI").
	if strings.HasPrefix(strings.TrimSpace(cr.lastUser()), "### Task:") {
		rr.NoSkills, rr.NoLearn, rr.Source, rr.TaskType = true, true, "background", "chat"
		if m, err := s.App.CheapestChat(ctx); err == nil {
			rr.Model = m
		}
		mode = "deny"
	} else if mode == "chat" {
		if handled := s.answerFromChat(ctx, rsp, &rr, cr, client.Name, fp); handled {
			return
		}
	}

	view := &runView{rsp: rsp}
	rr.OnStart = func(runID, model string, skills []string) {
		rsp.SetRunID(runID)
		rsp.SetHeader("X-Noskill-Model", model)
		rsp.SetHeader("X-Noskill-Skills", strings.Join(skills, ","))
	}
	rr.OnEvent = view.onEvent

	stop := make(chan struct{})
	var wg sync.WaitGroup
	if cr.Stream {
		wg.Add(1)
		go func() { // keepalives while queueing, loading a model or running a long tool
			defer wg.Done()
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					rsp.Keepalive()
				case <-stop:
					return
				}
			}
		}()
	}
	res, err := s.App.Run(ctx, rr)
	close(stop)
	wg.Wait()

	if err != nil && !(res != nil && res.Stop == agent.StopCancelled) {
		if rsp.Started() {
			rsp.FailMidStream(err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	if res != nil && res.Stop == agent.StopWaiting {
		rsp.Content(s.approvalPrompt(ctx, res.WaitingFor))
	}
	rsp.Finish(res)
}

// runView turns engine events into client output. Steps with tools on are held back: until the
// step ends, text could be the answer or the model thinking aloud before a tool call. Answers
// from steps without tools stream as they arrive.
type runView struct {
	rsp      *responder
	mu       sync.Mutex
	final    bool
	streamed bool
}

func (v *runView) onEvent(ev agent.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch ev.Kind {
	case agent.EventStep:
		v.final, v.streamed = ev.Final, false
	case agent.EventDelta:
		if ev.Thinking != "" && v.final {
			v.rsp.Thinking(ev.Thinking)
		}
		if ev.Content != "" && v.final {
			v.rsp.Content(ev.Content)
			v.streamed = true
		}
	case agent.EventProgress:
		v.rsp.Progress(ev.Note)
	case agent.EventToolCall:
		v.rsp.Progress(toolLine(ev))
	case agent.EventToolDenied:
		v.rsp.Progress("Skipped " + ev.Tool + ": not available in this run")
	case agent.EventBudget:
		if ev.Note != "" {
			v.rsp.Progress(ev.Note)
		}
	case agent.EventAnswer:
		if !v.streamed {
			v.rsp.Content(ev.Content)
		}
	}
}

// toolLine is the progress text for a tool call: "Searching: ollama release" style, from the
// tool name and its first short string argument.
func toolLine(ev agent.Event) string {
	var args map[string]any
	json.Unmarshal(ev.Args, &args)
	detail := ""
	for _, k := range []string{"query", "url", "path", "name", "task", "goal", "from"} {
		if s, ok := args[k].(string); ok && s != "" {
			detail = s
			break
		}
	}
	if r := []rune(detail); len(r) > 80 {
		detail = string(r[:80]) + "…"
	}
	verb := map[string]string{"search": "Searching", "fetch": "Reading", "list_dir": "Looking in", "read_text": "Reading",
		"move": "Moving", "move_many": "Moving files", "copy_verify": "Copying", "delegate": "Asking another model", "notes": "Taking notes"}[ev.Tool]
	if verb == "" {
		verb = "Using " + ev.Tool
	}
	if detail != "" {
		return verb + ": " + detail
	}
	return verb + "…"
}

// ---- approvals in chat ----

func (s *Server) approvalPrompt(ctx context.Context, id string) string {
	it, err := s.App.Approvals.Get(ctx, id)
	if err != nil {
		return "I need your approval before going on. See `noskill approvals`."
	}
	var d struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	json.Unmarshal(it.Details, &d)
	var pretty bytes.Buffer
	if json.Indent(&pretty, d.Args, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(d.Args)
	}
	args := pretty.String()
	if r := []rune(args); len(r) > 900 {
		args = string(r[:900]) + "\n…"
	}
	return fmt.Sprintf("I need your OK to run `%s`:\n\n```json\n%s\n```\n\nReply **yes** to go ahead or **no** to cancel. (approval %s)", d.Tool, args, it.ID)
}

// answerFromChat handles a reply to a pending approval in this conversation. It returns true when
// it answered the request itself; otherwise it may rewrite rr to resume the approved run.
func (s *Server) answerFromChat(ctx context.Context, rsp *responder, rr *app.RunRequest, cr chatRequest, client, fp string) bool {
	pending, err := s.App.Approvals.List(ctx, false, 0)
	if err != nil {
		return false
	}
	var it *approvals.Item
	for i := range pending {
		p := pending[i]
		if p.Source != "proxy" || p.ConversationFP != fp {
			continue
		}
		var d struct{ Client string }
		json.Unmarshal(p.Details, &d)
		if d.Client == client {
			it = &p
			break
		}
	}
	if it == nil {
		return false
	}
	// Only the user's own latest message counts: never tool output, pages or the model's text.
	verdict, folder := classifyReply(cr.lastUser())
	switch verdict {
	case replyNo:
		s.App.Approvals.Resolve(ctx, it.ID, approvals.Answer{Approve: false, Via: "chat"})
		rsp.Content("Okay, I won't do that.")
		rsp.Finish(nil)
		return true
	case replyYes:
		set := map[string]string{}
		if folder != "" {
			for _, f := range it.Fields {
				if f.Type == "folder" {
					set[f.Name] = folder
					break
				}
			}
		}
		done, err := s.App.Approvals.Resolve(ctx, it.ID, approvals.Answer{Approve: true, Set: set, Via: "chat"})
		if err != nil {
			rsp.Content("I couldn't use that answer: " + err.Error())
			rsp.Finish(nil)
			return true
		}
		// The parked run continues from the call it stopped on (app.Continuation).
		next, err := s.App.ResumeRequest(ctx, done.ID, *rr)
		if err != nil {
			rsp.Content("Approved, but I can't pick that run up again: " + err.Error())
			rsp.Finish(nil)
			return true
		}
		*rr = next
	}
	return false
}

type replyKind int

const (
	replyOther replyKind = iota
	replyYes
	replyNo
)

var yesWords = []string{"yes", "yep", "yeah", "yup", "y", "ok", "okay", "sure", "go ahead", "do it", "approve", "approved", "confirm", "please do", "go for it", "proceed"}
var noWords = []string{"no", "nope", "n", "cancel", "stop", "don't", "dont", "do not", "skip", "never mind", "nevermind", "deny", "abort", "reject"}

// classifyReply reads a short yes/no answer. Anything long or mixed is "other": an approval needs
// an unambiguous reply, and a long message could be anything. A folder after "to/in/into/at" is
// returned for approvals with an editable folder field ("yes, save it to /Volumes/RED/ANIME").
func classifyReply(text string) (replyKind, string) {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" || len(strings.Fields(t)) > 14 {
		return replyOther, ""
	}
	has := func(words []string) bool {
		for _, w := range words {
			if t == w || strings.HasPrefix(t, w+" ") || strings.HasPrefix(t, w+",") || strings.HasPrefix(t, w+".") || strings.HasPrefix(t, w+"!") {
				return true
			}
		}
		return false
	}
	switch {
	case has(noWords):
		return replyNo, ""
	case has(yesWords):
		return replyYes, folderIn(strings.TrimSpace(text))
	}
	return replyOther, ""
}

// folderIn finds an absolute path in the user's own words: parsed, never guessed by a model.
func folderIn(text string) string {
	for _, marker := range []string{" to ", " in ", " into ", " at "} {
		_, rest, ok := strings.Cut(strings.ToLower(text), marker)
		if !ok {
			continue
		}
		orig := text[len(text)-len(rest):]
		orig = strings.TrimSpace(orig)
		if strings.HasPrefix(orig, `"`) || strings.HasPrefix(orig, "'") {
			q := orig[:1]
			if end := strings.Index(orig[1:], q); end >= 0 {
				orig = orig[1 : 1+end]
			}
		} else if i := strings.IndexAny(orig, " \t"); i >= 0 {
			orig = orig[:i]
		}
		orig = strings.TrimRight(orig, ".,;!?")
		if strings.HasPrefix(orig, "/") || strings.HasPrefix(orig, "~/") || strings.HasPrefix(orig, "volume:") || (len(orig) > 2 && orig[1] == ':' && (orig[2] == '\\' || orig[2] == '/')) {
			return orig
		}
	}
	return ""
}

var _ = io.EOF
