# Prior art

Projects that already do parts of this. Checked 2026-10-15; several details come from secondary write-ups. **Verify against each repo before relying on a specific feature.**

## Closest matches: self-improving skill agents

| Project | What it does | Take from it | Where noskill differs |
|---|---|---|---|
| [NousResearch/hermes-agent](https://github.com/nousresearch/hermes-agent) | MIT-licensed self-improving agent. Writes skills after complex tasks (a background review once ≥ 10 tool iterations have passed, no verification; [S5](spikes/S5-results.md)), refines them in use, has a Curator that scores/merges/prunes learned skills (active → stale 30d → archived 90d), uses agentskills.io `SKILL.md`, supports Ollama. | Skill synthesis trigger, curator lifecycle, "never prune human-authored skills". **Study this first.** | Single Go binary, OpenAI/Ollama-compatible proxy so existing UIs gain skills, verification gate with tests before use, hardware-aware multi-model routing, grammar-constrained tools for small models, JEV decision models in the loop, native OS schedulers. **Decision: noskill is standalone** ([ADR-0003](adr/0003-standalone-not-hermes.md)); we only share the skill format and MCP with it. |
| [NousResearch/hermes-agent-self-evolution](https://github.com/NousResearch/hermes-agent-self-evolution) | Offline DSPy + GEPA optimisation of prompts/skill files. | FR-L9 approach. | — |
| [HKUDS/OpenSpace](https://github.com/HKUDS/OpenSpace) | "Self-evolving skills" (auto-fix, auto-improve, auto-learn) exposed to any MCP agent. Ollama support unconfirmed. | MCP-first delivery of skill evolution. | Local-model-first, own runtime. |
| [amazon-science/SAGE](https://github.com/amazon-science/SAGE) | Research (ACL 2026): RL for agents with a skill library. Reports +8.9% goal completion, −26% steps, −59% tokens on AppWorld. | Evidence that skill libraries cut steps/tokens; metrics for our eval. | Research code, not a product. |
| [MineDojo/Voyager](https://github.com/MineDojo/Voyager) | The original "LLM writes code skills into a library, verifies, reuses" (Minecraft). | Code-as-skill, self-verification, curriculum. | General desktop domain. |

## Agent runtimes and local-model hosts

| Project | Relevance |
|---|---|
| [block/goose](https://github.com/block/goose) | Cross-platform local agent with MCP extensions and desktop app; good reference for packaging and MCP UX. |
| [open-webui/open-webui](https://github.com/open-webui/open-webui) | Most popular Ollama UI; has Tools/Functions. Target **client** of our proxy. |
| [letta-ai/letta](https://github.com/letta-ai/letta) (MemGPT) | Long-term memory architecture; reference for FR-L6. |
| [huggingface/smolagents](https://github.com/huggingface/smolagents) | Code-acting agents that work with local models; reference for small-model prompting. |
| [openinterpreter/open-interpreter](https://github.com/openinterpreter/open-interpreter) | Local code-executing agent; lessons on sandboxing and UX. |
| [HKUDS/CLI-Anything](https://github.com/HKUDS/CLI-Anything) | Turns CLIs into agent skills; includes an Ollama wrapper skill. |
| [anthropics/skills](https://github.com/anthropics/skills) + [agentskills.io](https://agentskills.io/) | The open `SKILL.md` standard we're compatible with; progressive disclosure model. |
| [modelcontextprotocol](https://github.com/modelcontextprotocol) | MCP spec + SDKs (incl. Go). |

## Routing / gateways

| Project | Relevance |
|---|---|
| [lm-sys/RouteLLM](https://github.com/lm-sys/RouteLLM) | Trained routers between strong/weak models; claims up to 85% cost cut at 95% GPT-4 quality on MT-Bench. Last push reportedly Aug 2024. Ideas for difficulty estimation. |
| [paularlott/llmrouter](https://pkg.go.dev/github.com/paularlott/llmrouter) | Go gateway, MIT, Ollama provider + Ollama-compatible API, scriptable `auto` model. Closest to our proxy design; possibly reusable. |
| [BerriAI/litellm](https://github.com/BerriAI/litellm) | Proxy with fallbacks/load balancing across 100+ providers. |

## Research skill

| Project | Relevance |
|---|---|
| [assafelovic/gpt-researcher](https://github.com/assafelovic/gpt-researcher) | Planner → parallel searchers → report with citations. Template for `research`. |
| [ItzCrazyKns/Perplexica](https://github.com/ItzCrazyKns/Perplexica) | Local Perplexity-style search with SearXNG + Ollama. |
| [searxng/searxng](https://github.com/searxng/searxng) | Self-hosted metasearch; default private search provider. |
| Ollama web search API | `web_search` / `web_fetch` tools usable from local models (account/free tier). Optional provider. |

## Anime automation

| Project | Relevance |
|---|---|
| [AniList GraphQL API](https://docs.anilist.co/) | Discovery, seasonal lists, recommendations. Public queries need no account. |
| [qBittorrent Web API v2](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-(qBittorrent-4.1)) | **The user's actual setup.** RSS feeds/rules (`rss/setRule`, `rss/matchingArticles`), torrent info, `setLocation`, `renameFile`. anime-tracker's only downloader in v0.3. |
| [Sonarr](https://github.com/Sonarr/Sonarr) / [Prowlarr](https://github.com/Prowlarr/Prowlarr) | Most common download automation; anime support is awkward (absolute numbering, specials). Possible later adapter, not planned. |
| [matthewdias/transpondarr](https://pkg.go.dev/github.com/matthewdias/transpondarr) | Anime-first Sonarr alternative (Go, beta): add from AniList, grade by quality profile, grab via qBittorrent, hardlink into Plex/Jellyfin library. Automation off by default. |
| [ShokoAnime/ShokoServer](https://github.com/ShokoAnime/ShokoServer) | Identifies files by AniDB hash: robust naming/metadata. |
| [FileBot](https://www.filebot.net/) | Rename via AniDB/TheTVDB with anime-lists mapping (commercial). |
| [jellyfin-ani-sync](https://github.com/vosmiic/jellyfin-ani-sync) | Sync watch status to AniList/MAL. |

## Model types

| Project | Relevance |
|---|---|
| [Ollama JEV-style decision models](https://ollama.com/blog/ollama-now-supports-jev-style-decision-models) | Ollama ≥ 0.35 `/v1/systemone` (TypeSafe Jev API): typed `choice`/`noul`/`score` answers with probabilities; `nimble` (9B, Bespoke Labs), `tev1` (4B) and `tev1:0.8b` (Together AI). Python `typesafe-sdk`. Core of noskill's `decide` primitive. |
| [facebookresearch/vjepa2](https://github.com/facebookresearch/vjepa2), VL-JEPA ([arXiv 2512.10942](https://arxiv.org/abs/2512.10942)) | Embedding-predictive models. Possible P2 add-on for video tagging; not core. |

## Positioning summary

Nobody (that we found) combines all of these in one package:
1. a **drop-in proxy** that gives existing local-LLM UIs skills,
2. **verified** skill learning that works on **small** models,
3. **hardware-aware** multi-model orchestration on one machine,
4. **native** cross-OS daemon + scheduler + external-drive handling,
5. **JEV decision models** as a fast, calibrated judgement layer for routing, judging and safety, with an emulated fallback so it works on any model.

Hermes Agent covers learning well, so we study it closely but build standalone ([ADR-0003](adr/0003-standalone-not-hermes.md)). Interop stays cheap: noskill loads Hermes-created skills (same `SKILL.md` format) and Hermes can call noskill skills over MCP.
