---
name: anime-tracker
description: Find this season's new anime the user is likely to enjoy, ask the user to approve each title and choose its folder, download its English-subtitled episodes from Nyaa through the user's torrent app (Transmission or qBittorrent, 480p preferred), then rename and file finished episodes into the chosen folder. Use for scheduled anime discovery or when the user asks what new anime to watch or to track a show.
license: MIT
noskill:
  version: 0.11.0
  origin: builtin
  status: active
  task_types: [research, extract, plan]
  min_model: { context: 8192, tools: any }
  prefer_models: ["qwen3:4b", "qwen3:8b"]
  depends_on: [files]
  schedule_default: "0 3 * * *"
  permissions:
    network: ["graphql.anilist.co", "${config.nyaa_url}", "localhost"]   # localhost: the torrent app's Web UI / RPC
    filesystem:                              # the Go steps check their own folders (the download_dir setting, and only folders you
      - "read:$DOWNLOADS"                    # picked in approvals); these lines are what the skill shows as its default reach.
      - "write:$DOWNLOADS"                   # They must resolve with no settings, or a model-driven run fails before it starts.
    exec: false
    secrets: ["QBITTORRENT_USER?", "QBITTORRENT_PASS?", "TRANSMISSION_USER?", "TRANSMISSION_PASS?"]
  # No confirm list: approvals are created by the Go steps, one card per title (folder included).
  tools:
    - builtin: anime_run      # { stage?: all|answers|suggest|download|file, dry_run? } → report
    - builtin: anime_status   # titles the tracker knows: status, resolution, episodes filed, folder
    - builtin: anime_detect   # torrent apps found on this machine
  decisions:
    same_series:
      noul: "Is this RSS item a release of the given anime series (not a different show, season or spin-off)?"
      threshold: 0.85
    release_kind:
      choice:
        instructions: "What kind of release is this?"
        criteria:
          episode: "A single episode"
          batch: "Several episodes or a full season in one release"
          special: "OVA, ONA, special or recap"
          movie: "A film"
          other: "Not anime video, or unclear"
    would_enjoy:
      score:
        instructions: "Given the user's taste profile, how much would they enjoy this show?"
        criteria: ["Not for them", "Unlikely", "Maybe", "Likely", "Very likely"]
---

# Anime tracker

The steps run in Go (`internal/anime`, `noskill anime run`); a model only starts a run and reads its report.

**Nothing is downloaded without the user's approval.** Approval is per title (one season or cour). After a title is approved, its new episodes download and get filed without asking again. A new season is a new title and needs a new approval.

## Steps (scheduled run)

### A. Suggest and ask
1. Read the taste profile from memory (`anime.taste`). If it's missing, build it (see "Taste") and ask the user to confirm it once.
2. Call `anilist` for the current and next season, and last season's shows that are still airing. Only these **new** shows are ever suggested: finished and older shows never are.
3. Drop titles the user **already has** (a matching show in any of their anime folders or `library_dirs`), already approved, skipped, or marked "not interested".
4. Call `taste` to score the rest. Keep titles with score ≥ `min_score`, at most `max_new_per_run`.
5. For each kept title, search Nyaa's English-translated category to see which resolutions are out for it (checked with `decide("same_series")`). Pick the resolution by the user's preference order (default 480p, then 720p, then 1080p).
6. For each kept title, write one line on why the user might like it (genres, studio, staff, similar titles), then create **one approval** with: title, why, the resolution that will be downloaded, and the **folder to save it in** (prefilled with the last folder the user chose; the user can change it).
   - `notify-only` mode: send the list without approvals. Stop here.
7. Stop and wait. Approvals that time out count as "not now": the title is offered again on the next run, at most 3 times.

### B. When a title is approved
8. Save the chosen folder for that title (`set_destination`). Choosing a folder in the approval is what gives this skill permission to write there.
9. Build a rule (see "Rule building"), test it with `test_rule`, then `set_rule`. The rule downloads into `download_dir` (default `~/Downloads`) under category `noskill-anime`.

