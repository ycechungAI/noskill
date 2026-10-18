// Package update implements `noskill update` (docs/PACKAGING.md): find a release, verify it against
// the release key embedded in this binary, and swap the binary in a way that can be undone. Nothing
// is installed unless the signature on checksums.txt and the SHA-256 of the archive both check out.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// PublicKey is the base64 ed25519 release public key, set at build time:
//
//	-ldflags "-X noskill/internal/update.PublicKey=<base64>"
//
// A build without one can't self-update (it says so), which keeps a development build from
// trusting anything.
var PublicKey string

type Release struct {
	Tag        string // as published, e.g. "v0.2.0"
	Version    string // without the leading v
	Prerelease bool
	Assets     map[string]string // name → download URL
}

type Client struct {
	HTTP   *http.Client
	API    string // default https://api.github.com
	Repo   string // owner/name
	GOOS   string // defaults to runtime.GOOS
	GOARCH string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Client) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	return "https://api.github.com"
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (g ghRelease) release() Release {
	r := Release{Tag: g.TagName, Version: strings.TrimPrefix(g.TagName, "v"), Prerelease: g.Prerelease, Assets: map[string]string{}}
	for _, a := range g.Assets {
		r.Assets[a.Name] = a.URL
	}
	return r
}

func (c *Client) getJSON(ctx context.Context, p string, v any) error {
	if c.Repo == "" {
		return errors.New("no release source configured: set update.repo = \"owner/name\" in config.toml")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.api()+p, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "noskill-update")
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the release server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no such release in %s", c.Repo)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("release server answered %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// Latest returns the newest release on the channel: "stable" ignores pre-releases, "beta" takes them.
func (c *Client) Latest(ctx context.Context, channel string) (Release, error) {
	if channel != "beta" {
		var g ghRelease
		if err := c.getJSON(ctx, "/repos/"+c.Repo+"/releases/latest", &g); err != nil {
			return Release{}, err
		}
		return g.release(), nil
	}
	var list []ghRelease
	if err := c.getJSON(ctx, "/repos/"+c.Repo+"/releases?per_page=20", &list); err != nil {
		return Release{}, err
	}
	var best *Release
	for _, g := range list {
		if g.Draft {
			continue
		}
		r := g.release()
		if best == nil || Compare(r.Version, best.Version) > 0 {
			best = &r
		}
	}
	if best == nil {
		return Release{}, errors.New("no releases found")
	}
	return *best, nil
}

// Get returns a specific release by version (with or without the v).
func (c *Client) Get(ctx context.Context, version string) (Release, error) {
	var g ghRelease
	if err := c.getJSON(ctx, "/repos/"+c.Repo+"/releases/tags/v"+strings.TrimPrefix(version, "v"), &g); err != nil {
		return Release{}, err
	}
	return g.release(), nil
}

// Compare orders versions: negative if a < b. Handles v-prefixes, major.minor.patch and
// pre-release suffixes ("0.2.0-beta.1" < "0.2.0").
func Compare(a, b string) int {
	pa, sa := split(a)
	pb, sb := split(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case sa == sb:
		return 0
	case sa == "":
		return 1
	case sb == "":
		return -1
	case sa < sb:
		return -1
	}
	return 1
}

func split(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	core, _, _ = strings.Cut(core, "+")
	var out [3]int
	for i, p := range strings.SplitN(core, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out, pre
}

// ArchiveName is the release archive for a platform (matches .goreleaser.yaml).
func ArchiveName(version, goos, goarch string) string {
	version = strings.TrimPrefix(version, "v")
	arch := goarch
	if goos == "darwin" {
		arch = "universal"
	}
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("noskill_%s_%s_%s.%s", version, goos, arch, ext)
}

func (c *Client) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "noskill-update")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download answered %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download is larger than %d MB", limit>>20)
	}
	return b, nil
}

// VerifyChecksums checks the ed25519 signature of checksums.txt with the embedded key.
func VerifyChecksums(checksums, sigB64 []byte, publicKey string) error {
	if publicKey == "" {
		return errors.New("this build has no release key, so it can't verify an update; reinstall from a release or use your package manager")
	}
	pk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return errors.New("the release key embedded in this build is malformed")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return errors.New("the signature file isn't valid base64")
	}
	if !ed25519.Verify(ed25519.PublicKey(pk), checksums, sig) {
		return errors.New("the signature on checksums.txt doesn't match the release key: the release may have been tampered with")
	}
	return nil
}

