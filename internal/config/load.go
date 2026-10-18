package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Layer is where a value came from, lowest precedence first.
type Layer string

const (
	LayerDefault Layer = "default"
	LayerFile    Layer = "file"
	LayerProject Layer = "project"
	LayerEnv     Layer = "env"
	LayerFlag    Layer = "flag"
)

type Options struct {
	File       string            // the user's config.toml; a missing file is fine
	ProjectDir string            // folder whose .noskill/config.toml is the project layer; "" = none
	Env        []string          // usually os.Environ()
	Flags      map[string]string // "server.port" → "11500", from CLI flags
}

type Loaded struct {
	Config  Config
	Sources map[string]Layer // leaf key ("server.port") → layer that set it
	// SkillSources records the layer of each per-skill setting. Settings from the project layer
	// must never widen permissions, so path tokens like ${config.x} refuse them (SECURITY.md).
	SkillSources map[string]map[string]Layer
	Issues       []Issue
	Files        []string // files that were read
}

// Issue is a problem found by Load or Check. Errors stop the daemon from (re)loading the file;
// warnings are shown by `config check` and `doctor`.
type Issue struct {
	Level string `json:"level"` // "error" | "warn"
	Key   string `json:"key,omitempty"`
	Msg   string `json:"msg"`
}

func (i Issue) String() string {
	if i.Key == "" {
		return i.Level + ": " + i.Msg
	}
	return fmt.Sprintf("%s: %s: %s", i.Level, i.Key, i.Msg)
}

// Load applies the layers in order. A syntax or type error in a file is returned as an error with
// the file name and line; everything else becomes an Issue.
func Load(o Options) (*Loaded, error) {
	l := &Loaded{
		Config:       Defaults(),
		Sources:      map[string]Layer{},
		SkillSources: map[string]map[string]Layer{},
	}
	for _, k := range LeafKeys() {
		l.Sources[k] = LayerDefault
	}
	if o.File != "" {
		if err := l.loadFile(o.File); err != nil {
			return nil, err
		}
	}
	if o.ProjectDir != "" {
		proj := filepath.Join(o.ProjectDir, ".noskill", "config.toml")
		// Run from the home folder, the project's .noskill/config.toml is the user's own file: it isn't a
		// project layer, whose per-skill settings are ignored (it made a run from ~ use the default
		// Nyaa address instead of the user's mirror).
		if !sameFile(proj, o.File) {
			if err := l.loadProject(proj); err != nil {
				return nil, err
			}
		}
	}
	l.applyEnv(o.Env)
	keys := make([]string, 0, len(o.Flags))
	for k := range o.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := Set(&l.Config, k, o.Flags[k]); err != nil {
			return nil, fmt.Errorf("flag for %s: %w", k, err)
		}
		l.Sources[k] = LayerFlag
	}
	return l, nil
}

// sameFile says whether two paths are one file (a symlink or the same path spelled differently).
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(ai, bi)
}

func (l *Loaded) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	l.Files = append(l.Files, path)
	md, err := toml.Decode(string(b), &l.Config)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	leaves := leafSet()
	for _, k := range md.Keys() {
		if leaves[k.String()] {
			l.Sources[k.String()] = LayerFile
		}
	}
	for _, k := range md.Undecoded() {
		if len(k) >= 2 && k[0] == "skills" {
			continue // per-skill settings, collected below
		}
		l.Issues = append(l.Issues, Issue{Level: "error", Key: k.String(), Msg: "unknown key (typo?)"})
	}
	var raw map[string]any
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	l.mergeSkillTables(raw, LayerFile)
	return nil
}

// loadProject reads ./.noskill/config.toml. It may only add [skills] paths and per-skill settings;
// it can never widen permissions, change the server or turn off the sandbox (CONFIG.md).
func (l *Loaded) loadProject(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	l.Files = append(l.Files, path)
	var raw map[string]any
	if _, err := toml.Decode(string(b), &raw); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range raw {
		if k != "skills" {
			l.Issues = append(l.Issues, Issue{Level: "warn", Key: k, Msg: "ignored: a project config.toml can only set [skills] paths and per-skill settings"})
			continue
		}
		tbl, ok := v.(map[string]any)
		if !ok {
			l.Issues = append(l.Issues, Issue{Level: "error", Key: "skills", Msg: "want a table"})
			continue
		}
		for sk, sv := range tbl {
			switch sk {
			case "paths":
				arr, ok := sv.([]any)
				if !ok {
					l.Issues = append(l.Issues, Issue{Level: "error", Key: "skills.paths", Msg: "want an array of strings"})
					continue
				}
				base := filepath.Dir(filepath.Dir(path)) // the project folder
				for _, p := range arr {
					s, ok := p.(string)
					if !ok {
						l.Issues = append(l.Issues, Issue{Level: "error", Key: "skills.paths", Msg: "want an array of strings"})
						continue
					}
					if !filepath.IsAbs(s) && !strings.HasPrefix(s, "~") {
						s = filepath.Join(base, s)
					}
					l.Config.Skills.Paths = append(l.Config.Skills.Paths, s)
				}
				l.Sources["skills.paths"] = LayerProject
			case "auto_update":
				l.Issues = append(l.Issues, Issue{Level: "warn", Key: "skills.auto_update", Msg: "ignored: only your own config.toml can change this"})
			}
		}
	}
	l.mergeSkillTables(raw, LayerProject)
	return nil
}

