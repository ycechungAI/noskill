// Package registry signs skill folders and registry indexes, and checks them (FR-S10, ADR-0016).
// A signed skill carries .noskill-sig.json; a registry is a folder with index.json and
// index.json.sig. Keys are ed25519. Nothing here touches the network or the keychain.
package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SigFile      = ".noskill-sig.json"
	IndexFile    = "index.json"
	IndexSigFile = "index.json.sig"
	manifestHead = "noskill-skill-v1"
)

// KeyID names a public key: the first 16 hex digits of its sha256.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// ParsePublicKey reads a base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not an ed25519 public key (base64, as `noskill registry key show` prints it)")
	}
	return ed25519.PublicKey(b), nil
}

// EncodePublicKey is the form keys are shared in.
func EncodePublicKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// Manifest is what a skill signature covers: the name, the version and every file's sha256.
type Manifest struct {
	Name, Version string
	Files         map[string]string // slash path → hex sha256
}

// Text is the exact bytes that are signed.
func (m Manifest) Text() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\nname: %s\nversion: %s\n", manifestHead, m.Name, m.Version)
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", m.Files[p], p)
	}
	return b.Bytes()
}

// Digest is the manifest's sha256, as an index entry records it.
func (m Manifest) Digest() string {
	h := sha256.Sum256(m.Text())
	return "sha256:" + hex.EncodeToString(h[:])
}

// BuildManifest hashes a skill folder. .git and the signature file are left out; a symlink or
// special file is an error, since an install wouldn't copy it.
func BuildManifest(dir, name, version string) (Manifest, error) {
	m := Manifest{Name: name, Version: version, Files: map[string]string{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
			return nil
		case d.IsDir() && d.Name() == ".git":
			return fs.SkipDir
		case d.IsDir():
			return nil
		case rel == SigFile:
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%s isn't a regular file (links can't be signed)", rel)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		m.Files[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return m, err
}

// Signature is .noskill-sig.json.
type Signature struct {
	V       int       `json:"v"`
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Digest  string    `json:"digest"`
	KeyID   string    `json:"key_id"`
	Key     string    `json:"key"` // base64 public key
	Sig     string    `json:"sig"` // base64 ed25519 over the manifest text
	Signed  time.Time `json:"signed"`
	// Files is the signed manifest's file list, so a check can say which file changed.
	Files map[string]string `json:"files"`
}

// Manifest is the manifest the signature claims to cover.
func (s Signature) Manifest() Manifest {
	return Manifest{Name: s.Name, Version: s.Version, Files: s.Files}
}

// Sign signs a manifest.
func Sign(m Manifest, priv ed25519.PrivateKey, now time.Time) Signature {
	pub := priv.Public().(ed25519.PublicKey)
	return Signature{V: 1, Name: m.Name, Version: m.Version, Digest: m.Digest(), KeyID: KeyID(pub), Key: EncodePublicKey(pub),
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.Text())), Signed: now.UTC(), Files: m.Files}
}

// ReadSignature reads a folder's .noskill-sig.json; ok is false when it has none.
func ReadSignature(dir string) (Signature, bool, error) {
	var s Signature
	b, err := os.ReadFile(filepath.Join(dir, SigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, true, fmt.Errorf("%s isn't valid: %w", SigFile, err)
	}
	return s, true, nil
}

// MarshalSignature is the file's content.
func MarshalSignature(s Signature) []byte {
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// ChangedError is a folder that no longer matches its signature.
type ChangedError struct{ Files []string }

func (e *ChangedError) Error() string {
	return "these files changed after it was signed: " + strings.Join(e.Files, ", ")
}

// Verify checks a signature, then the folder as it is now against the file list it signed. A
// folder that changed since gives a *ChangedError naming the files.
func Verify(dir string, s Signature, name, version string) error {
	pub, err := ParsePublicKey(s.Key)
	if err != nil {
		return fmt.Errorf("%s: %w", SigFile, err)
	}
	if KeyID(pub) != s.KeyID {
		return fmt.Errorf("%s: its key ID doesn't match its key", SigFile)
	}
	signed := s.Manifest()
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || signed.Digest() != s.Digest || !ed25519.Verify(pub, signed.Text(), sig) {
		return fmt.Errorf("%s: the signature doesn't match its file list", SigFile)
	}
	if s.Name != name || s.Version != version {
		return fmt.Errorf("it was signed as %s %s, but the folder is %s %s", s.Name, s.Version, name, version)
	}
	now, err := BuildManifest(dir, name, version)
	if err != nil {
		return err
	}
	if c := Changed(signed, now); len(c) > 0 {
		return &ChangedError{c}
	}
	return nil
}

// Changed lists the files that differ between two manifests (added, removed or edited).
func Changed(was, now Manifest) []string {
	var out []string
	for p, h := range now.Files {
		if was.Files[p] != h {
			out = append(out, p)
		}
	}
	for p := range was.Files {
		if _, ok := now.Files[p]; !ok {
			out = append(out, p+" (removed)")
		}
	}
	sort.Strings(out)
	return out
}

// Entry is one skill in a registry.
type Entry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Path        string   `json:"path"` // slash path of the skill folder inside the registry
	Digest      string   `json:"digest"`
	Permissions []string `json:"permissions,omitempty"`
}

// Index is a registry's index.json.
type Index struct {
	V       int       `json:"v"`
	Name    string    `json:"name"`
	Made    time.Time `json:"made"`
	KeyID   string    `json:"key_id"`
	Entries []Entry   `json:"skills"`
}

// SignIndex returns index.json and its signature.
func SignIndex(ix Index, priv ed25519.PrivateKey) (data, sig []byte, err error) {
	ix.V, ix.KeyID = 1, KeyID(priv.Public().(ed25519.PublicKey))
	sort.Slice(ix.Entries, func(i, j int) bool { return ix.Entries[i].Name < ix.Entries[j].Name })
	data, err = json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	data = append(data, '\n')
	return data, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data)) + "\n"), nil
}

// VerifyIndex checks index.json's bytes against the key the user added for this registry.
func VerifyIndex(data, sig []byte, pub ed25519.PublicKey) (Index, error) {
	var ix Index
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, data, s) {
		return ix, fmt.Errorf("%s isn't signed by the key you added for this registry (%s): it was changed, or the registry has a new key", IndexFile, KeyID(pub))
	}
	if err := json.Unmarshal(data, &ix); err != nil {
		return ix, fmt.Errorf("%s: %w", IndexFile, err)
	}
	for _, e := range ix.Entries {
		if p := path.Clean(e.Path); e.Path == "" || path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(e.Path, `\`) {
			return ix, fmt.Errorf("%s: %s's path %q leaves the registry", IndexFile, e.Name, e.Path)
		}
	}
	return ix, nil
}

// Find looks a skill up by name.
func (ix Index) Find(name string) (Entry, bool) {
	for _, e := range ix.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Search returns the entries whose name or description holds every word (any case).
func (ix Index) Search(words []string) []Entry {
	var out []Entry
	for _, e := range ix.Entries {
		hay := strings.ToLower(e.Name + " " + e.Description)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, strings.ToLower(w)) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}
