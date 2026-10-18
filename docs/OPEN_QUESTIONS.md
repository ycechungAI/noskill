# Decisions log (formerly open questions)

All Phase 0 questions are answered. Kept here so the reasoning stays visible. New questions go at the bottom under "Open".

| # | Question | Answer (2026-10-15) | Where it shows up |
|---|---|---|---|
| <a id="q1"></a>Q1 | What are "JEV-like models"? | Ollama's **JEV-style decision models** (`nimble`, `tev1`, `tev1:0.8b`, `/v1/systemone`, Ollama ≥ 0.35) | `decide` primitive: FR-B6/B6a/B6b, [MODEL_ADAPTERS.md](MODEL_ADAPTERS.md#jev-decision-models) |
| Q2 | Name | **noskill** | Checked: package names free; GitHub org `noskill` and main domains taken ([PACKAGING.md](PACKAGING.md#name-and-signing-checks)) |
| <a id="q3"></a>Q3 | Build vs extend Hermes Agent | **Standalone** | [ADR-0003](adr/0003-standalone-not-hermes.md) |
| Q4 | Core language | **Go** | [ADR-0001](adr/0001-core-language.md) (accepted) |
| Q5 | Anime setup | **qBittorrent RSS only**: no Sonarr, no media server, no list account | [anime-tracker](../skills/anime-tracker/SKILL.md) |
| Q5a | Anime file naming | **Simple**: `<folder>/<Title>/<Title> - 03.mkv` | anime-tracker `naming = "simple"` |
| Q5b | Downloaded copy after filing | **Move to OS trash** after hash-verified copy ("delete" never means permanent) | anime-tracker `after_filing = "trash"` |
| Q5c | Which anime get downloaded | **Only titles you approve**, one approval per title (season); its later episodes need no new approval. No automatic mode | anime-tracker `mode = "approve"`, FR-A6 |
| Q5d | Resolution | **480p when available**, else 720p, else 1080p, per episode after 12 h | anime-tracker `quality.prefer`, `fallback_after` |
| Q5e | Where files go | **You pick the folder in each approval** (e.g. `/Volumes/RED/ANIME2026`), prefilled with the last one; change it later by asking | anime-tracker `default_destination`, editable approval fields (FR-A6) |
| Q5f | Download and filing flow | **Download to `~/Downloads`, then one at a time: copy → verify → rename → remove torrent → trash the download**, until nothing finished is left | anime-tracker steps D, `file_mode = "after_seeding"` |
| Q6 | When to use learned skills | **Ask on first use, automatic after 3 successes** | [LEARNING.md](LEARNING.md#5-trial-and-promote) |
| Q7 | Hardware baseline | **16 GB, 4B–8B models** | [PLATFORMS.md](PLATFORMS.md#first-run-noskill-init) |
| Q8 | License | **MIT** | [LICENSE](../LICENSE) |
| Q9 | Cloud fallback | **Opt-in, later** (after v0.4, never for scheduled jobs) | FR-B7, Phase 5 |
| Q10 | Interfaces | **CLI, chat apps via proxy, tray app**. MCP dropped (2026-10-15) | FR-I1–I3 P0, FR-I7 P1, [ADR-0002](adr/0002-proxy-first-integration.md), [ADR-0004](adr/0004-tray-app.md) |

## Open (to verify in Phase 0 spikes, not user decisions)

- ~~**JEV `noul` semantics.**~~ Answered by [S3](spikes/S3-results.md): P(yes) on `tev1` (0.92 on yes items, 0.06 on no); on `tev1:0.8b` it's a score (0.80 / 0.22), so thresholds are calibrated per decision.
- ~~**`tev1` + 8B chat model on 16 GB.**~~ Answered by [S3](spikes/S3-results.md): it doesn't fit (14% free with a 4B chat model) and is too slow (3–5 s per routing call), so `tev1` is only for machines with more than 16 GB.
- **Ollama MLX prefix cache.** Each loaded MLX model's memory grows by ~8.5 GB as prompts are cached, ~30 MB per question per call (S3). noskill works around it by reloading the decision model ([ADR-0006](adr/0006-decision-model-on-16gb.md)). S4: GGUF (llama.cpp) models don't grow at all, and Ollama 0.40.3's server-config log lists no setting for an MLX cache cap, so the cap is still unconfirmed. Is the budget configurable in the Ollama version we target (reported as `OLLAMA_MLX_MAX_PAGED_OUT_BYTES`), and what cap keeps speed? Still to test wherever a cap exists (S4 had none to try).
- **qBittorrent Web API version differences** for `rss/matchingArticles`, `renameFile`, and torrent states after share limits (`pausedUP` vs `stoppedUP` in qBittorrent 5.x).
- **Tray on Linux.** Which desktops show StatusNotifierItem icons without extensions (GNOME needs an extension).
- **Windows script sandbox.** Can an AppContainer reach the localhost egress proxy without the admin-only loopback exemption? Until answered, domain allowlists are advisory on Windows.
- **Windows code signing.** Whether Azure Trusted Signing is available to this project, or an OV certificate is needed.
- **Chat-app progress display.** Which clients show `reasoning_content` / `thinking` progress, and which need `<think>` tags ([PROXY.md](PROXY.md#streaming)).

Each of these is covered by an experiment in [PHASE0_TESTS.md](PHASE0_TESTS.md) (S2, S3, S4, S6, S8, S9) or the Phase 0 setup checks.
