package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"noskill/internal/app"
)

// cmdDocs manages the local-docs index (skills/local-docs): adding a folder is the user's grant, and
// the skill's tools reach only files inside the folders added here.
func cmdDocs(c *cmd, args []string) int {
	usage := "usage: noskill docs add <folder> | list | remove <folder> | index [folder] | search \"<question>\" [-k N]"
	if len(args) == 0 {
		args = []string{"list"}
	}
	ctx, stop := c.env.Context()
	defer stop()
	a, err := c.open(ctx)
	if err != nil {
		return c.fail(err)
	}
	defer a.Close()
	ix, err := a.Docs(ctx)
	if err != nil {
		return c.fail(err)
	}
	folderArg := func(s string) (string, error) {
		if !strings.HasPrefix(s, "volume:") && !strings.HasPrefix(s, "$") && !strings.HasPrefix(s, "~") && !filepath.IsAbs(s) {
			abs, err := filepath.Abs(s)
			if err != nil {
				return "", err
			}
			s = abs
		}
		p, err := a.ExpandPath(s)
		if err != nil {
			return "", err
		}
		return filepath.Clean(p), nil
	}
	switch args[0] {
	case "add":
		if len(args) != 2 {
			return c.usageErr(usage)
		}
		p, err := folderArg(args[1])
		if err != nil {
			return c.fail(err)
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return c.fail(fmt.Errorf("%s isn't a folder", p))
		}
		if err := ix.AddFolder(ctx, p); err != nil {
			return c.fail(err)
		}
		c.outf("✓ added %s; reading it now (only text, Markdown, HTML and Word files; PDFs aren't read yet)\n", p)
		return c.indexFolders(ctx, a, []string{p})
	case "remove":
		if len(args) != 2 {
			return c.usageErr(usage)
		}
		p, err := folderArg(args[1])
		if err != nil {
			return c.fail(err)
		}
		n, err := ix.RemoveFolder(ctx, p)
		if err != nil {
			return c.fail(err)
		}
		c.outf("✓ forgot %s and deleted its %d indexed file(s) from docs.db; your files are untouched\n", p, n)
	case "list":
		st, err := ix.Status(ctx)
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(st)
			return ExitOK
		}
		if len(st) == 0 {
			c.outf("No folders added. `noskill docs add ~/Documents/notes` lets noskill answer questions from them.\n")
			return ExitOK
		}
		rows := [][]string{{"FOLDER", "FILES", "PASSAGES", "MEANING", "UPDATED", "LEFT OUT"}}
		for _, s := range st {
			upd := "never"
			if !s.IndexedAt.IsZero() {
				upd = s.IndexedAt.Format("Jan 2 15:04")
			}
			var left []string
			for why, n := range s.Unindexed {
				left = append(left, fmt.Sprintf("%d: %s", n, why))
			}
			rows = append(rows, []string{s.Folder, fmt.Sprint(s.Files), fmt.Sprint(s.Chunks), fmt.Sprintf("%d/%d", s.Vectors, s.Chunks), upd, strings.Join(left, "; ")})
		}
		c.table(rows)
	case "index":
		folders, err := ix.Folders(ctx)
		if err != nil {
			return c.fail(err)
		}
		if len(args) == 2 {
			p, err := folderArg(args[1])
			if err != nil {
				return c.fail(err)
			}
			folders = []string{p}
		}
		if len(folders) == 0 {
			return c.fail(fmt.Errorf("no folders added yet (noskill docs add <folder>)"))
		}
		return c.indexFolders(ctx, a, folders)
	case "search":
		rest := args[1:]
		k := 6
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-k" && i+1 < len(rest) {
				fmt.Sscan(rest[i+1], &k)
				rest = append(rest[:i], rest[i+2:]...)
				break
			}
		}
		q := strings.TrimSpace(strings.Join(rest, " "))
		if q == "" {
			return c.usageErr(usage)
		}
		hits, err := ix.Search(ctx, q, k, "")
		if err != nil {
			return c.fail(err)
		}
		if c.g.json {
			c.printJSON(hits)
			return ExitOK
		}
		if len(hits) == 0 {
			c.outf("Nothing in your folders matches that.\n")
			return ExitOK
		}
		for _, h := range hits {
			where := h.File
			if h.Section != "" {
				where += " › " + h.Section
			}
			text := strings.Join(strings.Fields(h.Text), " ")
			if r := []rune(text); len(r) > 240 {
				text = string(r[:240]) + "…"
			}
			c.outf("%.2f  %s\n      %s\n", h.Score, where, text)
		}
	default:
		return c.usageErr(usage)
	}
	return ExitOK
}

func (c *cmd) indexFolders(ctx context.Context, a *app.App, folders []string) int {
	ix, err := a.Docs(ctx)
	if err != nil {
		return c.fail(err)
	}
	failed := false
	for _, f := range folders {
		st, err := ix.IndexFolder(ctx, f)
		if err != nil {
			c.errf("noskill: %v\n", err)
			failed = true
			continue
		}
		if c.g.json {
			c.printJSON(st)
			continue
		}
		c.outf("%s: %d file(s) read, %d unchanged, %d removed, %d passages\n", f, st.Indexed, st.Unchanged, st.Removed, st.Chunks)
		for why, n := range st.Unindexed {
			c.outf("  left out: %d file(s), %s\n", n, why)
		}
		switch {
		case st.EmbedError != "":
			c.outf("  %s\n", st.EmbedError)
		case ix.EmbedModel == "":
			c.outf("  no embedding model installed: searching by keywords only (`noskill models pull nomic-embed-text` adds search by meaning)\n")
		}
		if st.Truncated {
			c.outf("  stopped at 20000 files; split the folder up\n")
		}
	}
	if failed {
		return ExitError
	}
	return ExitOK
}