// Fetch downloads and verifies a release's archive for this platform and extracts the binary into
// dir as ".noskill.new" (dir should be the current binary's folder, so the swap is a same-filesystem
// rename). Nothing outside dir is written.
func (c *Client) Fetch(ctx context.Context, rel Release, dir string) (string, error) {
	goos, goarch := c.GOOS, c.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	name := ArchiveName(rel.Version, goos, goarch)
	for _, need := range []string{name, "checksums.txt", "checksums.txt.ed25519"} {
		if rel.Assets[need] == "" {
			return "", fmt.Errorf("release %s has no %s", rel.Tag, need)
		}
	}
	sums, err := c.download(ctx, rel.Assets["checksums.txt"], 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := c.download(ctx, rel.Assets["checksums.txt.ed25519"], 1<<10)
	if err != nil {
		return "", err
	}
	if err := VerifyChecksums(sums, sig, PublicKey); err != nil {
		return "", err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s isn't listed in the signed checksums", name)
	}
	archive, err := c.download(ctx, rel.Assets[name], 300<<20)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return "", errors.New("the downloaded archive doesn't match the signed checksum; nothing was installed")
	}
	bin := "noskill"
	if goos == "windows" {
		bin = "noskill.exe"
	}
	out, err := extractBinary(archive, name, bin)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, ".noskill.new")
	if err := os.WriteFile(dst, out, 0o755); err != nil {
		return "", err
	}
	return dst, nil
}

// extractBinary pulls the one file named bin out of a tar.gz or zip, ignoring paths.
func extractBinary(archive []byte, archiveName, bin string) ([]byte, error) {
	const limit = 200 << 20
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == bin && !f.FileInfo().IsDir() {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return readLimited(r, limit)
			}
		}
		return nil, fmt.Errorf("%s isn't in the archive", bin)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s isn't in the archive", bin)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == bin {
			return readLimited(tr, limit)
		}
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("the binary in the archive is implausibly large")
	}
	return b, nil
}

// SmokeTest runs the new binary's `version` command and checks it reports the expected version, so
// a corrupt or wrong-architecture binary is never swapped in.
func SmokeTest(ctx context.Context, bin, wantVersion string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("the new binary doesn't run on this machine: %w", err)
	}
	if !strings.Contains(string(out), strings.TrimPrefix(wantVersion, "v")) {
		return fmt.Errorf("the new binary reports %q, not %s", strings.TrimSpace(string(out)), wantVersion)
	}
	return nil
}

// Swap replaces exe with newBin and keeps the old one as exe+".old". It returns the backup's path.
// On Windows the running executable can be renamed but not overwritten, which is what this does.
// Any failure puts the old binary back.
func Swap(exe, newBin string) (backup string, err error) {
	backup = exe + ".old"
	if _, err := os.Lstat(backup); err == nil {
		// A leftover from an earlier update. Replace our own backup file.
		if err := os.Remove(backup); err != nil {
			return "", fmt.Errorf("can't clear the old backup %s: %w", backup, err)
		}
	}
	if err := os.Rename(exe, backup); err != nil {
		return "", fmt.Errorf("can't move the current binary aside: %w", err)
	}
	if err := os.Rename(newBin, exe); err != nil {
		os.Rename(backup, exe)
		return "", fmt.Errorf("can't put the new binary in place: %w", err)
	}
	return backup, nil
}

// Rollback puts the backup back as exe.
func Rollback(exe, backup string) error {
	failed := exe + ".failed"
	os.Remove(failed)
	if err := os.Rename(exe, failed); err != nil {
		return err
	}
	if err := os.Rename(backup, exe); err != nil {
		os.Rename(failed, exe)
		return err
	}
	return nil
}

// ---- package managers ----

// InstalledVia reports whether a package manager owns this binary, and the command that updates it.
// noskill never fights a package manager (docs/PACKAGING.md).
func InstalledVia(exe string) (manager, command string) {
	p := strings.ReplaceAll(exe, "\\", "/")
	low := strings.ToLower(p)
	switch {
	case strings.Contains(p, "/Cellar/noskill/") || strings.HasPrefix(p, "/opt/homebrew/bin/") || strings.HasPrefix(p, "/usr/local/Homebrew/") || strings.Contains(p, "/homebrew/"):
		return "Homebrew", "brew upgrade noskill"
	case strings.Contains(low, "/scoop/"):
		return "Scoop", "scoop update noskill"
	case strings.Contains(low, "/microsoft/winget/") || strings.Contains(low, "/winget/packages/"):
		return "winget", "winget upgrade noskill"
	case strings.HasPrefix(p, "/usr/bin/") || strings.HasPrefix(p, "/usr/sbin/"):
		if _, err := exec.LookPath("dpkg"); err == nil {
			return "apt", "sudo apt update && sudo apt install --only-upgrade noskill"
		}
		return "your package manager", "update the noskill package with your system's tools"
	}
	return "", ""
}

// ---- database restore after a failed update ----

// RestoreDB puts back the backup taken before a migration (state.db.bak-<version>), moving the
// migrated database to the trash first. Data written after the update is lost: the window is the
// 30 seconds the new version had to come up.
func RestoreDB(dataDir string, version int, trash func(string) (string, error)) error {
	backup := filepath.Join(dataDir, fmt.Sprintf("state.db.bak-%d", version))
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("no database backup %s", filepath.Base(backup))
	}
	db := filepath.Join(dataDir, "state.db")
	for _, suffix := range []string{"-wal", "-shm"} { // the migrated database's journal files go with it
		if _, err := os.Stat(db + suffix); err == nil {
			if _, err := trash(db + suffix); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(db); err == nil {
		if _, err := trash(db); err != nil {
			return err
		}
	}
	in, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(db, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
