// Package volumes resolves `volume:<label>` to a mounted drive and binds each label to the drive's
// UUID on first use (PLATFORMS.md "External drives", SECURITY.md "volume: label spoofing"). After
// that, another drive with the same label is refused: a stick called RED can't receive files meant
// for the user's RED drive.
package volumes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"noskill/internal/platform"
	"noskill/internal/store"
)

// Binder implements platform.Volumes over the volumes table.
type Binder struct {
	DB   *store.DB
	List func() ([]platform.VolumeInfo, error) // default platform.ListVolumes
	UUID func(mount string) (string, error)    // default platform.VolumeUUID
	Now  func() time.Time
}

func (b *Binder) list() ([]platform.VolumeInfo, error) {
	if b.List != nil {
		return b.List()
	}
	return platform.ListVolumes()
}

// uuid is the drive's UUID in upper case (Linux lists them in lower case, macOS in upper).
func (b *Binder) uuid(mount string) (string, error) {
	f := b.UUID
	if f == nil {
		f = platform.VolumeUUID
	}
	u, err := f(mount)
	return strings.ToUpper(u), err
}

func (b *Binder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Bound returns the UUID label is bound to, or "".
func (b *Binder) Bound(ctx context.Context, label string) (string, error) {
	var u string
	err := b.DB.QueryRowContext(ctx, `SELECT uuid FROM volumes WHERE label = ? ORDER BY last_seen_at DESC LIMIT 1`, label).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return u, err
}

// Bindings returns every label → UUID binding (for perms.Resolver.VolumeUUIDs).
func (b *Binder) Bindings(ctx context.Context) (map[string]string, error) {
	rows, err := b.DB.QueryContext(ctx, `SELECT label, uuid FROM volumes ORDER BY last_seen_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var l, u string
		if err := rows.Scan(&l, &u); err != nil {
			return nil, err
		}
		out[l] = u // newest wins
	}
	return out, rows.Err()
}

// Mount finds the drive for label: the one with the bound UUID, or on first use the only drive with
// that label, which then gets bound. want overrides the stored binding when set.
func (b *Binder) Mount(ctx context.Context, label, want string) (platform.VolumeInfo, error) {
	vs, err := b.list()
	if err != nil {
		return platform.VolumeInfo{}, err
	}
	var same []platform.VolumeInfo
	for _, v := range vs {
		if v.Label == label {
			same = append(same, v)
		}
	}
	if len(same) == 0 {
		return platform.VolumeInfo{}, fmt.Errorf("%w: %s", platform.ErrVolumeNotMounted, label)
	}
	if want == "" {
		if want, err = b.Bound(ctx, label); err != nil {
			return platform.VolumeInfo{}, err
		}
	}
	for i := range same {
		same[i].UUID, _ = b.uuid(same[i].Mount)
	}
	if want != "" {
		for _, v := range same {
			if strings.EqualFold(v.UUID, want) {
				b.seen(ctx, v)
				return v, nil
			}
		}
		return platform.VolumeInfo{}, fmt.Errorf("%w: a drive called %s is attached, but not the one noskill knows (UUID %s); `noskill volumes forget %s` if you replaced it",
			platform.ErrVolumeNotMounted, label, want, label)
	}
	if len(same) > 1 {
		return platform.VolumeInfo{}, fmt.Errorf("%d drives are called %s; rename one, or attach only the one to use the first time", len(same), label)
	}
	b.seen(ctx, same[0])
	return same[0], nil
}

// seen records the drive and binds its label (only drives with a UUID can be bound).
func (b *Binder) seen(ctx context.Context, v platform.VolumeInfo) {
	if v.UUID == "" {
		return
	}
	b.DB.ExecContext(ctx, `INSERT INTO volumes (label, uuid, fs_type, last_mount_path, last_seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET label = excluded.label, fs_type = excluded.fs_type, last_mount_path = excluded.last_mount_path, last_seen_at = excluded.last_seen_at`,
		v.Label, v.UUID, v.FSType, v.Mount, b.now().UnixMilli())
}

// Resolve implements platform.Volumes.
func (b *Binder) Resolve(label, uuid string) (string, string, error) {
	v, err := b.Mount(context.Background(), label, uuid)
	if err != nil {
		return "", "", err
	}
	return v.Mount, v.UUID, nil
}

// Known is one row of the volumes table.
type Known struct {
	Label    string    `json:"label"`
	UUID     string    `json:"uuid"`
	FSType   string    `json:"filesystem,omitempty"`
	LastPath string    `json:"last_mount_path,omitempty"`
	LastSeen time.Time `json:"last_seen_at,omitzero"`
}

// Known returns the bound drives.
func (b *Binder) Known(ctx context.Context) ([]Known, error) {
	rows, err := b.DB.QueryContext(ctx, `SELECT label, uuid, COALESCE(fs_type, ''), COALESCE(last_mount_path, ''), COALESCE(last_seen_at, 0) FROM volumes ORDER BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Known
	for rows.Next() {
		var k Known
		var seen int64
		if err := rows.Scan(&k.Label, &k.UUID, &k.FSType, &k.LastPath, &seen); err != nil {
			return nil, err
		}
		if seen > 0 {
			k.LastSeen = time.UnixMilli(seen)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Forget removes label's binding, so the next drive with that label is bound instead. It returns
// how many bindings were removed.
func (b *Binder) Forget(ctx context.Context, label string) (int64, error) {
	r, err := b.DB.ExecContext(ctx, `DELETE FROM volumes WHERE label = ?`, label)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