### C. Download (every run)
10. For each approved title, search Nyaa's English-translated category (`nyaa_url`) for uploads from the last `max_age` (default 100 days). Single episodes only: batches, and episodes numbered across seasons, are skipped. Skip every episode the user **already has** (on disk or filed before) or that the torrent app already has. Add the preferred resolution; if an episode has been out `fallback_after` (default 12 h) **only in other resolutions**, add the best one in preference order. Torrents get the label/category `noskill-anime`.

### D. File finished episodes (every run, and when qBittorrent reports a finished torrent)
11. List finished torrents labelled `noskill-anime`. Process them **one at a time**, oldest first:
    1. **Stop the torrent seeding** (pause/stop it in the torrent app; its files stay). Filing never waits for a share limit.
    2. Plan the name: `<folder>/<Title>/<Title> S02E03 [480p].mkv`.
    3. **Rename the file in the download folder to that name first**, then copy it into the title's folder under a temporary name, verify the copy's hash, and move the copy into place (`files.copy_verify`). If the rename can't happen, the copy still gives it the final name.
    4. Remove the torrent from the app, keeping its data (`remove_torrent`).
    5. Move the original in `~/Downloads` to the OS trash (`files.trash`).
    6. Next one.
12. Stop when nothing finished is left. Downloads still running are filed when qBittorrent reports them finished, or on the next run.

### E. Report
13. New picks and their approvals, rules added or changed, episodes that fell back to another resolution, what was filed where, anything skipped and why.

