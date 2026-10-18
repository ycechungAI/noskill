package platform

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func osDirs() (Dirs, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" || !filepath.IsAbs(cfg) {
		cfg = filepath.Join(h, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" || !filepath.IsAbs(data) {
		data = filepath.Join(h, ".local", "share")
	}
	return Dirs{Config: filepath.Join(cfg, "noskill"), Data: filepath.Join(data, "noskill")}, nil
}

// knownFolder reads ~/.config/user-dirs.dirs (xdg-user-dirs), which holds localised names such
// as "$HOME/Téléchargements", and falls back to the English defaults.
func knownFolder(token string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	key, def, ok := xdgKey(token)
	if !ok {
		return "", errors.New("unknown folder token")
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(h, ".config")
	}
	if f, err := os.Open(filepath.Join(cfg, "user-dirs.dirs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
			if !ok || k != key {
				continue
			}
			v = strings.Trim(v, `"`)
			v = strings.Replace(v, "$HOME", h, 1)
			if filepath.IsAbs(v) && filepath.Clean(v) != filepath.Clean(h) { // "$HOME" alone means "disabled"
				return v, nil
			}
		}
	}
	return filepath.Join(h, def), nil
}

func xdgKey(token string) (key, def string, ok bool) {
	switch token {
	case "$DOWNLOADS":
		return "XDG_DOWNLOAD_DIR", "Downloads", true
	case "$DOCUMENTS":
		return "XDG_DOCUMENTS_DIR", "Documents", true
	case "$DESKTOP":
		return "XDG_DESKTOP_DIR", "Desktop", true
	case "$PICTURES":
		return "XDG_PICTURES_DIR", "Pictures", true
	case "$VIDEOS":
		return "XDG_VIDEOS_DIR", "Videos", true
	case "$MUSIC":
		return "XDG_MUSIC_DIR", "Music", true
	}
	return "", "", false
}
