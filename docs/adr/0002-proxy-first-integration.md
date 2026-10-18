# ADR-0002: Integrate through the CLI, a compatible proxy and a tray app, not a new chat UI

Status: **accepted** (revised after Q10) · Date: 2026-10-15

## Context
"Easier integration" is a core goal. Users already have chat UIs (Open WebUI, LM Studio, Jan, Msty), editors (Continue, Zed, Cursor) and agents (Claude Code, Goose, Hermes). The project owner ranked CLI, chat apps via proxy, and a tray app as most important; MCP as less important.

## Decision
Integration surfaces, in priority order:
1. **CLI** (P0): the main way to run, schedule, approve and inspect. See [CLI.md](../CLI.md).
2. **Compatible proxy** (P0): an **OpenAI-compatible** endpoint and an **Ollama-compatible** port. Clients point at noskill instead of their backend; skills run server-side; model `noskill/auto` routes, and a real model name pins it.
3. **Tray/menu-bar app** (P1): status, jobs and approvals ([ADR-0004](0004-tray-app.md)).
4. **MCP** server + client (P1): skills for other agents, and MCP servers as tool sources.

No full chat UI is built. The daemon serves a small web status page (P1) that the tray opens.

## Consequences
- Integration = change one URL/port. Large reach for little effort.
- Both proxy flavours ship in v0.1, because many apps only speak Ollama's API.
- Server-side tool loops mean the client sees only the final streamed answer (plus optional progress as reasoning/thinking chunks). Clients that do their own tool calling can still pass `tools`; noskill then merges or passes them through (configurable).
- Need careful streaming compatibility testing against popular clients (Open WebUI, LM Studio as client, Continue, Msty).

## Evidence (S2, 2026-10-15)
[S2](../spikes/S2-results.md) confirmed the decision: one port serving `/v1/*` and `/api/*` worked with Open WebUI on both connection types, the OpenAI SDK, raw Ollama-format clients and the `ollama` CLI, adding 0.1–0.6 ms. Two consequences to carry into Phase 1: client-supplied tools can be huge (Open WebUI sends ~11k tokens of its own per request), so `merge` has a token budget; and chat apps send background requests (titles, tags, follow-ups) at the same model name, which the proxy runs directly on the cheapest model ([PROXY.md](../PROXY.md#measured-with-open-webui-s2)).