func (l *Loaded) mergeSkillTables(raw map[string]any, layer Layer) {
	tbl, _ := raw["skills"].(map[string]any)
	for name, v := range tbl {
		if name == "paths" || name == "auto_update" {
			continue
		}
		settings, ok := v.(map[string]any)
		if !ok {
			l.Issues = append(l.Issues, Issue{Level: "error", Key: "skills." + name, Msg: "per-skill settings must be a table [skills." + name + "]"})
			continue
		}
		if l.Config.Skills.Settings[name] == nil {
			l.Config.Skills.Settings[name] = map[string]any{}
			l.SkillSources[name] = map[string]Layer{}
		}
		for k, sv := range settings {
			l.Config.Skills.Settings[name][k] = sv
			l.SkillSources[name][k] = layer
		}
	}
}

// EnvName is the variable that overrides key: "server.port" → NOSKILL_SERVER_PORT.
func EnvName(key string) string {
	return "NOSKILL_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
}

func (l *Loaded) applyEnv(env []string) {
	vals := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "NOSKILL_") {
			vals[k] = v
		}
	}
	if len(vals) == 0 {
		return
	}
	for _, key := range LeafKeys() {
		v, ok := vals[EnvName(key)]
		if !ok {
			continue
		}
		if err := Set(&l.Config, key, v); err != nil {
			l.Issues = append(l.Issues, Issue{Level: "error", Key: key, Msg: EnvName(key) + ": " + err.Error()})
			continue
		}
		l.Sources[key] = LayerEnv
	}
}

// LeafKeys lists every settable key ("general.log_level", "server.tokens", …) in struct order.
func LeafKeys() []string {
	var out []string
	walk(reflect.TypeOf(Config{}), "", func(key string, _ []int) { out = append(out, key) })
	return out
}

func leafSet() map[string]bool {
	m := map[string]bool{}
	for _, k := range LeafKeys() {
		m[k] = true
	}
	return m
}

var (
	durationType  = reflect.TypeOf(Duration{})
	autoFloatType = reflect.TypeOf(AutoFloat{})
)

func walk(t reflect.Type, prefix string, fn func(key string, index []int)) {
	var rec func(t reflect.Type, prefix string, index []int)
	rec = func(t reflect.Type, prefix string, index []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("toml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			key := tag
			if prefix != "" {
				key = prefix + "." + tag
			}
			idx := append(append([]int{}, index...), i)
			if f.Type.Kind() == reflect.Struct && f.Type != durationType && f.Type != autoFloatType {
				rec(f.Type, key, idx)
				continue
			}
			fn(key, idx)
		}
	}
	rec(t, prefix, nil)
}

// Set parses raw for key's type and stores it: used by env overrides, flags and `config set`.
// Arrays of tables (backends, tokens, rules) and inline tables can only be set in the file.
func Set(c *Config, key, raw string) error {
	var idx []int
	walk(reflect.TypeOf(Config{}), "", func(k string, i []int) {
		if k == key {
			idx = i
		}
	})
	if idx == nil {
		return fmt.Errorf("unknown key %q", key)
	}
	v := reflect.ValueOf(c).Elem().FieldByIndex(idx)
	switch {
	case v.Type() == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(Duration{d}))
	case v.Type() == autoFloatType:
		var a AutoFloat
		if err := a.UnmarshalTOML(raw); err != nil {
			return err
		}
		v.Set(reflect.ValueOf(a))
	case v.Kind() == reflect.String:
		v.SetString(raw)
	case v.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("want true or false, got %q", raw)
		}
		v.SetBool(b)
	case v.Kind() == reflect.Int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("want an integer, got %q", raw)
		}
		v.SetInt(int64(n))
	case v.Kind() == reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("want a number, got %q", raw)
		}
		v.SetFloat(f)
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String:
		var items []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		if items == nil {
			items = []string{}
		}
		v.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("%s can only be set in config.toml", key)
	}
	return nil
}

// Get returns key's current value formatted for display.
func Get(c Config, key string) (string, bool) {
	var idx []int
	walk(reflect.TypeOf(Config{}), "", func(k string, i []int) {
		if k == key {
			idx = i
		}
	})
	if idx == nil {
		return "", false
	}
	v := reflect.ValueOf(c).FieldByIndex(idx)
	switch x := v.Interface().(type) {
	case Duration:
		return x.String(), true
	case AutoFloat:
		return x.String(), true
	case []string:
		return strconv.Quote(strings.Join(x, ",")), true
	case string:
		return strconv.Quote(x), true
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return "(not set)", true
	}
	if v.Kind() == reflect.Slice {
		return fmt.Sprintf("(%d entries)", v.Len()), true
	}
	return fmt.Sprint(v.Interface()), true
}
