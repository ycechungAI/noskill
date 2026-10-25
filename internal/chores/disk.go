package chores

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"noskill/internal/platform"
)

func strconvI(n int) string { return strconv.Itoa(n) }

// DiskOptions are the limits disk-watch warns at. A volume is low when it has less than MinFreeGB
// free or less than MinFreePct of its size free.
type DiskOptions struct {
	MinFreeGB  float64  // 20
	MinFreePct float64  // 5
	Only       []string // volume labels or mount paths; empty = every volume
}

func (o DiskOptions) withDefaults() DiskOptions {
	if o.MinFreeGB <= 0 {
		o.MinFreeGB = 20
	}
	if o.MinFreePct <= 0 {
		o.MinFreePct = 5
	}
	return o
}

// VolumeReport is one drive's free space and whether it is under the limits.
type VolumeReport struct {
	Label    string  `json:"label"`
	Mount    string  `json:"mount"`
	FreeGB   float64 `json:"free_gb"`
	TotalGB  float64 `json:"total_gb"`
	FreePct  float64 `json:"free_pct"`
	External bool    `json:"external"`
	Low      bool    `json:"low"`
	Why      string  `json:"why,omitempty"`
}

// CheckVolumes reports each volume against the limits.
func CheckVolumes(vs []platform.VolumeInfo, o DiskOptions) []VolumeReport {
	o = o.withDefaults()
	only := map[string]bool{}
	for _, s := range o.Only {
		only[strings.ToLower(s)] = true
	}
	out := []VolumeReport{}
	for _, v := range vs {
		// Tiny volumes are macOS's disk images and recovery partitions (a "Lockdown" image reads 0 GB
		// free and always will): not places the user's files go, so they never raise an alert.
		if v.TotalBytes < 2_000_000_000 {
			continue
		}
		if len(only) > 0 && !only[strings.ToLower(v.Label)] && !only[strings.ToLower(v.Mount)] {
			continue
		}
		r := VolumeReport{Label: v.Label, Mount: v.Mount, External: v.Removable,
			FreeGB: float64(v.FreeBytes) / 1e9, TotalGB: float64(v.TotalBytes) / 1e9}
		r.FreePct = 100 * float64(v.FreeBytes) / float64(v.TotalBytes)
		switch {
		case r.FreeGB < o.MinFreeGB:
			r.Low, r.Why = true, fmt.Sprintf("%.0f GB free, under the %.0f GB limit", r.FreeGB, o.MinFreeGB)
		case r.FreePct < o.MinFreePct:
			r.Low, r.Why = true, fmt.Sprintf("%.1f%% free, under the %.0f%% limit", r.FreePct, o.MinFreePct)
		}
		if r.Label == "" {
			r.Label = filepath.Base(v.Mount)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FreePct < out[j].FreePct })
	return out
}

// BigThing is a folder or file and what it takes.
type BigThing struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified,omitzero"`
}

// Usage lists what takes the space under root: its biggest folders (by their whole size) and its
// largest files not modified for at least idle. The walk stops at budget, and says so.
func Usage(ctx context.Context, root string, n int, minFile int64, idle, budget time.Duration) (folders, oldFiles []BigThing, truncated bool) {
	if n <= 0 {
		n = 10
	}
	if budget <= 0 {
		budget = 60 * time.Second
	}
	deadline := time.Now().Add(budget)
	sizes := map[string]int64{}
	now := time.Now()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			truncated = true
			return fs.SkipAll
		}
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(strings.ToLower(d.Name()), ".app")) {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if top, _, found := strings.Cut(filepath.ToSlash(rel), "/"); found {
			sizes[filepath.Join(root, top)] += fi.Size()
		}
		if fi.Size() >= minFile && now.Sub(fi.ModTime()) >= idle {
			oldFiles = append(oldFiles, BigThing{Path: p, Bytes: fi.Size(), Modified: fi.ModTime()})
		}
		return nil
	})
	for p, b := range sizes {
		folders = append(folders, BigThing{Path: p, Bytes: b})
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Bytes > folders[j].Bytes })
	sort.Slice(oldFiles, func(i, j int) bool { return oldFiles[i].Bytes > oldFiles[j].Bytes })
	if len(folders) > n {
		folders = folders[:n]
	}
	if len(oldFiles) > n {
		oldFiles = oldFiles[:n]
	}
	return folders, oldFiles, truncated
}

// Alert is the notification text for the volumes that are low; "" when none is.
func Alert(vs []VolumeReport) string {
	var parts []string
	for _, v := range vs {
		if v.Low {
			parts = append(parts, fmt.Sprintf("%s: %s", v.Label, v.Why))
		}
	}
	return strings.Join(parts, "; ")
}

// Human prints a byte count the way a person reads it.
func Human(b int64) string {
	const u = 1000
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
