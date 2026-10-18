package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"noskill/internal/tools"
)

// The anime-tracker's tools (skills/anime-tracker): the steps run in Go (internal/anime), so a
// model only starts a run and reads its report. The app implements AnimeService on its run services.

// AnimeService is what the anime tools need from the application. It is a separate interface so
// other tools' Services fakes need no change.
type AnimeService interface {
	// AnimeRun runs one stage ("answers", "suggest", "download", "file") or all of them ("all").
	// dryRun says what would happen and changes nothing.
	AnimeRun(ctx context.Context, stage string, dryRun bool) (string, error)
	// AnimeStatus lists the titles the tracker knows, as JSON.
	AnimeStatus(ctx context.Context) (string, error)
	// AnimeDetect lists the torrent apps found on this machine, as JSON.
	AnimeDetect(ctx context.Context) (string, error)
}

func animeSvc(env *tools.Env) (AnimeService, error) {
	if s, ok := env.Svc.(AnimeService); ok && s != nil {
		return s, nil
	}
	return nil, errors.New("the anime tracker isn't available in this run")
}

// Anime returns the anime-tracker builtins.
func Anime() []tools.Builtin {
	return []tools.Builtin{
		{ID: "anime_run", Description: "Run the anime tracker: suggest this season's new shows for the user to approve, download approved shows' new episodes through their torrent app, and file finished ones. Returns a report. Nothing is downloaded without the user's approval.",
			Schema: json.RawMessage(`{"type":"object","properties":{` +
				`"stage":{"type":"string","enum":["all","answers","suggest","download","file"],"default":"all","description":"One step, or all of them in order"},` +
				`"dry_run":{"type":"boolean","default":false,"description":"Say what would happen; change nothing"}}}`),
			Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (string, error) {
				s, err := animeSvc(env)
				if err != nil {
					return "", err
				}
				a := struct {
					Stage  string
					DryRun bool `json:"dry_run"`
				}{Stage: "all"}
				if len(raw) > 0 {
					if err := arg(raw, &a); err != nil {
						return "", err
					}
				}
				return s.AnimeRun(ctx, a.Stage, a.DryRun)
			}},
		{ID: "anime_status", Description: "List the anime titles the tracker knows: status (suggested, approved, skipped), resolution, episodes filed and the folder.",
			Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
				s, err := animeSvc(env)
				if err != nil {
					return "", err
				}
				return s.AnimeStatus(ctx)
			}},
		{ID: "anime_detect", Description: "Find the torrent apps on this machine (installed for this user or for everyone) and whether their remote access is on.",
			Run: func(ctx context.Context, env *tools.Env, _ json.RawMessage) (string, error) {
				s, err := animeSvc(env)
				if err != nil {
					return "", err
				}
				return s.AnimeDetect(ctx)
			}},
	}
}
