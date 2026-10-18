package docs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"noskill/internal/backends"
)

// schemaVersion changes when the tables do. docs.db holds nothing that can't be rebuilt from the
// folders, so a different version is dropped and re-indexed instead of migrated.
const schemaVersion = 1

// Embedder is the part of a backend the index needs.
type Embedder interface {
	Embed(ctx context.Context, req backends.EmbedRequest) ([][]float32, error)
}

// Index is docs.db. Embedder and EmbedModel are optional: without them search is by keywords only.
type Index struct {
	DB         *sql.DB
	Path       string
	Embedder   Embedder
	EmbedModel string
}

// Open opens (creating or rebuilding) the index at path.
func Open(ctx context.Context, path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	ix := &Index{DB: db, Path: path}
	if err := ix.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return ix, nil
}

func (ix *Index) Close() error { return ix.DB.Close() }

const schema = `
CREATE TABLE folders (path TEXT PRIMARY KEY, added_at INTEGER NOT NULL, indexed_at INTEGER);
CREATE TABLE files (
  id INTEGER PRIMARY KEY, folder TEXT NOT NULL, path TEXT NOT NULL UNIQUE, rel TEXT NOT NULL,
  size INTEGER NOT NULL, mtime INTEGER NOT NULL, hash TEXT, status TEXT NOT NULL, note TEXT, indexed_at INTEGER NOT NULL
);
CREATE INDEX files_folder ON files(folder);
CREATE TABLE chunks (
  id INTEGER PRIMARY KEY, file_id INTEGER NOT NULL, seq INTEGER NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL,
  section TEXT, text TEXT NOT NULL
);
CREATE INDEX chunks_file ON chunks(file_id);
CREATE VIRTUAL TABLE chunks_fts USING fts5(text, content='chunks', content_rowid='id', tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN INSERT INTO chunks_fts(rowid, text) VALUES (new.id, new.text); END;
CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES ('delete', old.id, old.text); END;
CREATE TABLE chunk_vectors (chunk_id INTEGER PRIMARY KEY, embed_model TEXT NOT NULL, vector BLOB NOT NULL);
`

func (ix *Index) init(ctx context.Context) error {
	var v int
	if err := ix.DB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == schemaVersion {
		return nil
	}
	if v != 0 {
		// An older layout: keep the folder list, rebuild the rest.
		rows, _ := ix.DB.QueryContext(ctx, `SELECT path, added_at FROM folders`)
		var keep [][2]any
		if rows != nil {
			for rows.Next() {
				var p string
				var at int64
				if rows.Scan(&p, &at) == nil {
					keep = append(keep, [2]any{p, at})
				}
			}
			rows.Close()
		}
		for _, t := range []string{"chunk_vectors", "chunks_fts", "chunks", "files", "folders"} {
			ix.DB.ExecContext(ctx, `DROP TABLE IF EXISTS `+t)
		}
		defer func() {
			for _, k := range keep {
				ix.DB.ExecContext(ctx, `INSERT OR IGNORE INTO folders (path, added_at) VALUES (?, ?)`, k[0], k[1])
			}
		}()
	}
	if _, err := ix.DB.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("docs.db: %w (this SQLite build needs FTS5)", err)
	}
	_, err := ix.DB.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// ---- folders ----

