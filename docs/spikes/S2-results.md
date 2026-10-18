# S2 results: Proxy compatibility and overhead
2026-10-15 · Mac mini M4, 16 GB, macOS 27.0.1 · Go 1.27.2 · Ollama 0.40.3 · Open WebUI 0.12.2 (`uvx`, Python 3.12, single-user mode) · harness [spikes/s2](../../spikes/s2/) (`run.sh` runs everything except Open WebUI and the real-Ollama check)

**What was built:** a minimal Go proxy (~500 lines, stdlib only) on one port serving `/v1/*` and `/api/*`. Plain model names pass straight through. `noskill/<anything>` runs a small agent loop with one server-side tool (`get_time`), streams its progress as reasoning, then streams the answer. It checks Host and Origin. It isn't the product; it exists to find out what the product's proxy has to handle.

**Deviations from the plan:**
- **Clients.** You chose scripted clients over a second chat app, so:
  - Open WebUI, driven by hand in the in-app browser: 3 chats per connection type (a tool step, a stopped answer, a 5,000-token answer), not 100.
  - 100 scripted runs each of the OpenAI Python SDK (OpenAI connection) and raw HTTP in Ollama format. 10 runs of the `ollama` CLI.
- **Overhead** was measured against a mock backend on localhost that streams fixed tokens, as planned. The one tool step in the agent path is a call to the mock, not a model.
- **Real Ollama** got a separate single pass (`qwen3.5:4b`) to check the real streaming format, a real tool call and a real cancel.
- **Downloads** (you approved Go and `uvx open-webui`): Go 240 MB via Homebrew; the `openai` package and a 24 MB CPython 3.12 via `uv`; Open WebUI via `uvx`, which has grown the `uv` cache to 3.8 GB.

## Numbers

### Protocol checks (mock backend)
A "run" is one pass through every scenario for that client. Scenarios: model list, non-stream, stream, thinking, 5,000-token answer, tool step, stop mid-stream (backend must see the cancel within 1 s), plain model name, error format. Raw HTTP adds `show`, `version`/`ps` and `/api/generate`.

| Client and connection | Runs | Scenarios per run | Errors |
|---|---|---|---|
| OpenAI Python SDK, OpenAI connection (`/v1`) | 100 | 9 | 0 |
| Raw HTTP, Ollama connection (`/api`) | 100 | 11 | 0 |
| `ollama` CLI (`list`, `show`, `run`, `run` plain) | 10 | 4 | 0 |
| Keepalives (proxy pings every 1 s, backend silent 3 s), SSE and NDJSON | 5 | 1 | 0 |
| Open WebUI, OpenAI connection (by hand) | 3 chats | tool step, stop, 5,000 tokens | 0 |
| Open WebUI, Ollama connection (by hand) | 3 chats | tool step, stop, 5,000 tokens | 0 |

The first full run was not clean, and the failures found real gaps (see Findings). After fixing the proxy and one bug in the test, the second run above had 0 errors.

### Overhead (2,000 requests per target, interleaved, mock streams 40 tokens with no delay)
| Target | Non-stream p50 / p99 ms | First content token p50 / p99 ms | Stream total p50 / p99 ms |
|---|---|---|---|
| Direct to the mock | 0.088 / 0.146 | 0.084 / 0.140 | 0.599 / 0.841 |
| Proxy, plain name (passthrough) | 0.186 / 0.273 | 0.185 / 0.287 | 0.711 / 1.008 |
| Proxy, `noskill/auto` (agent loop, no tool call) | 0.718 / 1.004 | 0.215 / 0.347 | 0.866 / 1.181 |

Added by the proxy: **+0.10 ms p50 / +0.13 ms p99** (non-stream, passthrough), **+0.63 / +0.86 ms** (non-stream, agent), and **+0.10 / +0.15 ms** and **+0.13 / +0.21 ms** on the first content token. A tool step adds a second call to the backend, which is model time, not proxy time.

### Cancel
| Test | Result |
|---|---|
| Client closes a stream, 30 runs per target, mock checks every 20 ms | Backend noticed after **22 ms** (direct, passthrough and agent are all the same) |
| Open WebUI **Stop**, both connection types | Backend noticed at once (logged within the same 20 ms as the click) |
| Real Ollama, `qwen3.5:4b`, 3,000-word story, client closes after 15 chunks | Ollama finished the request **0.2 s** later (its log shows 1.6 s for a story that would take minutes) |

