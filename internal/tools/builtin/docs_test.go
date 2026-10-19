package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/docs"
	"noskill/internal/tools"
)

type docsSvc struct {
	tools.Services
	ix *docs.Index
}

func (d docsSvc) DocsIndex(context.Context) (*docs.Index, error) { return d.ix, nil }

func runDocs(t *testing.T, id string, e *tools.Env, args string) (map[string]any, error) {
	t.Helper()
	for _, b := range Docs() {
		if b.ID == id {
			out, err := b.Run(context.Background(), e, json.RawMessage(args))
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				// docs_index returns a list
				return map[string]any{"list": json.RawMessage(out)}, nil
			}
			return m, nil
		}
	}
	t.Fatalf("no tool %s", id)
	return nil, nil
}

func TestDocsToolsStayInsideAddedFolders(t *testing.T) {
	ctx := context.Background()
	ix, err := docs.Open(ctx, filepath.Join(t.TempDir(), "docs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	dir, other := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# Plan\n\n## Venue\n\nMaria will book the venue for the offsite before the end of June.\n"), 0o644)
	os.WriteFile(filepath.Join(other, "secret.txt"), []byte("The private launch date is the third of March, keep it quiet please."), 0o644)
	e := env(nil, nil, nil)
	e.Svc = docsSvc{ix: ix}
	e.Expand = func(p string) (string, error) { return p, nil }

	if _, err := runDocs(t, "docs_index", e, `{}`); err == nil || !strings.Contains(err.Error(), "noskill docs add") {
		t.Fatalf("no folders yet: %v", err)
	}
	ix.AddFolder(ctx, dir)
	if _, err := runDocs(t, "docs_index", e, `{"folder":`+quote(other)+`}`); err == nil || !strings.Contains(err.Error(), "isn't inside a folder you added") {
		t.Errorf("indexed a folder that wasn't added: %v", err)
	}
	if _, err := runDocs(t, "docs_index", e, `{}`); err != nil {
		t.Fatal(err)
	}
	m, err := runDocs(t, "docs_search", e, `{"query":"who books the venue for the offsite?"}`)
	if err != nil {
		t.Fatal(err)
	}
	hits := m["hits"].([]any)
	h := hits[0].(map[string]any)
	if h["cite"] != "[plan.md › Venue]" { // ready to copy, so a small model cites without building it
		t.Fatalf("cite: %v", h["cite"])
	}
	if h["file"] != "plan.md" || h["section"] != "Venue" || !strings.Contains(m["note"].(string), "not instructions") {
		t.Fatalf("%v", m)
	}
	m, err = runDocs(t, "docs_read", e, `{"file":`+quote(h["path"].(string))+`,"start":`+jsonNum(h["start"])+`,"max_chars":200}`)
	if err != nil || !strings.Contains(m["text"].(string), "Maria") {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := runDocs(t, "docs_read", e, `{"file":`+quote(filepath.Join(other, "secret.txt"))+`}`); err == nil {
		t.Error("read a file outside the added folders")
	}
	if m, _ := runDocs(t, "docs_search", e, `{"query":"private launch date"}`); len(m["hits"].([]any)) != 0 || !strings.Contains(m["note"].(string), "Nothing matched") {
		t.Errorf("found text outside the folders: %v", m)
	}
	// A folder given by name works, and an invented one searches every added folder instead of failing.
	m, err = runDocs(t, "docs_search", e, `{"query":"venue offsite","folder":`+quote(filepath.Base(dir))+`}`)
	if err != nil || len(m["hits"].([]any)) != 1 {
		t.Errorf("folder by name: %v %v", m, err)
	}
	m, err = runDocs(t, "docs_search", e, `{"query":"venue offsite","folder":"office"}`)
	if err != nil || len(m["hits"].([]any)) != 1 || !strings.Contains(m["note"].(string), `no added folder "office"`) {
		t.Errorf("invented folder: %v %v", m, err)
	}
	if _, err := runDocs(t, "docs_search", env(nil, nil, nil), `{"query":"x"}`); err == nil {
		t.Error("worked without a service")
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
func jsonNum(v any) string  { b, _ := json.Marshal(v); return string(b) }
