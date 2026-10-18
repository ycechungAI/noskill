package perms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"noskill/internal/platform"
	"noskill/internal/store"
)

// UndoLog is where applied file effects are recorded (store.DB in production).
type UndoLog interface {
	AppendUndo(ctx context.Context, e store.UndoEntry) error
}

// ErrExists: the target exists and nothing is ever overwritten (SECURITY.md "Deletion policy").
// Skills with an approved `overwrite: true` arrive with the skill loader.
var ErrExists = errors.New("target already exists")

// Broker performs file effects for built-in tools: each path is checked against Perms first,
// nothing is overwritten, "delete" means the OS trash, and every effect goes to the undo log.
type Broker struct {
	RunID string
	Perms Set
	Trash platform.Trasher
	Undo  UndoLog
}

func (b *Broker) log(ctx context.Context, op, from, to, hash string) error {
	if b.Undo == nil {
		return nil
	}
	return b.Undo.AppendUndo(ctx, store.UndoEntry{RunID: b.RunID, Op: op, From: from, To: to, Hash: hash})
}

func absent(p string) error {
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("%s: %w", p, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Move moves a file or folder. Both ends need write scope. Across filesystems a file is copied,
// verified by hash, and the source goes to the trash (so even that step can be undone).
func (b *Broker) Move(ctx context.Context, from, to string) error {
	src, err := b.Perms.checkInside(from)
	if err != nil {
		return err
	}
	dst, err := b.Perms.CheckWrite(to)
	if err != nil {
		return err
	}
	if err := absent(dst); err != nil {
		return err
	}
	// The window between this check and the rename is a known v1 gap: a no-replace rename isn't
	// portable (renameat2 / renamex_np / MoveFileEx without REPLACE_EXISTING).
	err = os.Rename(src, dst)
	if err == nil {
		return b.log(ctx, "move", src, dst, "")
	}
	if !platform.IsCrossDevice(err) {
		return err
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("moving %s across drives: only files are supported", from)
	}
	if err := b.clearStalePart(ctx, dst); err != nil {
		return err
	}
	sum, err := copyVerified(src, dst)
	if err != nil {
		return err
	}
	if err := b.log(ctx, "copy", src, dst, sum); err != nil {
		return err
	}
	where, err := b.Trash.Trash(src)
	if err != nil {
		return fmt.Errorf("copied to %s but couldn't trash the original: %w", dst, err)
	}
	return b.log(ctx, "trash", src, where, sum)
}

// Copy copies a file. The source needs read scope, the target write scope.
func (b *Broker) Copy(ctx context.Context, from, to string) error {
	src, err := b.Perms.CheckRead(from)
	if err != nil {
		return err
	}
	dst, err := b.Perms.CheckWrite(to)
	if err != nil {
		return err
	}
	if err := absent(dst); err != nil {
		return err
	}
	if err := b.clearStalePart(ctx, dst); err != nil {
		return err
	}
	sum, err := copyVerified(src, dst)
	if err != nil {
		return err
	}
	return b.log(ctx, "copy", src, dst, sum)
}

// TrashPath moves a file or folder to the OS trash. It's the only way a skill deletes anything.
func (b *Broker) TrashPath(ctx context.Context, p string) error {
	src, err := b.Perms.checkInside(p)
	if err != nil {
		return err
	}
	if b.Trash == nil {
		return errors.New("no trash available")
	}
	where, err := b.Trash.Trash(src)
	if err != nil {
		return err
	}
	return b.log(ctx, "trash", src, where, "")
}

// stalePart is how long a ".part" file must have gone without a write to count as a copy that died
// (an unplugged drive, a crash) rather than one in progress.
const stalePart = 5 * time.Minute

var clock = time.Now

// clearStalePart moves the leftover "<dst>.part" of a copy that died to the trash, so the next try
// isn't blocked by it for ever. A fresh one is left alone: copyVerified reports it.
func (b *Broker) clearStalePart(ctx context.Context, dst string) error {
	part := dst + ".part"
	fi, err := os.Lstat(part)
	if err != nil || !fi.Mode().IsRegular() || clock().Sub(fi.ModTime()) < stalePart || b.Trash == nil {
		return nil
	}
	if _, err := b.Perms.CheckWrite(part); err != nil {
		return nil
	}
	where, err := b.Trash.Trash(part)
	if err != nil {
		return fmt.Errorf("a copy to %s died and left %s; couldn't move it to the trash: %w", dst, part, err)
	}
	return b.log(ctx, "trash", part, where, "")
}

// Mkdir creates a folder and its missing parents, logging each one it created.
func (b *Broker) Mkdir(ctx context.Context, p string) error {
	dst, err := b.Perms.CheckWrite(p)
	if err != nil {
		return err
	}
	var missing []string
	for d := dst; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if _, err := b.Perms.CheckWrite(missing[i]); err != nil {
			return fmt.Errorf("%s doesn't exist and is outside the write scope: %w", missing[i], err)
		}
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := b.log(ctx, "mkdir", "", missing[i], ""); err != nil {
			return err
		}
	}
	return nil
}

// WriteFile creates a new file. Existing files are never replaced.
func (b *Broker) WriteFile(ctx context.Context, p string, data []byte) error {
	dst, err := b.Perms.CheckWrite(p)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", dst, ErrExists)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	h := sha256.Sum256(data)
	return b.log(ctx, "create", "", dst, hex.EncodeToString(h[:]))
}

// copyVerified copies src to dst (which must not exist) the careful way: write dst+".part", sync
// it, check its SHA-256 against the source's, then rename it into place. Other programs never see
// a half-written dst. On failure the .part file is left where it is and named in the error: it is
// noskill's own file, but nothing here deletes without the trash.
func copyVerified(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return "", err
	}
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%s: %w (a leftover .part file from an earlier copy)", part, ErrExists)
	}
	if err != nil {
		return "", err
	}
	hs := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hs), in); err != nil {
		out.Close()
		return "", fmt.Errorf("copying to %s: %w (partial copy left at %s)", dst, err, part)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	want := hex.EncodeToString(hs.Sum(nil))
	got, err := hashFile(part)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("copy of %s doesn't match the original (left at %s)", src, part)
	}
	if err := absent(dst); err != nil { // appeared while copying
		return "", fmt.Errorf("%w (the finished copy is at %s)", err, part)
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	return want, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
