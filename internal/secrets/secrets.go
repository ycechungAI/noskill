// Package secrets stores API keys and tokens in the OS keychain (CLI.md `noskill secrets`).
// Config files only ever hold "secret:NAME" references (CONFIG.md). Values are never written to
// disk by noskill, never put on a command line, and never logged.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Store is a keychain.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
}

var ErrNotFound = errors.New("secret not found")

var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

// ValidName: names are used in env vars and keychain lookups, so keep them plain.
func ValidName(n string) error {
	if !nameRe.MatchString(n) {
		return fmt.Errorf("secret name %q: use letters, digits, '_', '.' and '-' (starting with a letter, at most 64 characters)", n)
	}
	return nil
}

// Manager adds what the OS keychains don't give: listing (kept as a names-only index, never values)
// and an environment override, NOSKILL_SECRET_<NAME>, for containers and CI where no keychain exists.
type Manager struct {
	Keychain  Store
	IndexPath string // file holding the names (not the values), one per line
	Environ   func(string) (string, bool)

	mu sync.Mutex
}

func NewManager(k Store, indexPath string) *Manager {
	return &Manager{Keychain: k, IndexPath: indexPath, Environ: os.LookupEnv}
}

func envName(n string) string {
	return "NOSKILL_SECRET_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(n))
}

func (m *Manager) Get(name string) (string, bool) {
	if v, ok := m.Environ(envName(name)); ok && v != "" {
		return v, true
	}
	if m.Keychain == nil {
		return "", false
	}
	v, err := m.Keychain.Get(name)
	return v, err == nil && v != ""
}

// Has reports whether a secret exists without exposing it (config check).
func (m *Manager) Has(name string) (bool, error) {
	if _, ok := m.Environ(envName(name)); ok {
		return true, nil
	}
	if m.Keychain == nil {
		return false, errors.New("no keychain on this system")
	}
	_, err := m.Keychain.Get(name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (m *Manager) Set(name, value string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if value == "" {
		return errors.New("empty secret")
	}
	if m.Keychain == nil {
		return errors.New("no keychain on this system; set NOSKILL_SECRET_" + strings.TrimPrefix(envName(name), "NOSKILL_SECRET_") + " in the environment instead")
	}
	if err := m.Keychain.Set(name, value); err != nil {
		return err
	}
	return m.index(func(names map[string]bool) { names[name] = true })
}

func (m *Manager) Delete(name string) error {
	if m.Keychain == nil {
		return errors.New("no keychain on this system")
	}
	err := m.Keychain.Delete(name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	m.index(func(names map[string]bool) { delete(names, name) })
	return err
}

// List returns the names noskill stored (plus those it's been told about through the environment).
func (m *Manager) List() ([]string, error) {
	set := map[string]bool{}
	if err := m.index(func(n map[string]bool) {
		for k := range n {
			set[k] = true
		}
	}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Manager) index(mut func(map[string]bool)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := map[string]bool{}
	if b, err := os.ReadFile(m.IndexPath); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				names[l] = true
			}
		}
	}
	mut(names)
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	return os.WriteFile(m.IndexPath, []byte(strings.Join(list, "\n")+"\n"), 0o600)
}

// Mem is an in-memory Store for tests.
type Mem struct {
	mu sync.Mutex
	M  map[string]string
}

func (m *Mem) Get(n string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.M[n]; ok {
		return v, nil
	}
	return "", ErrNotFound
}

func (m *Mem) Set(n, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.M == nil {
		m.M = map[string]string{}
	}
	m.M[n] = v
	return nil
}

func (m *Mem) Delete(n string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.M[n]; !ok {
		return ErrNotFound
	}
	delete(m.M, n)
	return nil
}
