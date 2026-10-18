package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"noskill/internal/perms"
	"noskill/internal/skills"
)

// File ops from a script (LEARNING.md §3 pass d, ADR-0013). A script tool works on a read-only copy
// of the folders it was given, so it can't move or trash anything itself. Instead it lists what to
// change, and the broker applies each change to the real folder under the skill's permissions:
// nothing is overwritten, "delete" is the OS trash, and every effect goes to the undo log.
//
//	{"ops": [{"op": "make_dir", "path": "in/docs/old"},
//	         {"op": "move",  "from": "in/docs/a.txt", "to": "in/docs/old/a.txt"},
//	         {"op": "copy",  "from": "in/docs/b.md",  "to": "in/docs/backup/b.md"},
//	         {"op": "write", "path": "in/docs/all.txt", "text": "..."},
//	         {"op": "trash", "path": "in/docs/c.tmp"}],
//	 "count": 3}
//
// Paths name the copies in in/ ("in/<name>/..."), which map back to the real files the arguments
// named. Anything else in the result is passed on to the model.

// Op is one file change a script asks for.
type Op struct {
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Text string `json:"text,omitempty"`
}

const (
	maxOps       = 500
	maxWriteText = 4 << 20
)

// applyOps runs a script's ops through the broker (or, on a dry run, only maps and checks them)
// and returns the result for the model.
func (e *Executor) applyOps(ctx context.Context, set perms.Set, obj map[string]json.RawMessage, t skills.Tool, inputs map[string]string, dryRun bool) (string, error) {
	var ops []Op
	if err := json.Unmarshal(obj["ops"], &ops); err != nil {
		return "", fmt.Errorf("tool %s: \"ops\" must be a list of {op, path | from, to}", t.Name)
	}
	if len(ops) > maxOps {
		return "", fmt.Errorf("tool %s asked for %d file changes; at most %d at once", t.Name, len(ops), maxOps)
	}
	br := &perms.Broker{RunID: e.RunID, Perms: set, Trash: e.Trash, Undo: e.Undo}
	var done, failed []string
	for _, op := range ops {
		desc, err := e.applyOp(ctx, br, op, inputs, dryRun)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", orOp(desc, op), err))
			continue
		}
		done = append(done, desc)
	}
	delete(obj, "ops")
	key := "done"
	if dryRun {
		key = "would_do"
		obj["dry_run"] = json.RawMessage("true")
	}
	if done == nil {
		done = []string{}
	}
	obj[key], _ = json.Marshal(done)
	if len(failed) > 0 {
		obj["failed"], _ = json.Marshal(failed)
	}
	b, _ := json.Marshal(obj)
	return string(b), nil
}

func orOp(desc string, op Op) string {
	if desc != "" {
		return desc
	}
	return op.Op
}

// applyOp maps one op's paths to the real ones and applies it. It returns a short description
// ("move a.txt → old/a.txt", relative to the input folder).
func (e *Executor) applyOp(ctx context.Context, br *perms.Broker, op Op, inputs map[string]string, dryRun bool) (string, error) {
	real := func(p string) (string, string, error) { return realPath(p, inputs) }
	switch op.Op {
	case "make_dir", "trash", "write":
		p, short, err := real(op.Path)
		if err != nil {
			return "", err
		}
		desc := map[string]string{"make_dir": "make folder ", "trash": "trash ", "write": "write "}[op.Op] + short
		if op.Op == "write" {
			if len(op.Text) > maxWriteText || !utf8.ValidString(op.Text) {
				return desc, errors.New("the text is too long or isn't valid UTF-8")
			}
		}
		if _, err := br.Perms.CheckWrite(p); err != nil {
			return desc, err
		}
		if dryRun {
			return desc, nil
		}
		switch op.Op {
		case "make_dir":
			return desc, br.Mkdir(ctx, p)
		case "trash":
			return desc, br.TrashPath(ctx, p)
		}
		if err := br.Mkdir(ctx, filepath.Dir(p)); err != nil {
			return desc, err
		}
		return desc, br.WriteFile(ctx, p, []byte(op.Text))
	case "move", "copy":
		from, sf, err := real(op.From)
		if err != nil {
			return "", err
		}
		to, st, err := real(op.To)
		if err != nil {
			return "", err
		}
		desc := op.Op + " " + sf + " → " + st
		if op.Op == "move" {
			_, err = br.Perms.CheckWrite(from)
		} else {
			_, err = br.Perms.CheckRead(from)
		}
		if err == nil {
			_, err = br.Perms.CheckWrite(to)
		}
		if err != nil || dryRun {
			return desc, err
		}
		if err := br.Mkdir(ctx, filepath.Dir(to)); err != nil {
			return desc, err
		}
		if op.Op == "move" {
			return desc, br.Move(ctx, from, to)
		}
		return desc, br.Copy(ctx, from, to)
	}
	return "", fmt.Errorf("unknown op %q (make_dir, move, copy, write or trash)", op.Op)
}

// realPath maps "in/<name>/rest" (also "/in/…" and "./in/…") to the real file behind the input
// <name>, refusing anything that leaves it. short is the path shown to the model: "<name>/rest".
func realPath(p string, inputs map[string]string) (real, short string, err error) {
	s := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(p), "./"), "/")
	rest, ok := strings.CutPrefix(s, "in/")
	if !ok {
		return "", "", fmt.Errorf("path %q isn't inside in/: name the copy of an input", p)
	}
	name, sub, _ := strings.Cut(rest, "/")
	root, ok := inputs[name]
	if !ok {
		return "", "", fmt.Errorf("path %q: no input named %q", p, name)
	}
	sub = filepath.Clean(filepath.FromSlash(sub))
	if sub == "." || sub == "" {
		return root, name, nil
	}
	if filepath.IsAbs(sub) || sub == ".." || strings.HasPrefix(sub, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q leaves its input", p)
	}
	return filepath.Join(root, sub), name + "/" + filepath.ToSlash(sub), nil
}