### Real Ollama (`qwen3.5:4b`, one pass)
- A real tool call works end to end: the model called `get_time`, the proxy ran it, and the answer used the result ("The current time is 10:17:28 EDT.", 2.1 s with thinking off).
- Thinking tokens are forwarded and show in Open WebUI as a collapsed "Thought" block, with the proxy's "Calling get_time…" line inside it.
- Ollama's `/v1` stream names the thinking field **`reasoning`**, not `reasoning_content`. The proxy reads both and sends `reasoning_content`.
- With thinking on, `qwen3.5:4b` thought for more than 10 minutes on a one-line tool question. `reasoning_effort: "none"` on `/v1` turns it off and the tool call comes back at once.

## Findings (what the real proxy must do)
1. **`HEAD /` and `GET /`.** The `ollama` CLI starts with `HEAD /` and stops with "something went wrong" if it gets a 404. The proxy answers both itself ("noskill is running").
2. **Model digests must look real.** `ollama list` crashes (a slice out of range) when a model's `digest` is shorter than 12 characters. Synthetic `noskill/*` entries get a 64-character hex digest.
3. **`ollama run <model> "prompt"` uses `/api/generate`, not `/api/chat`.** The reply shape differs (`response` instead of `message.content`, and `thinking` at the top level). Implemented for the spike.
4. **Thinking.** Internal steps (tool steps, background tasks) send `reasoning_effort: "none"` unless the client asked for thinking. Client `reasoning_effort`, `max_tokens`, `temperature`, Ollama `think: false` and `options.num_predict` are passed on.
5. **Open WebUI sends background requests to the same model**, each starting "### Task:": a title, follow-up suggestions and tags after the first message (3 requests), then follow-up suggestions after every later turn. With a real model each would run the whole agent loop and queue behind the chat. The proxy has to recognise them and run them directly on the cheapest model, with no skills, tools or routing.
6. **Open WebUI attaches its own built-in tools to every chat request** (`get_current_timestamp`, `calculate_timestamp` and more), on both connections. The chat request for "what time is it" was **45 KB, roughly 11,000 tokens**, against 2.7 KB for its background tasks. That alone overflows an 8k context. `client_tools = "merge"` can't forward them blindly to a small model. The spike ignored client tools, so this is untested beyond the size.
7. **Which endpoints Open WebUI uses:** OpenAI connection `GET /v1/models`, `POST /v1/chat/completions`. Ollama connection `GET /api/tags`, `GET /api/ps`, `POST /api/chat`. It never called `/api/show` or `/api/version`. It sends the conversation history every turn, as PROXY.md assumes (fingerprinting works on it).
8. **Keepalives** work in both formats (SSE comment lines, NDJSON empty chunks).

## Pass/fail
| Rule | Result |
|---|---|
| 0 protocol errors in 100 runs per client and connection type | **Pass** for the SDK and raw Ollama format (100 runs, 0 errors), the `ollama` CLI (10) and Open WebUI on both connections (3 chats each). Caveat: Open WebUI was 3 chats, not 100, and the first full run needed three proxy fixes (findings 1–3) |
| Added latency p50 < 20 ms, p99 < 60 ms; time-to-first-token unchanged ± 10 ms | **Pass:** +0.1 to +0.6 ms p50, +0.2 to +0.9 ms p99, on a localhost mock |
| Cancel in the client stops generation in Ollama within 1 s | **Pass:** 0.2 s with real Ollama; Open WebUI's Stop reached the backend at once |
| Server-side tool use is invisible to the client except as optional progress/thinking text | **Pass** for the server-side tool (shows only as a "Thought" block). **Not met for client tools** (finding 6) |

## Decision
- **ADR-0002 stands:** one port serving `/v1/*` and `/api/*` works with Open WebUI on both connection types, the OpenAI SDK, raw Ollama-format clients and the `ollama` CLI. There's no need for a separate Ollama port.
- [PROXY.md](../PROXY.md) now covers the root probe, digests, `/api/generate`, thinking field names and effort, background-task detection, and a client-tool budget ([CONFIG.md](../CONFIG.md): `client_tools_budget_tokens`).
- **Not tested here** (Phase 1 tests): approvals in chat, `client_tools = "merge"` round trips with the fingerprint cache, Host/Origin/token behaviour beyond a smoke check, memory use, and the other clients in the compatibility matrix.
