package platform

import (
	"errors"
	"os"
	"path/filepath"
)

func osDirs() (Dirs, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	d := filepath.Join(h, ".noskill")
	return Dirs{Config: d, Data: d}, nil
}

func knownFolder(token string) (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name, ok := map[string]string{
		"$DOWNLOADS": "Downloads", "$DOCUMENTS": "Documents", "$DESKTOP": "Desktop",
		"$PICTURES": "Pictures", "$VIDEOS": "Movies", "$MUSIC": "Music",
	}[token]
	if !ok {
		return "", errors.New("unknown folder token")
	}
	return filepath.Join(h, name), nil
}
