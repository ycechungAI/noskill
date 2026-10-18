package platform

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// OSTrash implements the freedesktop.org Trash spec 1.0: the home trash for files on the home
// filesystem, $topdir/.Trash/$uid or $topdir/.Trash-$uid for other drives. File managers (Nautilus,
// Dolphin, Thunar) show and restore these.
type OSTrash struct{}

func (OSTrash) Trash(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	dev := devOf(fi)
	home, err := homeTrash()
	if err != nil {
		return "", err
	}
	trashDir, infoPath := home, abs
	if hd, err := deviceOfDir(home); err != nil || hd != dev {
		top := mountTop(abs, dev)
		trashDir, err = topTrash(top)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(top, abs)
		if err != nil {
			return "", err
		}
		infoPath = rel // relative paths are allowed (and portable) for $topdir trashes
	}
	for _, d := range []string{"files", "info"} {
		if err := os.MkdirAll(filepath.Join(trashDir, d), 0o700); err != nil {
			return "", err
		}
	}
	name, info, err := reserve(trashDir, filepath.Base(abs))
	if err != nil {
		return "", err
	}
	u := url.URL{Path: infoPath}
	body := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", u.EscapedPath(), time.Now().Format("2006-01-02T15:04:05"))
	if _, err := info.WriteString(body); err != nil {
		info.Close()
		os.Remove(info.Name()) // our own reservation file
		return "", err
	}
	info.Close()
	dst := filepath.Join(trashDir, "files", name)
	if err := os.Rename(abs, dst); err != nil {
		os.Remove(info.Name())
		return "", fmt.Errorf("trash %s: %w", abs, err)
	}
	return dst, nil
}

func homeTrash() (string, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" || !filepath.IsAbs(data) {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data = filepath.Join(h, ".local", "share")
	}
	t := filepath.Join(data, "Trash")
	return t, os.MkdirAll(t, 0o700)
}

// reserve picks a free name ("a.txt", "a.2.txt", …) by creating its .trashinfo with O_EXCL,
// which is the spec's atomic reservation.
func reserve(trashDir, base string) (string, *os.File, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		name := base
		if i > 1 {
			name = stem + "." + strconv.Itoa(i) + ext
		}
		if _, err := os.Lstat(filepath.Join(trashDir, "files", name)); err == nil {
			continue
		}
		f, err := os.OpenFile(filepath.Join(trashDir, "info", name+".trashinfo"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, f, nil
	}
	return "", nil, errors.New("trash: no free name")
}

// topTrash returns $topdir/.Trash/$uid when the admin created a valid shared .Trash (a real
// directory with the sticky bit), else $topdir/.Trash-$uid.
func topTrash(top string) (string, error) {
	uid := strconv.Itoa(os.Getuid())
	shared := filepath.Join(top, ".Trash")
	if fi, err := os.Lstat(shared); err == nil && fi.IsDir() && fi.Mode()&os.ModeSticky != 0 && fi.Mode()&os.ModeSymlink == 0 {
		d := filepath.Join(shared, uid)
		if err := os.MkdirAll(d, 0o700); err == nil {
			return d, nil
		}
	}
	d := filepath.Join(top, ".Trash-"+uid)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("no trash on %s: %w", top, err)
	}
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unsafe trash folder %s", d)
	}
	return d, nil
}

// mountTop walks up from p while the device stays the same; the last such folder is the mount point.
func mountTop(p string, dev uint64) string {
	cur := filepath.Dir(p)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		d, err := deviceOfDir(parent)
		if err != nil || d != dev {
			return cur
		}
		cur = parent
	}
}

func deviceOfDir(p string) (uint64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return devOf(fi), nil
}

func devOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// TrashRestored is called after a file was put back from the trash: the freedesktop trash keeps a
// .trashinfo beside each file, and one without its file shows up as a broken entry in file
// managers. where is the path Trash returned (…/files/<name>); the info file is noskill's own.
func TrashRestored(where string) {
	files := filepath.Dir(where)
	if filepath.Base(files) != "files" {
		return
	}
	os.Remove(filepath.Join(filepath.Dir(files), "info", filepath.Base(where)+".trashinfo"))
}
