package perms

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"noskill/internal/platform"
)

type Access string

const (
	Read  Access = "read"
	Write Access = "write" // includes reading, moving out of and trashing (SECURITY.md "Permissions")
)

// Scope is one compiled filesystem permission.
type Scope struct {
	Access Access
	Root   string // canonical
	Decl   string // as declared, e.g. "write:volume:RED/ANIME2026"
}

// Set is what a tool call may touch: the owning skill's permissions, narrowed by the caller.
type Set struct {
	Files      []Scope
	Network    []string // host patterns: "api.x.com", "*.github.com", "localhost", "*"
	Exec       bool
	Subprocess bool
}

// Resolver turns the tokens in a declaration into real folders (PLATFORMS.md "Paths").
type Resolver struct {
	// Config looks up ${config.<key>}. fromProject marks a value from a project's .noskill/config.toml,
	// which may never grant filesystem access: a cloned repository would choose its own scope.
	Config func(key string) (value string, fromProject bool, ok bool)
	// Approved holds the folders the user picked in this skill's approvals, already validated with
	// ValidateApproved.
	Approved map[string]string
	Volumes  platform.Volumes
	// VolumeUUIDs are the bindings from the volumes table: label → UUID. A mounted volume with the
	// same label but another UUID is refused (label spoofing).
	VolumeUUIDs map[string]string
}

// Compile parses declarations like "read:~/Documents" or "write:${approved.dest}".
func (r Resolver) Compile(decls []string) ([]Scope, error) {
	var out []Scope
	for _, d := range decls {
		acc, p, ok := strings.Cut(d, ":")
		if !ok || (acc != string(Read) && acc != string(Write)) {
			return nil, fmt.Errorf("filesystem permission %q: want read:<path> or write:<path>", d)
		}
		path, err := r.Expand(p)
		if err != nil {
			return nil, fmt.Errorf("filesystem permission %q: %w", d, err)
		}
		root, err := Canonical(path)
		if err != nil {
			return nil, fmt.Errorf("filesystem permission %q: %w", d, err)
		}
		out = append(out, Scope{Access: Access(acc), Root: root, Decl: d})
	}
	return out, nil
}