// Folders lists the folders the user added.
func (ix *Index) Folders(ctx context.Context) ([]string, error) {
	rows, err := ix.DB.QueryContext(ctx, `SELECT path FROM folders ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddFolder registers a folder (an absolute path) so it can be indexed and searched.
func (ix *Index) AddFolder(ctx context.Context, path string) error {
	_, err := ix.DB.ExecContext(ctx, `INSERT OR IGNORE INTO folders (path, added_at) VALUES (?, ?)`, path, time.Now().UnixMilli())
	return err
}

// RemoveFolder forgets a folder and deletes everything indexed from it. The files are not touched.
func (ix *Index) RemoveFolder(ctx context.Context, path string) (int, error) {
	tx, err := ix.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vectors WHERE chunk_id IN (SELECT c.id FROM chunks c JOIN files f ON f.id = c.file_id WHERE f.folder = ?)`, path); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE file_id IN (SELECT id FROM files WHERE folder = ?)`, path); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM files WHERE folder = ?`, path)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE path = ?`, path); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// Within returns the registered folder that contains path (after resolving symbolic links), or "".
// Every read of the user's files goes through it, so nothing outside the added folders is touched.
func (ix *Index) Within(ctx context.Context, path string) (folder string, real string, err error) {
	real, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	folders, err := ix.Folders(ctx)
	if err != nil {
		return "", "", err
	}
	for _, f := range folders {
		rf, err := filepath.EvalSymlinks(f)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rf, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return f, real, nil
		}
	}
	return "", real, fmt.Errorf("%s isn't inside a folder you added (noskill docs add <folder>)", path)
}

// ---- indexing ----

// Stats is what an indexing pass did.
type Stats struct {
	Folder     string         `json:"folder"`
	Seen       int            `json:"files_seen"`
	Indexed    int            `json:"indexed"`
	Unchanged  int            `json:"unchanged"`
	Removed    int            `json:"removed"`
	Chunks     int            `json:"chunks"`
	Embedded   int            `json:"embedded"`
	Unindexed  map[string]int `json:"unindexed,omitempty"` // reason → files
	EmbedError string         `json:"embed_error,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"` // stopped at the file limit
}

// maxFiles bounds one pass over a folder.
const maxFiles = 20000

var skipDirs = map[string]bool{"node_modules": true, ".git": true, "$recycle.bin": true, "system volume information": true, "lost+found": true}

// IndexFolder brings the index up to date with a registered folder: new and changed files are read
// and chunked, files that vanished are dropped. Unchanged files (same size and modification time)
// aren't opened. Hidden folders and symbolic links are skipped.
func (ix *Index) IndexFolder(ctx context.Context, folder string) (Stats, error) {
	st := Stats{Folder: folder, Unindexed: map[string]int{}}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return st, fmt.Errorf("%s isn't a folder that can be read right now (is the drive plugged in?)", folder)
	}
	known := map[string]fileRow{}
	rows, err := ix.DB.QueryContext(ctx, `SELECT id, path, size, mtime, coalesce(hash, ''), status FROM files WHERE folder = ?`, folder)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var r fileRow
		if err := rows.Scan(&r.id, &r.path, &r.size, &r.mtime, &r.hash, &r.status); err != nil {
			rows.Close()
			return st, err
		}
		known[r.path] = r
	}
	rows.Close()

	present := map[string]bool{}
	walkErr := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // an unreadable entry is skipped
		}
		if d.IsDir() {
			if p != folder && (strings.HasPrefix(d.Name(), ".") || skipDirs[strings.ToLower(d.Name())]) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "~$") || !Supported(d.Name()) {
			return nil
		}
		if st.Seen >= maxFiles {
			st.Truncated = true
			return filepath.SkipAll
		}
		st.Seen++
		present[p] = true
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if k, ok := known[p]; ok && k.size == info.Size() && k.mtime == info.ModTime().UnixMilli() {
			st.Unchanged++
			if k.status != "indexed" {
				st.Unindexed[ix.noteOf(ctx, k.id)]++
			}
			return nil
		}
		return ix.indexFile(ctx, folder, p, info, known[p], &st)
	})
	if walkErr != nil {
		return st, walkErr
	}
	// Files that are gone from the folder leave the index (only when the whole folder was seen).
	if !st.Truncated {
		for p, k := range known {
			if !present[p] {
				if err := ix.dropFile(ctx, k.id); err != nil {
					return st, err
				}
				st.Removed++
			}
		}
	}
	ix.DB.ExecContext(ctx, `UPDATE folders SET indexed_at = ? WHERE path = ?`, time.Now().UnixMilli(), folder)
	st.Embedded, st.EmbedError = ix.embedMissing(ctx, folder)
	_ = ix.DB.QueryRowContext(ctx, `SELECT count(*) FROM chunks c JOIN files f ON f.id = c.file_id WHERE f.folder = ?`, folder).Scan(&st.Chunks)
	if len(st.Unindexed) == 0 {
		st.Unindexed = nil
	}
	return st, nil
}

type fileRow struct {
	id          int64
	path, hash  string
	size, mtime int64
	status      string
}

// noteOf says why a file was left out, for the summary of a pass that didn't re-read it.
func (ix *Index) noteOf(ctx context.Context, id int64) string {
	var n sql.NullString
	ix.DB.QueryRowContext(ctx, `SELECT note FROM files WHERE id = ?`, id).Scan(&n)
	if n.String == "" {
		return "unindexed"
	}
	return n.String
}

func (ix *Index) indexFile(ctx context.Context, folder, path string, info fs.FileInfo, old fileRow, st *Stats) error {
	rel, _ := filepath.Rel(folder, path)
	now := time.Now().UnixMilli()
	setStatus := func(status, note, hash string) error {
		if old.id != 0 {
			if err := ix.dropChunks(ctx, old.id); err != nil {
				return err
			}
			_, err := ix.DB.ExecContext(ctx, `UPDATE files SET size=?, mtime=?, hash=?, status=?, note=?, indexed_at=? WHERE id=?`,
				info.Size(), info.ModTime().UnixMilli(), nullable(hash), status, nullable(note), now, old.id)
			return err
		}
		_, err := ix.DB.ExecContext(ctx, `INSERT INTO files (folder, path, rel, size, mtime, hash, status, note, indexed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			folder, path, filepath.ToSlash(rel), info.Size(), info.ModTime().UnixMilli(), nullable(hash), status, nullable(note), now)
		return err
	}
	sum := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	text, err := Extract(path)
	var un ErrUnindexed
	switch {
	case errors.As(err, &un):
		st.Unindexed[un.Reason]++
		return setStatus("unindexed", un.Reason, "")
	case err != nil:
		st.Unindexed["couldn't be read"]++
		return setStatus("unindexed", "couldn't be read", "")
	}
	h := sum()
	if old.id != 0 && old.hash != "" && old.hash == h && old.status == "indexed" {
		// Touched but not changed: keep the chunks, remember the new modification time.
		st.Unchanged++
		_, err := ix.DB.ExecContext(ctx, `UPDATE files SET size=?, mtime=? WHERE id=?`, info.Size(), info.ModTime().UnixMilli(), old.id)
		return err
	}
	chunks := Split(text, isMarkdown(path))
	if len(chunks) == 0 {
		st.Unindexed["no text found (empty, or a scan)"]++
		return setStatus("unindexed", "no text found (empty, or a scan)", "")
	}
	tx, err := ix.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if old.id != 0 {
		id = old.id
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vectors WHERE chunk_id IN (SELECT id FROM chunks WHERE file_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE file_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET size=?, mtime=?, hash=?, status='indexed', note=NULL, indexed_at=? WHERE id=?`,
			info.Size(), info.ModTime().UnixMilli(), h, now, id); err != nil {
			return err
		}
	} else {
		res, err := tx.ExecContext(ctx, `INSERT INTO files (folder, path, rel, size, mtime, hash, status, indexed_at) VALUES (?, ?, ?, ?, ?, ?, 'indexed', ?)`,
			folder, path, filepath.ToSlash(rel), info.Size(), info.ModTime().UnixMilli(), h, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
	}
	for i, c := range chunks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks (file_id, seq, start, end, section, text) VALUES (?, ?, ?, ?, ?, ?)`,
			id, i, c.Start, c.End, nullable(c.Section), c.Text); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	st.Indexed++
	return nil
}

