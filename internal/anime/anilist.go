package anime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// AniListURL is AniList's public GraphQL endpoint; no account is needed.
const AniListURL = "https://graphql.anilist.co"

// AniList reads show data from AniList.
type AniList struct {
	URL  string // default AniListURL
	HTTP *http.Client
}

// Media is one AniList entry (one season or cour).
type Media struct {
	ID       int      `json:"id"`
	Romaji   string   `json:"romaji"`
	English  string   `json:"english,omitempty"`
	Native   string   `json:"native,omitempty"`
	Synonyms []string `json:"synonyms,omitempty"`
	Format   string   `json:"format,omitempty"` // TV, TV_SHORT, ONA, MOVIE, OVA, SPECIAL
	Episodes int      `json:"episodes,omitempty"`
	Status   string   `json:"status,omitempty"` // RELEASING, NOT_YET_RELEASED, FINISHED
	Season   string   `json:"season,omitempty"`
	Year     int      `json:"year,omitempty"`
	Genres   []string `json:"genres,omitempty"`
	Tags     []Tag    `json:"tags,omitempty"`
	Studios  []string `json:"studios,omitempty"`
	Score    int      `json:"score,omitempty"` // averageScore, 0–100
	Popular  int      `json:"popularity,omitempty"`
	Synopsis string   `json:"synopsis,omitempty"`
	URL      string   `json:"url,omitempty"`
	// Recs are AniList's recommendations from this show: media id → rating.
	Recs map[int]int `json:"recs,omitempty"`
	// Prequels are the ids this entry continues (a "Season 2" lists its season 1).
	Prequels []int `json:"prequels,omitempty"`
}

type Tag struct {
	Name string `json:"name"`
	Rank int    `json:"rank"` // 0–100: how much the tag applies
}

// Title is the name used for folders and approvals, in the user's preferred language.
func (m Media) Title(lang string) string {
	switch lang {
	case "romaji":
		return m.Romaji
	case "native":
		if m.Native != "" {
			return m.Native
		}
	}
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}

// Aliases are every name the show goes by, for matching release names.
func (m Media) Aliases() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range append([]string{m.English, m.Romaji}, m.Synonyms...) {
		n := Normalize(t)
		if n == "" || seen[n] || !latinish(t) {
			continue
		}
		seen[n] = true
		out = append(out, t)
	}
	return out
}

// latinish skips native-script synonyms: release names in the user's feeds are in Latin script.
func latinish(s string) bool {
	n, latin := 0, 0
	for _, r := range s {
		if r > ' ' {
			n++
			if r < 0x250 {
				latin++
			}
		}
	}
	return n > 0 && latin*10 >= n*8
}

const mediaFields = `id title{romaji english native} synonyms format episodes status season seasonYear genres
tags{name rank} studios(isMain:true){nodes{name}} averageScore popularity description(asHtml:false) siteUrl
relations{edges{relationType node{id}}} recommendations(perPage:8,sort:RATING_DESC){nodes{rating mediaRecommendation{id}}}`

