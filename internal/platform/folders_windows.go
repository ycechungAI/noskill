package platform

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func osDirs() (Dirs, error) {
	roaming, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
	if err != nil {
		roaming = os.Getenv("APPDATA")
	}
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		local = os.Getenv("LOCALAPPDATA")
	}
	if roaming == "" || local == "" {
		return Dirs{}, errors.New("APPDATA/LOCALAPPDATA not set")
	}
	return Dirs{Config: filepath.Join(roaming, "noskill"), Data: filepath.Join(local, "noskill")}, nil
}

// knownFolder uses SHGetKnownFolderPath, which follows folders the user moved (e.g. Downloads on D:).
func knownFolder(token string) (string, error) {
	id, ok := map[string]*windows.KNOWNFOLDERID{
		"$DOWNLOADS": windows.FOLDERID_Downloads, "$DOCUMENTS": windows.FOLDERID_Documents,
		"$DESKTOP": windows.FOLDERID_Desktop, "$PICTURES": windows.FOLDERID_Pictures,
		"$VIDEOS": windows.FOLDERID_Videos, "$MUSIC": windows.FOLDERID_Music,
	}[token]
	if !ok {
		return "", errors.New("unknown folder token")
	}
	return windows.KnownFolderPath(id, 0)
}