## Followed shows (donghua and other long-running series)
Shows named in config (`[[skills.anime-tracker.follow]]`, the config entry is the approval: no card) skip the "new this season" filter and the 480p / size rules. Each week the tracker fetches the **one newest episode, as the smallest English-subbed file** on Nyaa, and files it flat into the folder as `<name> Episode <n>.<ext>` (the user's own naming). It never fetches older episodes: it starts after `from` (or the newest episode already in the folder, or the newest out now). Keys: `title`, `match` (words a release must contain), `dest`, `name`, `search`, `from`, `window_days` (default 10). Example: `title = "Renegade Immortal"`, `match = ["renegade immortal", "xian ni"]`, `dest = "volume:RED/@DONG+"`, `from = 162`.

## Taste (no account needed)
Build the profile from what the user already has:
- show folders already in their chosen anime folders,
- titles the user approved (genres, tags, AniList recommendations from them, rating).
With nothing approved yet, it goes by rating and popularity. The score is deterministic Go (`anime.Taste`); see the implementation notes.

## Naming (default `simple` preset)
Just the show's name, the episode and the resolution:
- Episode: `<folder>/<Title>/<Title> S02E03 [480p].mkv`, e.g. `/Volumes/RED/ANIME2026/Frieren Season 2/Frieren Season 2 S02E03 [480p].mkv`
- `episode_style = "ep"` gives `<Title> EP 3 [480p].mkv`; `"episode"` gives `<Title> Episode 3 [480p].mkv`.
- Special/OVA: `<Title> S00E01 [480p].mkv` (or `SP 1` / `Special 1` with the styles above)
- Movie: `<folder>/<Title>/<Title> (2026).mkv`
- Each season or cour uses its own AniList title (e.g. `<Title> Season 2`), so it gets its own folder.
- In `SxxExx` names episode numbers have 2 digits, or 3–4 if the show has 100+ episodes.
- `title_folders = false` puts files straight into `<folder>` without a per-title subfolder.
Other presets: `plex-jellyfin` (`<Title (Year)>/Season 01/<Title> - S01E03 - <Episode Title> [480p].mkv`) or a custom template.

## Rules
- Never download a title the user hasn't approved.
- Only read the source the user configured: Nyaa's "Anime - English-translated" category at `nyaa_url` (default `https://nyaa.si`; where that's blocked, a mirror such as `https://nyaa.media`). Never search other sites. Links must be on that site, or a magnet read from the item's page there.
- Only suggest new shows (this season or next, or last season and still airing), and never a show or episode the user already has.
- Only manage torrents labelled (Transmission) or categorised (qBittorrent) `noskill-anime`. Never touch the user's own torrents. Removing a torrent always keeps its files.
- English subtitles only: drop an upload tagged for another language only (`[CHS]`, `BIG5`, `Portuguese`…) unless it also says English or multiple subtitles (Erai-raws' "Multiple Subtitle").
- 720p and above only when the file is 500 MB or less (`quality.hd_max_size_mb`, 0 = no limit); an unreadable size counts as too big. A title whose only releases are big HD files is neither offered nor downloaded.
- At 480p try Erai-raws first (`quality.prefer_groups`), then any other group (SubsPlease is fine). Every group's file is renamed the same way (`<Title> S02E03 [480p].mkv`).
- Earlier seasons: when a **complete 480p batch** of an earlier season of a show you follow is newly uploaded (last 45 days) and you have none of that show, send an approval card (size, group, the later season's folder prefilled) and download only after you approve. Offered once per season.
- Prefer 480p. Use another resolution only when the title or episode has no 480p release in the user's feeds (after `fallback_after`), and say so in the approval or report.
- Write only into folders the user chose in an approval. If that folder's drive isn't mounted, don't file anything: downloads wait in `~/Downloads`, and the report says so. Retry next run.
- Never overwrite files. If a fixed release (v2) of the same episode and resolution arrives, move the old file to the trash first.
- Stop a finished torrent from seeding before filing it. Only `noskill-anime` torrents, never the user's own.
- Rename a download to its final name before it is moved.
- Move a download to the OS trash only after its copy in the destination is verified. Never delete permanently. Never trash the download folder itself: a torrent without a folder of its own has only its placed videos trashed.
- A file the user changed after noskill filed it (re-encoded, remuxed, subtitles added) is theirs: a fixed release doesn't replace it.
- If unsure which series or episode a file belongs to, skip it and list it in the report.

<!--
=== Implementation spec ===

Implemented (Block 3): internal/anime, `noskill anime detect | scan | run [--stage answers|suggest|download|file] [--dry-run] | status`.
Deviations from the design below, by the user's request (2026-10-16):
  - Source: Nyaa's English-translated category (1_2) read by noskill, not feeds in qBittorrent. Mirrors
    without links in their feed (nyaa.media) are resolved from the item page's magnet link.
  - Clients: Transmission (RPC, labels, Transmission 3.00+) and qBittorrent (Web API v2), detected
    installed per user and system wide, and by their local Web UI/RPC (`noskill anime detect`).
    noskill never changes the app's settings; it says where to turn remote access on.
  - Both clients use the same path: noskill matches episodes itself and adds them (`download` stage).
    qBittorrent RSS rules (BuildRule/SetRule) are built and tested but not used yet, so new episodes
    arrive when the job runs: schedule it hourly for quick pickup.
  - Taste is deterministic (genres, tags, AniList recs, rating; popularity when cold), not embeddings or
    decide(would_enjoy); the LLM doesn't score.
  - Episodes numbered across seasons (SubsPlease "Show - 50" for season 3) are skipped rather than mapped.
  - Filing copies through perms.Broker (verify, never overwrite, undo log) and trashes the download.
    Before each copy the destination drive needs the file's size plus 5% free (platform.DiskFree); if
    not, nothing is copied, the torrent stays and the report says how much is needed and free.
    A fixed release (v2) of an episode noskill filed, at the same resolution, is downloaded and replaces
    it when `replace_versions` is on (default): the old file goes to the OS trash first. Files noskill
    didn't file, or that were modified after the filing, are never replaced. If the copy then fails, the
    report says the earlier release is in the trash and the fixed one is retried on the next run.
    After a run that filed files, the report ends with `noskill undo anime-<id>`, which puts them back.
    Leftover `.part` files of a copy that died are moved to the trash after 5 minutes, then it retries.
    The library scan gives each folder 45 seconds; a hung network share is left out and named in the
    report. Titles are made safe as names: no leading dot (`.hack//SIGN` → `hack--SIGN`), no Windows
    device names, at most 200 bytes. `download_dir` is checked like an approved folder (not a home
    folder, a drive root or a system folder).
  - Earlier-season batches (2026-10-18, by the user's request: "ask me first"): stage `batches`, internal/anime/batch.go.
    For each approved title it walks AniList's PREQUEL chain (cached a week, up to four seasons back), skips
    shows already in the library or already offered, and offers a batch that is 480p, English, covers the season
    (EpisodeEnd >= AniList's episode count) and is within `BatchWindow` (45 days). Approved batches are added to
    the torrent app once and filed episode by episode like any download. Downloading without asking is not built.
  - Approval cards stay open to the end of the broadcast season (Winter 31 Mar, Spring 30 Jun, Summer 30 Sep,
    Fall 31 Dec; at least a day), by the user's request, so a "maybe" can wait. `approval_retries` still
    covers cards that do time out.
  - Followed shows (2026-10-18, by the user's request): internal/anime/follow.go. A TitleState with a negative
    id and `Follow` set; filing matches it by the release name's `match` words instead of AniList titles.
    New episodes of Renegade Immortal come out on Sundays about 1 pm Eastern, so a job runs the
    download and file stages on Sunday afternoons and evenings (see docs/WEEK_RUN.md).
  - Release preferences (2026-10-18, by the user's request): `quality.hd_max_size_mb` (default 500),
    `quality.prefer_groups` (default ["Erai-raws"]; the group is only a tie-break at the same resolution,
    so a night's run doesn't wait for Erai-raws to post), and an English-subtitles guard on top of
    Nyaa's English-translated category. Nyaa's size text is parsed as MiB/GiB.
  - Stop and rename (2026-10-18, by the user's request): each finished torrent is stopped from seeding
    (`torrent-stop`; qBittorrent `torrents/stop`, falling back to `pause` on v4) and the report lists it
    under "Stopped seeding". The video is renamed in the download folder (broker move, in the undo log)
    before the verified copy; a retry after a failed copy finds it under its new name. This replaced
    "wait for the share limit", so the share-limit wording in the design below no longer applies.
  - `mode = "notify-only"` sends the suggestions as one notification (the `notify` channels, else all)
    and creates no approval cards, so nothing is recorded or downloaded.
  - "Already stored" scans Videos, Downloads, Desktop, library_dirs, approved folders and every external
    drive (read only; Time Machine folders skipped); `anime scan` confirms titles on AniList (cached),
    and confirmed shows join the taste profile.
  - Model-driven runs: builtins `anime_run`, `anime_status`, `anime_detect` (the model starts a run and reads its report;
    the earlier design's anilist/taste/qbittorrent/library tools stay inside Go).
  - `noskill hook qbittorrent-finished "%I"` (2026-10-18): files that one torrent now (the file stage limited to its hash), so an
    episode doesn't wait for the next run. `transmission-finished` and `torrent-finished` are the same command; with no hash
    argument it reads `TR_TORRENT_HASH`, which Transmission sets. It waits up to 3 minutes for the filing lock if a run is
    filing, touches only noskill's own category, and does nothing for any other hash. Scheduled runs still file anything it
    missed (app closed, drive unplugged). Transmission calls it through `scripts/transmission-done.sh`.

Config (config.toml):
  [skills.anime-tracker]
  client = "auto"                   # auto | transmission | qbittorrent; auto = Transmission first, qBittorrent only as a backup
  nyaa_url = "https://nyaa.si"      # https only; a mirror where nyaa.si is blocked
  trusted_only = false              # Nyaa's trusted-uploader filter
  max_age = "2400h"                 # only uploads this recent (default 100 days)
  library_dirs = []                 # other folders where you keep anime: read to skip what you have
  transmission_host = "localhost:9091"
  category = "noskill-anime"        # Transmission label / qBittorrent category
  mode = "approve"                  # approve | notify-only   (there is no automatic mode)
  default_destination = ""          # first approval asks; afterwards the last chosen folder is the default
  title_folders = true
  naming = "simple"                 # simple | plex-jellyfin | custom template string
  episode_style = "sxxexx"          # simple names: sxxexx (S02E03) | ep (EP 3) | episode (Episode 3)
  title_language = "english"        # english | romaji | native
  min_score = 0.7
  max_new_per_run = 5
  qbittorrent_host = "localhost:8080"
  qbittorrent_category = "noskill-anime"
  download_dir = "$DOWNLOADS"       # qBittorrent save path for the category
  feeds = []                        # [] = all feeds in qBittorrent; or a list of feed names/paths
  quality = { prefer = ["480p", "720p", "1080p"], fallback_after = "12h", prefer_groups = [], avoid = ["HEVC 10bit"] }
  file_mode = "after_seeding"       # after_seeding | qbit_move
  after_filing = "trash"            # trash | keep   (trash = OS trash, recoverable; only after hash verify)
  replace_versions = true           # v2/v3 of the same episode and resolution replace v1 (old file goes to trash)
  approval_retries = 3              # times a timed-out title is offered again
  notify = ["os"]

State (skill_kv, see DATA_MODEL.md):
  titles.<anilist_id> = { status: pending|approved|skipped|not_interested|finished, destination,
                          resolution, rule_name, offered: n, approved_at }
  last_destination
  destinations = [ ... ]            # granted write scopes, each bound to a volume (label + UUID) when on an external drive

Approval card (approvals queue; CLI, notification, tray, status page, chat):
  Add "Frieren: Beyond Journey's End Season 2"?                    anime-tracker · expires in 24h
  Why: slow, reflective fantasy like Frieren S1 and Mushishi; same studio (Madhouse).
  Releases in your feeds: SubsPlease 480p ✓ (also 720p, 1080p)
  Save to: /Volumes/RED/ANIME2026/Frieren Beyond Journey's End Season 2/        [Change folder…]
  [Approve]  [Skip]  [Not interested]
  Editable field: dest. CLI: noskill approvals approve <id> --set dest=/Volumes/RED/ANIME2026
  Chat: "yes, save it to /Volumes/RED/ANIME2026". Tray: Change folder… opens the approval on the status page
  with a folder picker.
  Changing a title's folder later: ask ("save Frieren to /Volumes/RED/ANIME2027 from now on") → set_destination
  (confirm). Already-filed episodes are not moved unless asked.

Destinations on external drives:
  /Volumes/RED/ANIME2026 is stored as volume:RED (label + volume UUID) + "ANIME2026", so it still resolves if the
  mount path changes (/Volumes/RED 1) and on Windows (drive letter) / Linux (/run/media/...). Check free space
  (file size + 5% reserve) before each copy. Name sanitisation per target filesystem (exFAT/NTFS).

qBittorrent tool (Web API v2):
  Auth: recommend enabling "Bypass authentication for clients on localhost" in qBittorrent;
        otherwise POST /api/v2/auth/login with credentials from the OS keychain. Never stored in config.
  rss_feeds()          GET  /api/v2/rss/items?withData=false     → feed names/urls the user configured
  rss_items(feed)      GET  /api/v2/rss/items?withData=true      → recent articles (title, torrentURL, date)
  find_releases(title) rss_items + library.parse + decide(same_series) → {resolution: [items]} for the title
  test_rule(rule)      POST /api/v2/rss/setRule (temp name "noskill: __test__") +
                       GET  /api/v2/rss/matchingArticles?ruleName=…  → matched titles; then removeRule
  set_rule(rule)       POST /api/v2/rss/setRule  ruleName="noskill: <Title>", ruleDef = {
                         enabled, mustContain (regex: title aliases AND resolution pattern), mustNotContain,
                         useRegex: true, episodeFilter, smartFilter: true, affectedFeeds,
                         assignedCategory: <qbittorrent_category>, savePath: <download_dir>,
                         previouslyMatchedEpisodes }
  add_torrent(item)    POST /api/v2/torrents/add urls=<torrentURL> category=<cat> savepath=<download_dir>
                       (fallback episodes only, for approved titles only)
  mark_episode(rule,e) setRule with e added to previouslyMatchedEpisodes so the smart filter skips it
  remove_rule(name)    POST /api/v2/rss/removeRule (only "noskill: " rules; when a title finishes airing)
  list_completed()     GET  /api/v2/torrents/info?category=<cat>&filter=completed
  torrent_files(h)     GET  /api/v2/torrents/files?hash=…
  set_location(h,p)    POST /api/v2/torrents/setLocation             (qbit_move mode only)
  rename_file(h,o,n)   POST /api/v2/torrents/renameFile               (qbit_move mode only)
  remove_torrent(h)    POST /api/v2/torrents/delete deleteFiles=false (only category <cat>; files are trashed by us)
  Verify exact endpoint/field names against the user's qBittorrent version (GET /api/v2/app/webapiVersion),
  especially previouslyMatchedEpisodes and the torrentURL article field (Phase 0 S6).

Resolution patterns (case-insensitive):
  480p: 480p|848x480|854x480|640x480     720p: 720p|1280x720     1080p: 1080p|1920x1080
  A title's rule uses the first resolution in quality.prefer that its feeds carry. Weekly re-check: if a title
  on a fallback resolution gains 480p releases, switch the rule for future episodes and say so in the report.

Rule building:
  - Aliases from AniList (english, romaji, synonyms) → escaped regex alternation.
  - Draft → test_rule → for each matched article: decide(same_series, {series, aliases, item_title})
    and decide(release_kind). False positives (same_series < threshold) → add mustNotContain terms.
    Unmatched feed items with same_series ≥ threshold → widen aliases. Max 3 iterations, else report.
  - Rules are rechecked weekly against new feed items (curator-style), and fixes go through `confirm`.

Filing (file_mode):
  after_seeding (default): one torrent at a time: wait for qBittorrent's share limits if the user set any
    (state stoppedUP/pausedUP), else proceed at once → files.copy_verify to <dest>/<Title>/<name>.part,
    xxhash verify, rename to final name → remove torrent (keep data) → files.trash the original
    (after_filing = "trash") → next. Robust if the drive is unplugged: nothing is removed until the copy is
    verified. Needs free space for one extra copy at a time.
  qbit_move: qBittorrent moves the data into the destination and renames in place, so seeding continues from
    the drive. Needs the drive always mounted; doesn't use ~/Downloads after the move.
  Instant trigger: qBittorrent "Run external program on torrent finished":
    noskill hook qbittorrent-finished "%I"   (same command on all OSes; queues a filing run)

Taste model (taste tool):
  - Profile = weighted genres/tags/studios/staff + embedding centroid of liked synopses.
  - score = 0.4·cosine(synopsis_emb, liked_centroid) + 0.25·tag overlap + 0.15·AniList rec strength
            + 0.2·decide(would_enjoy) normalised; minus penalties for disliked tags.
  - Weights updated from approve/skip/not-interested feedback. The LLM only writes the "why" lines.

Library tool: anitopy-style release-name parser in Go (group, title, ep, res, codec, batch, version);
  match to AniList id via aliases + fuzzy match + decide(same_series) when ambiguous;
  simple preset: number = episode within the AniList entry; if a release uses absolute numbering across
  entries, map via anime-lists XML / AniList relations. Confidence < 0.9 → skip and report.

Preconditions: qBittorrent reachable; destination volume mounted (for filing only); not on battery (configurable).
Outcome checks: each filed file exists at destination with matching hash and the original is in the trash;
  name matches template regex; no torrent outside category <cat> was touched; every noskill rule still parses
  and matches ≥ 1 expected item or is flagged; no unapproved title has a rule.

Tests: 200 real-world release-name fixtures (incl. 480p/SD variants); rule-building fixtures with recorded feeds
  (look-alike titles, sequels, titles with no 480p); fallback timing and no-duplicate checks; approval with a
  changed folder; dry-run move plans; drive unplugged mid-copy; mount path change (/Volumes/RED 1); duplicate
  files; exFAT/NTFS name sanitisation; qBittorrent API mocked per webapiVersion.
-->
