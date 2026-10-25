package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"noskill/internal/chores"
	"noskill/internal/platform"
	"noskill/internal/tools"
)

// The housekeeping skills' tools (downloads-tidy, duplicate-finder, disk-watch, media-renamer,
// daily-brief). The planning runs in Go (internal/chores): a plan tool reads and proposes, and the
// files skill's move_many and trash apply it after the user's approval, so a small model never
// builds a plan by hand. Nothing here changes a file.

// ChoresService is what the daily-brief tools need from the application.
type ChoresService interface {
	// Brief is the digest of the last `hours` hours.
	Brief(ctx context.Context, hours int) (string, error)
	// AddReminder reads a sentence and schedules it; it returns what was understood, as JSON.
	AddReminder(ctx context.Context, text string) (string, error)
	ListReminders(ctx context.Context) (string, error)
	RemoveReminder(ctx context.Context, name string) error
}

func choresSvc(env *tools.Env) (ChoresService, error) {
	if s, ok := env.Svc.(ChoresService); ok && s != nil {
		return s, nil
	}
	return nil, errors.New("reminders and the daily brief aren't available in this run")
}

// Chores returns the housekeeping builtins.
func Chores() []tools.Builtin {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []tools.Builtin{
		{ID: "tidy_plan", Description: "Propose how to sort the loose files of a folder (default Downloads) into Documents, Images, Installers and so on. Returns a plan for move_many, old installers for the trash, and the files it leaves alone with the reason. Changes nothing.",
			Schema: obj(`{"type":"object","properties":{"folder":{"type":"string","default":"$DOWNLOADS"},"only":{"type":"array","items":{"type":"string"},"description":"Sort only these categories, e.g. [\"Documents\",\"Images\"]"}}}`),
			Run:    tidyPlan},
		{ID: "tidy_apply", Description: "Do the tidy: plan the folder again right now and carry it out, moving each file into its folder and old installers to the OS trash. Asks the user first. Nothing is deleted for good and nothing is replaced; the moves can be undone.",
			Schema: obj(`{"type":"object","properties":{"folder":{"type":"string","default":"$DOWNLOADS"},"only":{"type":"array","items":{"type":"string"}}}}`),
			Run:    tidyApply},
		{ID: "dupes_find", Description: "Find files with identical content in one or more folders. Returns groups with the copy to keep and the others as candidates for the trash, and how much space they take. Changes nothing.",
			Schema: obj(`{"type":"object","required":["folders"],"properties":{"folders":{"type":"array","minItems":1,"maxItems":6,"items":{"type":"string"}},"min_mb":{"type":"number","default":1,"minimum":0,"description":"Ignore files smaller than this many MB"},"include_small":{"type":"boolean","default":false,"description":"Also compare files under 1 MB, however small"}}}`),
			Run:    dupesFind},
		{ID: "dupes_trash", Description: "Move duplicate copies to the OS trash. Each path must have exactly the same content as keep (checked again now) and may not be keep itself. Nothing is deleted for good.",
			Schema: obj(`{"type":"object","required":["keep","paths"],"properties":{"keep":{"type":"string","description":"The copy that stays"},"paths":{"type":"array","minItems":1,"maxItems":50,"items":{"type":"string"}}}}`),
			Run:    dupesTrash},
		{ID: "disk_report", Description: "Free space on every drive, flagging the ones under the limits. With a path, also the folders that take the most space under it and the large files nobody has opened for a year. Changes nothing.",
			Schema: obj(`{"type":"object","properties":{"path":{"type":"string","description":"A folder to look inside, e.g. volume:RED"},"idle_days":{"type":"integer","default":365}}}`),
			Run:    diskReport},
		{ID: "rename_plan", Description: "Propose one naming pattern for the episodes in a folder, reading the show and episode number from each file name. Returns a plan for move_many and the files it leaves alone with the reason. Changes nothing.",
			Schema: obj(`{"type":"object","required":["folder"],"properties":{"folder":{"type":"string"},"show":{"type":"string","description":"The show's name for every file; leave out to read it from each name"},"style":{"type":"string","enum":["episode","ep","sxxexx"],"default":"episode","description":"Show Episode 3, Show EP 3 or Show S01E03"}}}`),
			Run:    renamePlan},
		{ID: "rename_apply", Description: "Do the renames: plan the folder again right now and carry it out. Asks the user first. Nothing is replaced and the renames can be undone.",
			Schema: obj(`{"type":"object","required":["folder"],"properties":{"folder":{"type":"string"},"show":{"type":"string"},"style":{"type":"string","enum":["episode","ep","sxxexx"],"default":"episode"}}}`),
			Run:    renameApply},
		{ID: "brief_build", Description: "What happened on this computer since a while ago: scheduled jobs that ran or failed, episodes filed, approvals waiting, low disk space and what runs next.",
			Schema: obj(`{"type":"object","properties":{"hours":{"type":"integer","default":24,"minimum":1,"maximum":168}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				s, err := choresSvc(env)
				if err != nil {
					return "", err
				}
				a := struct{ Hours int }{Hours: 24}
				if len(raw) > 0 {
					arg(raw, &a)
				}
				return s.Brief(ctx, a.Hours)
			}},
		{ID: "reminder_add", Description: "Schedule a reminder from the user's sentence, such as \"remind me to call Sam tomorrow at 5pm\" or \"every Monday at 9 submit the report\". Returns what was understood (the text and when). If it can't tell when, it says what to ask the user.",
			Schema: obj(`{"type":"object","required":["text"],"properties":{"text":{"type":"string","description":"The user's own words"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				s, err := choresSvc(env)
				if err != nil {
					return "", err
				}
				var a struct{ Text string }
				arg(raw, &a)
				return s.AddReminder(ctx, a.Text)
			}},
		{ID: "reminder_list", Description: "List the reminders: name, text and when.",
			Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
				s, err := choresSvc(env)
				if err != nil {
					return "", err
				}
				return s.ListReminders(ctx)
			}},
		{ID: "reminder_remove", Description: "Remove a reminder by the name reminder_list shows.",
			Schema: obj(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				s, err := choresSvc(env)
				if err != nil {
					return "", err
				}
				var a struct{ Name string }
				arg(raw, &a)
				if err := s.RemoveReminder(ctx, a.Name); err != nil {
					return "", err
				}
				return jsonOut(map[string]any{"removed": a.Name})
			}},
	}
}

type tidyArgs struct {
	Folder string
	Only   []string
}

// planTidy reads the folder and plans it; tidy_plan shows the result and tidy_apply carries it out.
func planTidy(env *tools.Env, raw json.RawMessage, write bool) (root string, p chores.TidyPlan, err error) {
	a := tidyArgs{Folder: "$DOWNLOADS"}
	if len(raw) > 0 {
		arg(raw, &a)
	}
	if a.Folder == "" {
		a.Folder = "$DOWNLOADS"
	}
	if root, err = env.Expand(a.Folder); err != nil {
		return "", p, err
	}
	check := env.Perms.CheckRead // looking at a folder needs only read access; applying the plan needs write
	if write {
		check = env.Perms.CheckWrite
	}
	if root, err = check(root); err != nil {
		return "", p, err
	}
	des, err := os.ReadDir(root)
	if err != nil {
		return "", p, err
	}
	var entries []chores.Entry
	for _, d := range des {
		fi, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, chores.Entry{Name: d.Name(), Size: fi.Size(), ModTime: fi.ModTime(), IsDir: d.IsDir(), Link: d.Type()&os.ModeSymlink != 0})
	}
	o := chores.TidyOptions{Only: a.Only}
	// Whether a file changed today is left alone is the user's setting, not something the model can
	// switch off in a call: a small model flipped it unprompted in a trial.
	if v, ok := skillSetting(env, "min_age_hours"); ok {
		if h, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && h >= 0 {
			o.MinAge = time.Duration(h * float64(time.Hour))
			if h == 0 {
				o.MinAge = time.Nanosecond
			}
		}
	}
	if v, ok := skillSetting(env, "folder"); ok && v != "" {
		o.Folder = v
	}
	return root, chores.PlanTidy(root, entries, time.Now(), o), nil
}

func tidyPlan(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	root, p, err := planTidy(env, raw, false)
	if err != nil {
		return "", err
	}
	next := "tell the user what it found, then tidy_apply with the same arguments (it asks them first)"
	return jsonOut(map[string]any{"folder": root, "moves": len(p.Moves), "summary": p.Summary, "old_installers_to_trash": len(p.Trash),
		"examples": firstMoves(p.Moves, 6), "left_alone_count": len(p.Kept), "left_alone": capKept(p.Kept), "next": next})
}

func tidyApply(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	root, p, err := planTidy(env, raw, true)
	if err != nil {
		return "", err
	}
	moved, skipped := applyMoves(ctx, env, p.Moves)
	trashed := []string{}
	for _, t := range p.Trash {
		if err := env.Broker.TrashPath(ctx, t); err != nil {
			skipped = append(skipped, filepath.Base(t)+": "+friendly(err).Error())
			continue
		}
		trashed = append(trashed, filepath.Base(t))
	}
	return jsonOut(map[string]any{"folder": root, "moved": moved, "trashed": trashed, "skipped": skipped,
		"undo": "noskill undo <run-id> puts the moves back; the trashed files are in the OS trash"})
}

// capKept keeps the list the model reads short: a folder of hundreds of files must not fill an 8k context.
// The count is reported separately.
func capKept(k []chores.Kept) []chores.Kept {
	if len(k) > 20 {
		return k[:20]
	}
	return k
}

type exampleMove struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func firstMoves(ms []chores.Move, n int) []exampleMove {
	out := []exampleMove{}
	for i, m := range ms {
		if i == n {
			break
		}
		out = append(out, exampleMove{filepath.Base(m.From), filepath.ToSlash(strings.TrimPrefix(m.To, filepath.Dir(m.From)+string(filepath.Separator)))})
	}
	return out
}

// applyMoves moves each file, making its folder first; a move that can't happen is reported, not fatal.
func applyMoves(ctx context.Context, env *tools.Env, ms []chores.Move) (moved int, skipped []string) {
	skipped = []string{}
	for _, m := range ms {
		if _, err := os.Lstat(m.To); err == nil {
			skipped = append(skipped, filepath.Base(m.From)+": the target already exists")
			continue
		}
		if err := moveInto(ctx, env, m.From, m.To); err != nil {
			skipped = append(skipped, filepath.Base(m.From)+": "+friendly(err).Error())
			continue
		}
		moved++
	}
	return moved, skipped
}

func dupesFind(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Folders      []string
		MinMB        float64 `json:"min_mb"`
		IncludeSmall bool    `json:"include_small"`
	}
	arg(raw, &a)
	var roots []string
	for _, f := range a.Folders {
		p, err := env.Expand(f)
		if err != nil {
			return "", err
		}
		if p, err = env.Perms.CheckRead(p); err != nil {
			return "", err
		}
		roots = append(roots, p)
	}
	if len(roots) == 0 {
		return "", errors.New("name at least one folder")
	}
	o := chores.DupeOptions{}
	if a.MinMB > 0 {
		o.MinSize = int64(a.MinMB * (1 << 20))
	}
	if a.IncludeSmall {
		o.MinSize = 1
	}
	res, err := chores.FindDuplicates(ctx, roots, o)
	if err != nil {
		return "", err
	}
	if len(res.Groups) > 40 {
		res.Truncated = strings.TrimSpace(res.Truncated + " showing the 40 biggest of " + strconv.Itoa(len(res.Groups)) + " groups")
		res.Groups = res.Groups[:40]
	}
	return jsonOut(map[string]any{"groups": res.Groups, "files_scanned": res.Scanned, "space_wasted": chores.Human(res.WastedBytes),
		"note": res.Truncated, "next": "ask the user which copies to trash; trash one path at a time, never the Keep path"})
}

