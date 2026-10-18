package platform

// VolumeInfo describes a mounted drive for the `files` skill's volumes tool (PLATFORMS.md
// "External drives"). UUID is filled by internal/volumes (VolumeUUID), not by ListVolumes, which
// stays cheap enough to call for every prompt.
type VolumeInfo struct {
	Label      string `json:"label"`
	Mount      string `json:"mount"`
	FSType     string `json:"filesystem,omitempty"`
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
	Removable  bool   `json:"external"` // under /Volumes, /media, /mnt; any non-system drive on Windows
	UUID       string `json:"uuid,omitempty"`
}