// Expand resolves one path token. Only one token is allowed, at the start of the path.
func (r Resolver) Expand(p string) (string, error) {
	head, tail := p, ""
	if i := strings.IndexAny(p, `/\`); i >= 0 {
		head, tail = p[:i], p[i+1:]
	}
	var base string
	var err error
	switch {
	case head == "~":
		base, err = platform.ExpandHome("~")
	case slices.Contains(platform.FolderTokens, head):
		base, err = platform.KnownFolder(head)
	case strings.HasPrefix(p, "volume:"):
		label, rest, _ := strings.Cut(strings.TrimPrefix(p, "volume:"), "/")
		base, err = r.volume(label)
		tail = rest
	case strings.HasPrefix(head, "${config.") && strings.HasSuffix(head, "}"):
		key := strings.TrimSuffix(strings.TrimPrefix(head, "${config."), "}")
		if r.Config == nil {
			return "", fmt.Errorf("${config.%s}: no config", key)
		}
		v, fromProject, ok := r.Config(key)
		switch {
		case !ok || v == "":
			return "", fmt.Errorf("${config.%s} isn't set", key)
		case fromProject:
			return "", fmt.Errorf("${config.%s} comes from a project config, which can't grant file access", key)
		}
		if strings.Contains(v, "${") {
			return "", fmt.Errorf("${config.%s} contains another token", key)
		}
		base, err = r.Expand(v)
	case strings.HasPrefix(head, "${approved.") && strings.HasSuffix(head, "}"):
		key := strings.TrimSuffix(strings.TrimPrefix(head, "${approved."), "}")
		v, ok := r.Approved[key]
		if !ok {
			return "", fmt.Errorf("${approved.%s}: no folder approved yet", key)
		}
		base = v
	case strings.Contains(p, "$"):
		return "", fmt.Errorf("unknown token in %q", p)
	default:
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%q isn't absolute and has no token", p)
		}
		return p, nil
	}
	if err != nil {
		return "", err
	}
	if tail == "" {
		return base, nil
	}
	return filepath.Join(base, filepath.FromSlash(tail)), nil
}

func (r Resolver) volume(label string) (string, error) {
	if r.Volumes == nil {
		return "", fmt.Errorf("volume:%s: %w", label, platform.ErrUnsupported)
	}
	want := r.VolumeUUIDs[label]
	mount, got, err := r.Volumes.Resolve(label, want)
	if err != nil {
		return "", fmt.Errorf("volume:%s: %w", label, err)
	}
	if want != "" && !strings.EqualFold(got, want) {
		return "", fmt.Errorf("volume:%s: mounted volume has UUID %s, bound to %s: %w", label, got, want, platform.ErrVolumeNotMounted)
	}
	return mount, nil
}

// IsRoot reports whether canonical path c is a granted folder itself. Moving or trashing a whole
// granted folder (the Downloads folder, a library root) is never what a tool call means.
func (s Set) IsRoot(c string) bool {
	for _, sc := range s.Files {
		if fold(c) == fold(sc.Root) {
			return true
		}
	}
	return false
}

// checkInside is CheckWrite for operations that take something out of its place (move, trash): the
// path must be inside a granted folder, not the folder itself.
func (s Set) checkInside(p string) (string, error) {
	c, err := s.CheckWrite(p)
	if err != nil {
		return "", err
	}
	if s.IsRoot(c) {
		return "", deny("write", p, "that is a whole granted folder, not something inside it")
	}
	return c, nil
}

// CheckRead returns the canonical path if s allows reading it.
func (s Set) CheckRead(p string) (string, error) { return s.check(Read, p) }

// CheckWrite returns the canonical path if s allows creating, changing, moving or trashing it.
func (s Set) CheckWrite(p string) (string, error) { return s.check(Write, p) }

func (s Set) check(a Access, p string) (string, error) {
	c, err := Canonical(p)
	if err != nil {
		return "", err
	}
	for _, sc := range s.Files {
		if (sc.Access == a || sc.Access == Write) && Within(c, sc.Root) {
			return c, nil
		}
	}
	return "", deny(string(a), p, "outside this skill's %s scopes", a)
}

// Narrow returns the intersection of the owner's permissions and the caller's (SECURITY.md "Scope
// rule"): a calling skill can narrow what a tool may do, never widen it.
func Narrow(owner, caller Set) Set {
	out := Set{Exec: owner.Exec && caller.Exec, Subprocess: owner.Subprocess && caller.Subprocess}
	for _, o := range owner.Files {
		for _, c := range caller.Files {
			acc := Read
			if o.Access == Write && c.Access == Write {
				acc = Write
			}
			switch {
			case Within(c.Root, o.Root):
				out.Files = append(out.Files, Scope{Access: acc, Root: c.Root, Decl: c.Decl})
			case Within(o.Root, c.Root):
				out.Files = append(out.Files, Scope{Access: acc, Root: o.Root, Decl: o.Decl})
			}
		}
	}
	for _, c := range caller.Network {
		if owner.allowsPattern(c) {
			out.Network = append(out.Network, c)
		}
	}
	for _, o := range owner.Network {
		if o != "*" && !slices.Contains(out.Network, o) && caller.allowsPattern(o) {
			out.Network = append(out.Network, o)
		}
	}
	return out
}

// allowsPattern reports whether every host matching pattern p is allowed by s.
func (s Set) allowsPattern(p string) bool {
	for _, n := range s.Network {
		switch {
		case n == p, n == "*" && p != "localhost":
			return true
		case strings.HasPrefix(n, "*.") && strings.HasSuffix(p, n[1:]):
			return true
		}
	}
	return false
}

// ValidateApproved checks a folder the user picked in an approval before it joins a write scope
// (SECURITY.md "Folders picked in approvals"): not inside noskill's own folders, not an OS folder,
// not a whole home folder or drive root. It returns the canonical path.
func ValidateApproved(dir string, own []string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", deny("scope", dir, "an approved folder must be an absolute path")
	}
	c, err := Canonical(dir)
	if err != nil {
		return "", err
	}
	for _, o := range own {
		if oc, err := Canonical(o); err == nil && (Within(c, oc) || Within(oc, c)) {
			return "", deny("scope", dir, "overlaps noskill's own folder %s", o)
		}
	}
	for _, sd := range platform.SystemDirs() {
		if sc, err := Canonical(sd); err == nil && Within(c, sc) {
			return "", deny("scope", dir, "inside the system folder %s", sd)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if hc, err := Canonical(home); err == nil && fold(hc) == fold(c) {
			return "", deny("scope", dir, "a whole home folder can't be approved")
		}
	}
	if platform.IsVolumeRoot(c) {
		return "", deny("scope", dir, "a whole drive can't be approved")
	}
	return c, nil
}
