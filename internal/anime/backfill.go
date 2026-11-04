package anime

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// BackfillFiled records episodes already in a show folder as filed, so the History tab shows
// them. It reads `<root>/<Title>/<Title> S0xEnn [res].mkv` (the naming the tracker uses) and matches
// the title against approved titles by name or alias. Episodes already recorded are left alone.
func BackfillFiled(ctx context.Context, st *State, root string) ([]string, error) {
	titles, err := st.Titles(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]int{}
	for i, t := range titles {
		if t.Status != Approved {
			continue
		}
		byName[strings.ToLower(t.Title)] = i
		if head, _, ok := strings.Cut(t.Title, ":"); ok {
			byName[strings.ToLower(strings.TrimSpace(head))] = i
		}
		for _, a := range t.Aliases {
			byName[strings.ToLower(a)] = i
		}
	}
	re := regexp.MustCompile(`^(.+) S\d+E(\d+) \[(\d+p)\]\.(mkv|mp4)$`)
	var added []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		m := re.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		i, ok := byName[strings.ToLower(m[1])]
		if !ok {
			return nil
		}
		ep := strconv.Itoa(atoiOr(m[2]))
		t := &titles[i]
		if _, done := t.Filed[ep]; done {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if t.Filed == nil {
			t.Filed = map[string]FiledEpi{}
		}
		t.Filed[ep] = FiledEpi{Path: path, Resolution: m[3], At: info.ModTime()}
		if err := st.PutTitle(ctx, *t); err != nil {
			return err
		}
		added = append(added, t.Title+" "+ep)
		return nil
	})
	return added, err
}

func atoiOr(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
