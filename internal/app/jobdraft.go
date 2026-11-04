package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"noskill/internal/scheduler"
)

// JobDraft is a job the local model filled in from a plain-language description. The Add a job
// form shows it for editing; nothing is scheduled until the user adds it (AddJob checks it then).
type JobDraft struct {
	Target string   `json:"target"`
	Kind   string   `json:"kind"` // "every" or "cron"
	When   string   `json:"when"`
	Name   string   `json:"name,omitempty"`
	Prompt string   `json:"prompt,omitempty"`
	Only   []string `json:"only,omitempty"` // preconditions (FR-J5), as AddJob takes them
	Notify bool     `json:"notify"`
	Model  string   `json:"model"`
	Notes  []string `json:"notes,omitempty"` // what was dropped or changed, for the user to check
}

var jobDraftSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"target":{"type":"string"},` +
	`"kind":{"type":"string","enum":["every","cron"]},` +
	`"when":{"type":"string"},` +
	`"name":{"type":"string"},` +
	`"prompt":{"type":"string"},` +
	`"only":{"type":"array","items":{"type":"string"}},` +
	`"notify":{"type":"boolean"}},` +
	`"required":["target","kind","when","name","prompt","only","notify"]}`)

// DraftJob asks the local model to fill in a job from a description. It only drafts: the
// schedule, target and conditions are checked by AddJob when the user adds the job.
func (a *App) DraftJob(ctx context.Context, text string) (JobDraft, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return JobDraft{}, &UserError{"describe the job first, e.g. every morning at 7 run the daily brief"}
	}
	models, _ := a.Models(ctx)
	m, err := a.draftModel(ctx, models)
	if err != nil {
		return JobDraft{}, &UserError{"auto fill needs a chat model: " + err.Error()}
	}
	out, err := a.authorLLM(m)(ctx, a.jobDraftPrompt(text), jobDraftSchema, 400)
	if err != nil {
		return JobDraft{}, err
	}
	var d JobDraft
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return JobDraft{}, errors.New("the model's draft wasn't readable; fill the form yourself")
	}
	d.Target = strings.TrimSpace(d.Target)
	d.When = strings.TrimSpace(d.When)
	d.Name = strings.TrimSpace(d.Name)
	d.Prompt = strings.TrimSpace(d.Prompt)
	d.Model = m.Name
	if d.Kind != "cron" {
		d.Kind = "every"
	}
	var only []string
	for _, c := range d.Only {
		p, err := scheduler.ParsePrecondition(c)
		if err != nil {
			d.Notes = append(d.Notes, fmt.Sprintf("left out the condition %q", c))
			continue
		}
		only = append(only, p)
	}
	d.Only = only
	if d.Target == "" {
		d.Notes = append(d.Notes, "the model didn't name what to run; write it in What")
	}
	return d, nil
}

const draftModelKey = "jobs.draft_model"

// DraftModelInfo is the model Auto fill uses: the user's choice (Chosen, "" = automatic), the model
// it would use now (Using), and the chat models that can be chosen (Choices).
type DraftModelInfo struct {
	Chosen      string     `json:"chosen"`
	Using       string     `json:"using"`
	Choices     []string   `json:"choices"`
	Recommended []ChatPick `json:"recommended"` // RecommendedChat, with whether each is installed
}

// ChatPick is a recommended chat model and whether it's already installed.
type ChatPick struct {
	Name      string  `json:"name"`
	Role      string  `json:"role"`
	SizeGB    float64 `json:"size_gb"`
	Installed bool    `json:"installed"`
}

// DraftModelInfo reports the Auto fill model choice (FR-J7), and the recommended models to download.
func (a *App) DraftModelInfo(ctx context.Context) DraftModelInfo {
	models, _ := a.Models(ctx)
	info := DraftModelInfo{Chosen: a.chosenDraftModel(ctx), Choices: []string{}, Recommended: []ChatPick{}}
	for _, m := range models {
		if m.chat() {
			info.Choices = append(info.Choices, m.Name)
		}
	}
	for _, r := range RecommendedChat {
		p := ChatPick{Name: r.Name, Role: r.Role, SizeGB: r.SizeGB}
		for _, m := range models {
			if m.Name == r.Name || m.Name == r.Name+":latest" {
				p.Installed = true
			}
		}
		info.Recommended = append(info.Recommended, p)
	}
	if m, err := a.draftModel(ctx, models); err == nil {
		info.Using = m.Name
	}
	return info
}

// SetDraftModel stores the Auto fill model; "" goes back to automatic. It must be an installed chat model.
func (a *App) SetDraftModel(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name != "" {
		models, _ := a.Models(ctx)
		if !slices.ContainsFunc(models, func(m Model) bool { return m.Name == name && m.chat() }) {
			return &UserError{fmt.Sprintf("%s isn't an installed chat model", name)}
		}
	}
	return a.DB.SetState(ctx, draftModelKey, name, time.Now())
}

func (a *App) chosenDraftModel(ctx context.Context) string {
	v, _ := a.DB.GetState(ctx, draftModelKey)
	return strings.TrimSpace(v)
}

// draftModel is the user's Auto fill choice if it's still installed, else the learning author pick.
func (a *App) draftModel(ctx context.Context, models []Model) (Model, error) {
	if want := a.chosenDraftModel(ctx); want != "" {
		for _, m := range models {
			if m.Name == want && m.chat() {
				return m, nil
			}
		}
	}
	return a.authorModel(models, "")
}

// jobDraftPrompt is short on purpose: the local models that run this have a small context.
func (a *App) jobDraftPrompt(text string) string {
	var names []string
	for _, s := range a.Skills.All() {
		if !s.NeedsApproval() {
			names = append(names, s.Name)
		}
	}
	return "Turn the request into a scheduled job for a personal assistant. Reply with JSON only.\n" +
		"Fields:\n" +
		"- target: a skill name from this list, exactly: " + strings.Join(names, ", ") + ".\n" +
		"  Or one of: brief (daily summary), backup (back up the job list), diskwatch (disk space check), remind:<text> (a reminder).\n" +
		"  Or, when no skill fits, the task written as a plain sentence.\n" +
		"- kind: \"every\" for an interval, or \"cron\" for a fixed time.\n" +
		"- when: for every, an interval like 30m, 6h or 1d. For cron, five fields: minute hour day month weekday, e.g. \"0 7 * * *\" for 07:00 daily.\n" +
		"- name: a short name for the job, or \"\".\n" +
		"- prompt: for a skill job, what the skill should do; otherwise \"\".\n" +
		"- only: conditions, from volume:<label>, ac-power, unmetered, window:HH:MM-HH:MM; or [].\n" +
		"- notify: true if the user wants the result sent to them.\n" +
		"Request: " + text
}