func diskReport(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Path     string
		IdleDays int `json:"idle_days"`
	}
	if len(raw) > 0 {
		arg(raw, &a)
	}
	vs, err := platform.ListVolumes()
	if err != nil {
		return "", err
	}
	o := chores.DiskOptions{}
	o.MinFreeGB = settingFloat(env, "min_free_gb")
	o.MinFreePct = settingFloat(env, "min_free_percent")
	out := map[string]any{"volumes": chores.CheckVolumes(vs, o)}
	if a.Path != "" {
		root, err := env.Expand(a.Path)
		if err != nil {
			return "", err
		}
		if root, err = env.Perms.CheckRead(root); err != nil {
			return "", err
		}
		idle := 365
		if a.IdleDays > 0 {
			idle = a.IdleDays
		}
		folders, old, trunc := chores.Usage(ctx, root, 10, 500<<20, time.Duration(idle)*24*time.Hour, 60*time.Second)
		out["biggest_folders"], out["large_idle_files"], out["cut_short"] = folders, old, trunc
	}
	return jsonOut(out)
}

func skillSetting(env *tools.Env, key string) (string, bool) {
	if env.Setting == nil {
		return "", false
	}
	return env.Setting(key)
}

func settingFloat(env *tools.Env, key string) float64 {
	v, ok := skillSetting(env, key)
	if !ok {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f
}

func planRename(env *tools.Env, raw json.RawMessage, write bool) (dir string, p chores.RenamePlan, err error) {
	var a struct{ Folder, Show, Style string }
	arg(raw, &a)
	if dir, err = env.Expand(a.Folder); err != nil {
		return "", p, err
	}
	check := env.Perms.CheckRead
	if write {
		check = env.Perms.CheckWrite
	}
	if dir, err = check(dir); err != nil {
		return "", p, err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return "", p, err
	}
	var names []string
	for _, d := range des {
		if !d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			names = append(names, d.Name())
		}
	}
	return dir, chores.PlanRenames(filepath.Clean(dir), names, chores.RenameOptions{Show: a.Show, Style: a.Style, Sanitize: SanitizeName}), nil
}