func isMarkdown(path string) bool {
	e := strings.ToLower(filepath.Ext(path))
	return e == ".md" || e == ".markdown"
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (ix *Index) dropChunks(ctx context.Context, fileID int64) error {
	if _, err := ix.DB.ExecContext(ctx, `DELETE FROM chunk_vectors WHERE chunk_id IN (SELECT id FROM chunks WHERE file_id = ?)`, fileID); err != nil {
		return err
	}
	_, err := ix.DB.ExecContext(ctx, `DELETE FROM chunks WHERE file_id = ?`, fileID)
	return err
}

func (ix *Index) dropFile(ctx context.Context, id int64) error {
	if err := ix.dropChunks(ctx, id); err != nil {
		return err
	}
	_, err := ix.DB.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	return err
}

// ---- embeddings ----

const embedBatch = 16
const embedMax = 4000 // chunks embedded per pass; the rest wait for the next one

// embedMissing embeds the folder's chunks that have no vector for the current embedding model. A
// failure (the model isn't installed or is busy) leaves keyword search working and is reported.
func (ix *Index) embedMissing(ctx context.Context, folder string) (int, string) {
	if ix.Embedder == nil || ix.EmbedModel == "" {
		return 0, ""
	}
	rows, err := ix.DB.QueryContext(ctx, `SELECT c.id, c.text FROM chunks c JOIN files f ON f.id = c.file_id
		LEFT JOIN chunk_vectors v ON v.chunk_id = c.id AND v.embed_model = ?
		WHERE f.folder = ? AND v.chunk_id IS NULL ORDER BY c.id LIMIT ?`, ix.EmbedModel, folder, embedMax)
	if err != nil {
		return 0, err.Error()
	}
	type item struct {
		id   int64
		text string
	}
	var todo []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.text) == nil {
			todo = append(todo, it)
		}
	}
	rows.Close()
	done := 0
	for i := 0; i < len(todo); i += embedBatch {
		if ctx.Err() != nil {
			return done, ctx.Err().Error()
		}
		batch := todo[i:min(i+embedBatch, len(todo))]
		in := make([]string, len(batch))
		for j, b := range batch {
			in[j] = b.text
		}
		vecs, err := ix.Embedder.Embed(ctx, backends.EmbedRequest{Model: ix.EmbedModel, Input: in})
		if err != nil || len(vecs) != len(batch) {
			if err == nil {
				err = errors.New("the embedding model returned the wrong number of vectors")
			}
			return done, "meaning-based search is off for now: " + err.Error()
		}
		tx, err := ix.DB.BeginTx(ctx, nil)
		if err != nil {
			return done, err.Error()
		}
		for j, b := range batch {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO chunk_vectors (chunk_id, embed_model, vector) VALUES (?, ?, ?)`, b.id, ix.EmbedModel, encode(vecs[j])); err != nil {
				tx.Rollback()
				return done, err.Error()
			}
		}
		if err := tx.Commit(); err != nil {
			return done, err.Error()
		}
		done += len(batch)
	}
	return done, ""
}

// encode writes a vector as little-endian float32 (DATA_MODEL.md).
func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decode(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---- status ----

// FolderStatus summarises what is indexed for a folder.
type FolderStatus struct {
	Folder    string         `json:"folder"`
	Files     int            `json:"files"`
	Chunks    int            `json:"chunks"`
	Vectors   int            `json:"with_vectors"`
	Unindexed map[string]int `json:"unindexed,omitempty"`
	IndexedAt time.Time      `json:"indexed_at,omitzero"`
}

func (ix *Index) Status(ctx context.Context) ([]FolderStatus, error) {
	folders, err := ix.Folders(ctx)
	if err != nil {
		return nil, err
	}
	var out []FolderStatus
	for _, f := range folders {
		s := FolderStatus{Folder: f}
		var at sql.NullInt64
		ix.DB.QueryRowContext(ctx, `SELECT indexed_at FROM folders WHERE path = ?`, f).Scan(&at)
		if at.Valid {
			s.IndexedAt = time.UnixMilli(at.Int64)
		}
		ix.DB.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE folder = ? AND status = 'indexed'`, f).Scan(&s.Files)
		ix.DB.QueryRowContext(ctx, `SELECT count(*) FROM chunks c JOIN files x ON x.id = c.file_id WHERE x.folder = ?`, f).Scan(&s.Chunks)
		ix.DB.QueryRowContext(ctx, `SELECT count(*) FROM chunk_vectors v JOIN chunks c ON c.id = v.chunk_id JOIN files x ON x.id = c.file_id WHERE x.folder = ?`, f).Scan(&s.Vectors)
		rows, err := ix.DB.QueryContext(ctx, `SELECT coalesce(note, 'unindexed'), count(*) FROM files WHERE folder = ? AND status != 'indexed' GROUP BY 1`, f)
		if err == nil {
			for rows.Next() {
				var n string
				var c int
				if rows.Scan(&n, &c) == nil {
					if s.Unindexed == nil {
						s.Unindexed = map[string]int{}
					}
					s.Unindexed[n] = c
				}
			}
			rows.Close()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Folder < out[j].Folder })
	return out, nil
}