type rawMedia struct {
	ID    int `json:"id"`
	Title struct {
		Romaji, English, Native string
	} `json:"title"`
	Synonyms   []string `json:"synonyms"`
	Format     string   `json:"format"`
	Episodes   int      `json:"episodes"`
	Status     string   `json:"status"`
	Season     string   `json:"season"`
	SeasonYear int      `json:"seasonYear"`
	Genres     []string `json:"genres"`
	Tags       []Tag    `json:"tags"`
	Studios    struct {
		Nodes []struct{ Name string } `json:"nodes"`
	} `json:"studios"`
	AverageScore int    `json:"averageScore"`
	Popularity   int    `json:"popularity"`
	Description  string `json:"description"`
	SiteURL      string `json:"siteUrl"`
	Relations    struct {
		Edges []struct {
			RelationType string `json:"relationType"`
			Node         struct{ ID int }
		} `json:"edges"`
	} `json:"relations"`
	Recommendations struct {
		Nodes []struct {
			Rating              int `json:"rating"`
			MediaRecommendation *struct{ ID int }
		} `json:"nodes"`
	} `json:"recommendations"`
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (r rawMedia) media() Media {
	m := Media{ID: r.ID, Romaji: r.Title.Romaji, English: r.Title.English, Native: r.Title.Native, Synonyms: r.Synonyms,
		Format: r.Format, Episodes: r.Episodes, Status: r.Status, Season: r.Season, Year: r.SeasonYear, Genres: r.Genres,
		Tags: r.Tags, Score: r.AverageScore, Popular: r.Popularity, URL: r.SiteURL}
	for _, s := range r.Studios.Nodes {
		m.Studios = append(m.Studios, s.Name)
	}
	d := strings.Join(strings.Fields(tagRe.ReplaceAllString(r.Description, " ")), " ")
	if rs := []rune(d); len(rs) > 600 {
		d = string(rs[:600]) + "…"
	}
	m.Synopsis = d
	for _, e := range r.Relations.Edges {
		if e.RelationType == "PREQUEL" {
			m.Prequels = append(m.Prequels, e.Node.ID)
		}
	}
	for _, n := range r.Recommendations.Nodes {
		if n.MediaRecommendation != nil && n.Rating > 0 {
			if m.Recs == nil {
				m.Recs = map[int]int{}
			}
			m.Recs[n.MediaRecommendation.ID] = n.Rating
		}
	}
	return m
}

func (a *AniList) query(ctx context.Context, q string, vars map[string]any) ([]Media, error) {
	body, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
	u := a.URL
	if u == "" {
		u = AniListURL
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		c := a.HTTP
		if c == nil {
			c = &http.Client{Timeout: 20 * time.Second}
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, fmt.Errorf("can't reach AniList: %w", err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests { // 90 requests a minute
			last = errors.New("AniList is rate limiting; try again in a minute")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 5 * time.Second):
			}
			continue
		}
		var out struct {
			Data struct {
				Page struct {
					Media []rawMedia `json:"media"`
				} `json:"Page"`
			} `json:"data"`
			Errors []struct{ Message string } `json:"errors"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("AniList: HTTP %d, unexpected reply", resp.StatusCode)
		}
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("AniList: %s", out.Errors[0].Message)
		}
		ms := make([]Media, 0, len(out.Data.Page.Media))
		for _, r := range out.Data.Page.Media {
			ms = append(ms, r.media())
		}
		return ms, nil
	}
	return nil, last
}

// Season lists a season's series (TV, TV short, ONA), most popular first.
func (a *AniList) Season(ctx context.Context, season string, year, n int) ([]Media, error) {
	q := `query($s:MediaSeason,$y:Int,$n:Int){Page(perPage:$n){media(season:$s,seasonYear:$y,type:ANIME,isAdult:false,
format_in:[TV,TV_SHORT,ONA],sort:POPULARITY_DESC){` + mediaFields + `}}}`
	return a.query(ctx, q, map[string]any{"s": season, "y": year, "n": n})
}

// Search finds shows by name, best match first.
func (a *AniList) Search(ctx context.Context, name string, n int) ([]Media, error) {
	q := `query($q:String,$n:Int){Page(perPage:$n){media(search:$q,type:ANIME,isAdult:false){` + mediaFields + `}}}`
	return a.query(ctx, q, map[string]any{"q": name, "n": n})
}

// ByID fetches shows by AniList id.
func (a *AniList) ByID(ctx context.Context, ids []int) ([]Media, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `query($ids:[Int],$n:Int){Page(perPage:$n){media(id_in:$ids,type:ANIME){` + mediaFields + `}}}`
	return a.query(ctx, q, map[string]any{"ids": ids, "n": min(len(ids), 50)})
}

// SeasonOf returns AniList's season name and year for t, and the one after it.
func SeasonOf(t time.Time) (season string, year int, next string, nextYear int) {
	names := []string{"WINTER", "SPRING", "SUMMER", "FALL"}
	i := (int(t.Month()) - 1) / 3
	season, year = names[i], t.Year()
	next, nextYear = names[(i+1)%4], t.Year()
	if i == 3 {
		nextYear++
	}
	return
}