func renamePlan(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	dir, p, err := planRename(env, raw, false)
	if err != nil {
		return "", err
	}
	type ex struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	examples := []ex{}
	for i, m := range p.Moves {
		if i == 6 {
			break
		}
		examples = append(examples, ex{filepath.Base(m.From), filepath.Base(m.To)})
	}
	return jsonOut(map[string]any{"folder": dir, "renames": len(p.Moves), "examples": examples, "left_alone_count": len(p.Kept), "left_alone": capKept(p.Kept),
		"next": "show the user a few of the new names, then rename_apply (it asks them first)"})
}

func renameApply(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	dir, p, err := planRename(env, raw, true)
	if err != nil {
		return "", err
	}
	moved, skipped := applyMoves(ctx, env, p.Moves)
	return jsonOut(map[string]any{"folder": dir, "renamed": moved, "skipped": skipped, "undo": "noskill undo <run-id> puts the old names back"})
}

func dupesTrash(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Keep  string
		Paths []string
	}
	arg(raw, &a)
	keep, err := env.Expand(a.Keep)
	if err != nil {
		return "", err
	}
	if keep, err = env.Perms.CheckRead(keep); err != nil {
		return "", err
	}
	done, refused := []string{}, []string{}
	for _, p := range a.Paths {
		full, err := env.Expand(p)
		if err != nil {
			return "", err
		}
		if full, err = env.Perms.CheckWrite(full); err != nil {
			return "", err
		}
		if same, why := chores.SameContent(keep, full); !same {
			refused = append(refused, p+": "+why)
			continue
		}
		if err := env.Broker.TrashPath(ctx, full); err != nil {
			return "", friendly(err)
		}
		done = append(done, full)
	}
	return jsonOut(map[string]any{"kept": keep, "trashed": done, "refused": refused})
}
