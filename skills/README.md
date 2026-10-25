# Built-in skills (specs)

| Skill | Phase | What it does |
|---|---|---|
| [web-search](web-search/SKILL.md) | P0 · v0.1 | Search and read web pages, with sources |
| [research](research/SKILL.md) | P0 · v0.1 | Multi-step research → cited report |
| [storm](storm/SKILL.md) | P1 · v0.1 | STORM-style research from five viewpoints (practitioner, academic, skeptic, economist, historian), one at a time, merged into one cited report (block 5e). One tool call, `storm_run`: the loop runs in Go |
| [orchestrator](orchestrator/SKILL.md) | P0 · v0.1 | Pick and hand work to the right local model |
| [files](files/SKILL.md) | P0 · v0.2 | Scoped file operations, external drives, undo |
| [model-manager](model-manager/SKILL.md) | P0 · v0.1 | Suggest, pull, bench and clean up models for this machine |
| [skill-author](skill-author/SKILL.md) | P1 · v0.2 | Turns good runs into skills and fixes learned skills (the learning loop's writer) |
| [code-runner](code-runner/SKILL.md) | P1 · v0.2 (built in since block 2a) | Sandboxed Python for exact math and data files |
| [watcher](watcher/SKILL.md) | P1 · v0.3 | Scheduled watching of pages, feeds, releases, searches → alerts and digests |
| [local-docs](local-docs/SKILL.md) | P1 · v0.3 | Answer from folders you add with `noskill docs add`, citing the file and section (PDFs not read yet) |
| [anime-tracker](anime-tracker/SKILL.md) | P1 · v0.3 | This season's new anime → your approval + folder per title → English-subbed episodes from Nyaa through Transmission or qBittorrent (480p preferred) → copy, rename and file into your folder, download to trash |
| [downloads-tidy](downloads-tidy/SKILL.md) | P1 · v0.4 | Sort Downloads into folders by type; old installers to the trash (`tidy_plan`, then `tidy_apply` after one approval) |
| [duplicate-finder](duplicate-finder/SKILL.md) | P1 · v0.4 | Same-content files across folders and drives; `dupes_trash` re-checks the content before each trash |
| [disk-watch](disk-watch/SKILL.md) | P1 · v0.4 | Free space, biggest folders, large idle files; the `diskwatch` job alerts only when a drive is low |
| [media-renamer](media-renamer/SKILL.md) | P1 · v0.4 | One naming pattern for a folder's episodes, read with the anime tracker's parser (`rename_plan`, `rename_apply`) |
| [daily-brief](daily-brief/SKILL.md) | P1 · v0.4 | Morning digest of noskill's own state and plain-sentence reminders; the `brief` and `remind` jobs run in Go with no model |

Each `SKILL.md` has two parts:
- **front-matter + body**: what the model sees. Kept short and imperative for small models.
- an HTML comment `=== Implementation spec ===`: notes for whoever builds the tools. Stripped before the model sees the skill.

Format reference: [docs/SKILL_FORMAT.md](../docs/SKILL_FORMAT.md).
