package volumes

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"noskill/internal/platform"
	"noskill/internal/store"
)

func TestBindsOnFirstUseAndRefusesLookAlike(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	attached := []platform.VolumeInfo{{Label: "RED", Mount: "/Volumes/RED", FSType: "exfat"}}
	uuids := map[string]string{"/Volumes/RED": "AAAA"}
	b := &Binder{DB: db, List: func() ([]platform.VolumeInfo, error) { return attached, nil },
		UUID: func(m string) (string, error) { return uuids[m], nil }}

	if mount, got, err := b.Resolve("RED", ""); err != nil || mount != "/Volumes/RED" || got != "AAAA" {
		t.Fatalf("first use: %s %s %v", mount, got, err)
	}
	if bound, _ := b.Bindings(ctx); bound["RED"] != "AAAA" {
		t.Fatalf("not bound: %v", bound)
	}
	// Another stick called RED: refused.
	uuids["/Volumes/RED"] = "BBBB"
	if _, _, err := b.Resolve("RED", ""); !errors.Is(err, platform.ErrVolumeNotMounted) || !strings.Contains(err.Error(), "volumes forget RED") {
		t.Fatalf("look-alike: %v", err)
	}
	// The real one at another mount point still resolves.
	attached = append(attached, platform.VolumeInfo{Label: "RED", Mount: "/Volumes/RED 1"})
	uuids["/Volumes/RED 1"] = "aaaa"
	if mount, _, err := b.Resolve("RED", ""); err != nil || mount != "/Volumes/RED 1" {
		t.Fatalf("moved mount: %s %v", mount, err)
	}
	if _, _, err := b.Resolve("BLUE", ""); !errors.Is(err, platform.ErrVolumeNotMounted) {
		t.Fatalf("absent: %v", err)
	}
	// Forget, then the new drive binds.
	if n, err := b.Forget(ctx, "RED"); n != 1 || err != nil {
		t.Fatalf("forget: %d %v", n, err)
	}
	attached = attached[:1]
	if _, got, err := b.Resolve("RED", ""); err != nil || got != "BBBB" {
		t.Fatalf("rebind: %s %v", got, err)
	}
	if k, _ := b.Known(ctx); len(k) != 1 || k[0].UUID != "BBBB" || k[0].LastPath != "/Volumes/RED" {
		t.Fatalf("known: %+v", k)
	}
}

func TestTwoUnboundDrivesWithOneLabel(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := &Binder{DB: db, List: func() ([]platform.VolumeInfo, error) {
		return []platform.VolumeInfo{{Label: "RED", Mount: "/a"}, {Label: "RED", Mount: "/b"}}, nil
	}, UUID: func(m string) (string, error) { return "U" + m, nil }}
	if _, _, err := b.Resolve("RED", ""); err == nil || !strings.Contains(err.Error(), "2 drives") {
		t.Fatalf("ambiguous: %v", err)
	}
}
