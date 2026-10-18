package anime

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

// testdata/releases.tsv: release names in the styles the user's feeds carry (SubsPlease, Erai-raws,
// scene WEB releases, batches, specials, movies, SD resolutions), with what they should parse to.
func TestParseReleaseFixtures(t *testing.T) {
	f, err := os.Open("testdata/releases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c := strings.Split(line, "\t")
		if len(c) != 9 {
			t.Fatalf("bad fixture %q", line)
		}
		n++
		season, _ := strconv.Atoi(c[3])
		ep, _ := strconv.ParseFloat(c[4], 64)
		end, _ := strconv.ParseFloat(c[5], 64)
		ver, _ := strconv.Atoi(c[6])
		r := ParseRelease(c[0])
		if r.Group != c[1] || r.Title != c[2] || r.Season != season || r.Episode != ep || r.EpisodeEnd != end || r.Version != ver || r.Resolution != c[7] || r.Kind != c[8] {
			t.Errorf("%s\n got  group=%q title=%q s=%d ep=%v-%v v=%d res=%q kind=%s\n want group=%q title=%q s=%d ep=%v-%v v=%d res=%q kind=%s",
				c[0], r.Group, r.Title, r.Season, r.Episode, r.EpisodeEnd, r.Version, r.Resolution, r.Kind,
				c[1], c[2], season, ep, end, ver, c[7], c[8])
		}
	}
	if n < 50 {
		t.Fatalf("only %d fixtures", n)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Frieren: Beyond Journey's End Season 2": "frieren beyond journeys end s2",
		"Frieren 2nd Season":                     "frieren s2",
		"Sousou no Frieren S02":                  "sousou no frieren s2",
		"SPY×FAMILY":                             "spy family",
		"Kaiju No. 8":                            "kaiju no 8",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
