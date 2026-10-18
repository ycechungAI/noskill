package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"noskill/internal/platform"
	"noskill/internal/tools"
)

// Files returns the `files` skill's tools. Every path is expanded (~, $DOWNLOADS, volume:…) and
// then checked against the calling skill's permissions by the broker; the tools never touch a
// path the check didn't pass (SECURITY.md). Nothing is overwritten and nothing is hard-deleted.
func Files() []tools.Builtin {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }
	return []tools.Builtin{
		{ID: "list_dir", Description: "List a folder: name, type, size and modified time for each entry.",
			Schema: obj(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"glob":{"type":"string","description":"Name pattern such as *.mkv"},"recursive":{"type":"boolean","default":false},"max_entries":{"type":"integer","default":200,"minimum":1,"maximum":1000}}}`),
			Run:    listDir},
		{ID: "read_text", Description: "Read a text file.",
			Schema: obj(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"max_chars":{"type":"integer","default":6000,"minimum":100,"maximum":50000}}}`),
			Run:    readText},
		{ID: "write_text", Description: "Create a new text file. Refuses to replace an existing file.",
			Schema: obj(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
			Run:    writeText},
		{ID: "join_files", Description: "Make a new text file by joining existing text files in the order given, with a blank line between them. The sources are kept. Refuses to replace an existing file.",
			Schema: obj(`{"type":"object","required":["from","to"],"properties":{"from":{"type":"array","minItems":1,"maxItems":40,"items":{"type":"string"}},"to":{"type":"string"}}}`),
			Run:    joinFiles},
		{ID: "move", Description: "Move or rename one file or folder. Refuses to replace an existing one.",
			Schema: obj(`{"type":"object","required":["from","to"],"properties":{"from":{"type":"string"},"to":{"type":"string"}}}`),
			Run:    moveOne},
		{ID: "move_many", Description: "Move or rename many files. Call with dry_run true first to check the plan, then with dry_run false to apply it.",
			Schema: obj(`{"type":"object","required":["plan"],"properties":{"plan":{"type":"array","minItems":1,"maxItems":500,"items":{"type":"object","required":["from","to"],"properties":{"from":{"type":"string"},"to":{"type":"string"}}}},"dry_run":{"type":"boolean","default":true}}}`),
			Run:    moveMany},
		{ID: "copy_verify", Description: "Copy a file, check the copy against the original, then put it in place. The original stays. Never replaces an existing file.",
			Schema: obj(`{"type":"object","required":["from","to"],"properties":{"from":{"type":"string"},"to":{"type":"string"}}}`),
			Run:    copyVerify},
		{ID: "make_dir", Description: "Create a folder, and any missing parent folders inside the allowed area.",
			Schema: obj(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
			Run:    makeDir},
		{ID: "trash", Description: "Move a file or folder to the OS trash. Nothing is ever deleted for good.",
			Schema: obj(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
			Run:    trash},
		{ID: "volumes", Description: "List mounted drives: label, mount path, free space, file system.",
			Run: volumes},
		{ID: "sanitize_name", Description: "Make a file name safe for the drive it's going to (removes characters that file system rejects).",
			Schema: obj(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"target_fs":{"type":"string","enum":["auto","apfs","ext4","ntfs","exfat","fat32"],"default":"auto"}}}`),
			Run:    sanitizeName},
	}
}

func jsonOut(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func arg(raw json.RawMessage, v any) error { return json.Unmarshal(raw, v) }

func listDir(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Path       string
		Glob       string
		Recursive  bool
		MaxEntries int `json:"max_entries"`
	}
	arg(raw, &a)
	if a.MaxEntries <= 0 {
		a.MaxEntries = 200
	}
	root, err := env.Expand(a.Path)
	if err != nil {
		return "", err
	}
	root, err = env.Perms.CheckRead(root)
	if err != nil {
		return "", err
	}
	if a.Glob != "" {
		if _, err := path.Match(a.Glob, "x"); err != nil {
			return "", fmt.Errorf("glob %q: %v", a.Glob, err)
		}
	}
	type entry struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Size     int64  `json:"size,omitempty"`
		Modified string `json:"modified,omitempty"`
	}
	var out []entry
	more := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable entries are skipped
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(rel, string(filepath.Separator))
		match := a.Glob == "" || globMatch(a.Glob, d.Name())
		if match {
			if len(out) >= a.MaxEntries {
				more = true
				return fs.SkipAll
			}
			e := entry{Name: filepath.ToSlash(rel), Type: "file"}
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				e.Type = "link" // never followed: it could lead out of the allowed folders
			case d.IsDir():
				e.Type = "folder"
			}
			if fi, err := d.Info(); err == nil {
				e.Modified = fi.ModTime().UTC().Format(time.RFC3339)
				if e.Type == "file" {
					e.Size = fi.Size()
				}
			}
			out = append(out, e)
		}
		if d.IsDir() && (!a.Recursive || depth >= 4) {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("couldn't list %s: %w", a.Path, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return jsonOut(map[string]any{"path": root, "entries": out, "more": more})
}

func globMatch(pattern, name string) bool {
	ok, _ := path.Match(strings.ToLower(pattern), strings.ToLower(name))
	return ok
}

func readText(_ context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Path     string
		MaxChars int `json:"max_chars"`
	}
	arg(raw, &a)
	if a.MaxChars <= 0 {
		a.MaxChars = 6000
	}
	p, err := env.Expand(a.Path)
	if err != nil {
		return "", err
	}
	p, err = env.Perms.CheckRead(p)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a folder; use list_dir", a.Path)
	}
	// Up to 4 bytes per character, plus one to know whether there's more.
	buf, err := io.ReadAll(io.LimitReader(f, int64(a.MaxChars)*4+1))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(buf[:min(len(buf), 8192)], 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file, not text", a.Path)
	}
	text := strings.ToValidUTF8(string(buf), "")
	truncated := fi.Size() > int64(len(buf))
	if r := []rune(text); len(r) > a.MaxChars {
		text, truncated = string(r[:a.MaxChars]), true
	}
	return jsonOut(map[string]any{"path": p, "text": text, "truncated": truncated, "size": fi.Size()})
}

func writeText(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct{ Path, Content string }
	arg(raw, &a)
	if !utf8.ValidString(a.Content) {
		return "", errors.New("content isn't valid UTF-8 text")
	}
	p, err := env.Expand(a.Path)
	if err != nil {
		return "", err
	}
	if err := env.Broker.WriteFile(ctx, p, []byte(a.Content)); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"written": p, "bytes": len(a.Content)})
}

// joinFiles writes one new file made of several text files (a report from its sections). Each
// source needs read scope, the target write scope; nothing is replaced.
func joinFiles(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		From []string
		To   string
	}
	arg(raw, &a)
	if len(a.From) == 0 || a.To == "" {
		return "", errors.New(`join_files needs "from" (a list of files) and "to"`)
	}
	const maxEach, maxAll = 1 << 20, 4 << 20
	var parts []string
	total := 0
	for _, f := range a.From {
		p, err := env.Expand(f)
		if err != nil {
			return "", err
		}
		if p, err = env.Perms.CheckRead(p); err != nil {
			return "", err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", friendly(err)
		}
		if len(b) > maxEach || !utf8.Valid(b) {
			return "", fmt.Errorf("%s isn't a text file of a size to join (1 MB at most)", f)
		}
		if total += len(b); total > maxAll {
			return "", errors.New("the files are too big to join (4 MB at most)")
		}
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	to, err := env.Expand(a.To)
	if err != nil {
		return "", err
	}
	out := strings.Join(parts, "\n\n") + "\n"
	if err := env.Broker.WriteFile(ctx, to, []byte(out)); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"written": to, "bytes": len(out), "files": len(a.From)})
}

// friendly rewrites broker errors for the model: say what to do, not what happened internally.
func friendly(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%v (check the path with list_dir)", err)
	}
	if strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("%v: choose another name; existing files are never replaced", err)
	}
	return err
}

func makeDir(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct{ Path string }
	arg(raw, &a)
	p, err := env.Expand(a.Path)
	if err != nil {
		return "", err
	}
	if err := env.Broker.Mkdir(ctx, p); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"folder": p})
}

// moveInto moves from to to, creating the target's parent folders first (inside the write scope,
// logged for undo), so a plan can move files into folders that don't exist yet.
func moveInto(ctx context.Context, env *tools.Env, from, to string) error {
	if err := env.Broker.Mkdir(ctx, filepath.Dir(to)); err != nil {
		return err
	}
	return env.Broker.Move(ctx, from, to)
}

func moveOne(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct{ From, To string }
	arg(raw, &a)
	from, to, err := expand2(env, a.From, a.To)
	if err != nil {
		return "", err
	}
	if err := moveInto(ctx, env, from, to); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"moved": from, "to": to})
}

func expand2(env *tools.Env, a, b string) (string, string, error) {
	x, err := env.Expand(a)
	if err != nil {
		return "", "", err
	}
	y, err := env.Expand(b)
	if err != nil {
		return "", "", err
	}
	return x, y, nil
}

type moveItem struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func moveMany(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Plan   []moveItem
		DryRun bool `json:"dry_run"`
	}
	arg(raw, &a)
	type row struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Status string `json:"status"` // ok | would_move | moved | skipped
		Reason string `json:"reason,omitempty"`
	}
	rows := make([]row, len(a.Plan))
	targets := map[string]int{}
	sources := map[string]bool{}
	for i, it := range a.Plan {
		rows[i] = row{From: it.From, To: it.To}
		from, to, err := expand2(env, it.From, it.To)
		if err != nil {
			rows[i].Status, rows[i].Reason = "skipped", err.Error()
			continue
		}
		rows[i].From, rows[i].To = from, to
		cf, err := env.Perms.CheckWrite(from)
		var ct string
		if err == nil {
			ct, err = env.Perms.CheckWrite(to)
		}
		if err == nil {
			if _, serr := os.Lstat(cf); serr != nil {
				err = fmt.Errorf("source doesn't exist")
			} else if _, serr := os.Lstat(ct); serr == nil {
				err = fmt.Errorf("target already exists")
			} else if j, dup := targets[ct]; dup {
				err = fmt.Errorf("same target as item %d", j+1)
			} else if cf == ct {
				err = fmt.Errorf("source and target are the same")
			}
		}
		if err != nil {
			rows[i].Status, rows[i].Reason = "skipped", err.Error()
			continue
		}
		targets[ct] = i
		sources[cf] = true
		rows[i].Status = "ok"
	}
	for i := range rows {
		if rows[i].Status != "ok" {
			continue
		}
		// A target inside a folder this plan also moves would end up in the wrong place.
		ct, _ := env.Perms.CheckWrite(rows[i].To)
		for src := range sources {
			if ct != src && strings.HasPrefix(ct, src+string(filepath.Separator)) {
				rows[i].Status, rows[i].Reason = "skipped", "target is inside a folder this plan moves"
			}
		}
	}
	counts := map[string]int{}
	for i := range rows {
		if rows[i].Status == "ok" && !a.DryRun {
			if err := moveInto(ctx, env, rows[i].From, rows[i].To); err != nil {
				rows[i].Status, rows[i].Reason = "skipped", friendly(err).Error()
			} else {
				rows[i].Status = "moved"
			}
		} else if rows[i].Status == "ok" {
			rows[i].Status = "would_move"
		}
		counts[rows[i].Status]++
	}
	return jsonOut(map[string]any{"dry_run": a.DryRun, "summary": counts, "items": rows})
}

func copyVerify(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct{ From, To string }
	arg(raw, &a)
	from, to, err := expand2(env, a.From, a.To)
	if err != nil {
		return "", err
	}
	if err := env.Broker.Copy(ctx, from, to); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"copied": from, "to": to, "verified": true})
}

func trash(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
	var a struct{ Path string }
	arg(raw, &a)
	p, err := env.Expand(a.Path)
	if err != nil {
		return "", err
	}
	if err := env.Broker.TrashPath(ctx, p); err != nil {
		return "", friendly(err)
	}
	return jsonOut(map[string]any{"trashed": p})
}

func volumes(context.Context, *tools.Env, json.RawMessage) (string, error) {
	vs, err := platform.ListVolumes()
	if err != nil {
		return "", err
	}
	if vs == nil {
		vs = []platform.VolumeInfo{}
	}
	return jsonOut(vs)
}

// ---- sanitize_name ----

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

func sanitizeName(_ context.Context, _ *tools.Env, raw json.RawMessage) (string, error) {
	var a struct {
		Name     string
		TargetFS string `json:"target_fs"`
	}
	arg(raw, &a)
	name := SanitizeName(a.Name, a.TargetFS)
	return jsonOut(map[string]any{"name": name, "changed": name != a.Name})
}

// SanitizeName makes a file name valid for target_fs. "auto" (and unknown values) use the
// strictest rules, so the name works on any drive it might end up on.
func SanitizeName(name, targetFS string) string {
	strict := targetFS != "apfs" && targetFS != "ext4"
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == 0:
			b.WriteRune('-')
		case r < 0x20 || r == 0x7f:
			// dropped
		case strict && strings.ContainsRune(`<>:"\|?*`, r):
			if r == ':' {
				b.WriteString(" -")
			} else {
				b.WriteRune('_')
			}
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if strict {
		out = strings.TrimRight(out, ". ")
		base, ext, _ := strings.Cut(out, ".")
		if reserved[strings.ToUpper(base)] {
			out = "_" + base + func() string {
				if ext != "" {
					return "." + ext
				}
				return ""
			}()
		}
	}
	out = strings.TrimLeft(out, " ")
	if out == "" || out == "." || out == ".." {
		out = "_"
	}
	// 255 bytes is the common limit; cut on a rune boundary and keep the extension.
	for len(out) > 255 {
		ext := filepath.Ext(out)
		stem := strings.TrimSuffix(out, ext)
		if len(ext) > 20 {
			ext = ""
		}
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
		out = stem + ext
	}
	return out
}
