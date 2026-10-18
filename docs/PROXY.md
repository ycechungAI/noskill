# Proxy and chat apps

How chat apps, editors and scripts get skills without changing anything but a URL (FR-I2, FR-I3, [ADR-0002](adr/0002-proxy-first-integration.md)). One port, **127.0.0.1:11435**, mirrors Ollama's own layout on 11434:

| Path | Speaks | Used by |
|---|---|---|
| `/v1/*` | OpenAI API | Open WebUI (OpenAI connection), LibreChat, OpenAI SDK scripts, most editors |
| `/api/*` | Ollama API | Open WebUI (Ollama connection), Enchanted, Page Assist, `ollama` CLI, apps that only speak Ollama |
| `/noskill/v1/*` | noskill's own API | Feedback, runs, approvals, event stream (tray, status page) |
| `GET /`, `HEAD /` | Liveness probe | Answers `200` "noskill is running" itself. The `ollama` CLI starts with `HEAD /` and aborts on a 404 (S2) |
| `/ui` | Web status page | Tray "Open status page" (FR-I6) |

## Endpoints

### OpenAI-compatible
| Endpoint | Behaviour |
|---|---|
| `POST /v1/chat/completions` | Full agent loop for `noskill/*` models; passthrough for plain names. Streaming and non-streaming |
| `GET /v1/models` | `noskill/auto`, exposed skills, and the backend's models |
| `POST /v1/embeddings` | Passthrough to the embed backend (no skills) |
| `POST /v1/systemone` | A JEV model's request goes unchanged to the Ollama backend that has it; one naming a chat model is emulated with `decide` (FR-B6a) and answered in the same form with `"emulated": true` and no `confidence`; no model (or `noskill/auto`) uses the configured decision model, falling back to emulation. Unknown model → 404 |
| `POST /v1/completions` | Passthrough only (legacy) |

### Ollama-compatible
| Endpoint | Behaviour |
|---|---|
| `POST /api/chat` | Same as `/v1/chat/completions`, Ollama format (NDJSON streaming) |
| `POST /api/generate` | `prompt` is treated as one user message (`system` goes first) for `noskill/*` models, answered in the generate shape (`response`, top-level `thinking`). `ollama run <model> "prompt"` uses this, not `/api/chat`. `raw: true` and plain names pass through |
| `GET /api/tags` | Same list as `/v1/models`. Synthetic `noskill/*` entries carry a 64-character hex `digest`: `ollama list` crashes on a digest shorter than 12 characters (S2) |
| `POST /api/show` | Real model info for backend models; a synthetic entry for `noskill/*` names, so `ollama run` and model pickers work |
| `GET /api/ps`, `GET /api/version` | Passthrough (`/api/ps` also shows what the orchestrator holds in each slot) |
| `POST /api/embed`, `/api/embeddings` | Passthrough |
| `POST /api/pull`, `/api/delete`, `/api/create`, `/api/copy`, `/api/push` | Passthrough if `server.allow_model_management = true` (default); the orchestrator re-reads model profiles afterwards |

### noskill's own (`/noskill/v1`)
| Endpoint | Purpose |
|---|---|
| `GET /health` | Daemon, backends, sandbox status |
| `POST /feedback` | `{run_id \| messages, rating: "up"\|"down", comment?}`: outcome signal for learning |
| `GET /runs/{id}` | Run summary: skills, models, steps, sources, file effects |
| `GET /approvals`, `POST /approvals/{id}` | List; `{action: "approve"\|"skip", fields?: {name: value}, note?}` |
| `GET /events` | Server-sent events: approval created/resolved, run finished. For the tray and status page |
| `GET /status` | **Admin.** One snapshot: loaded model, backends, models with speed and quality scores, jobs, approvals, recent runs, skills, learning queue, eval runs |
| `POST /jobs/{id}/{pause\|resume\|run}`, `POST /jobs/all/{pause\|resume}` | **Admin.** Job control for the tray and the status page. `all` pauses every enabled job and remembers which, so resuming doesn't wake the ones you had paused |
| `GET /ui` (outside `/noskill/v1`) | The status page: static HTML, no token needed to load it; its calls carry the token ([ADR-0011](adr/0011-tray-and-status-page-over-http.md)) |

**Admin** means a `[[server.tokens]]` entry with `admin = true`, or the `local` client when no tokens are set. Admins also see every approval and may approve ones that need `exec`.

The CLI doesn't use this API: it reads and writes the database directly ([ADR-0009](adr/0009-scheduler-through-the-database.md)).

## Model names

| Name | Effect |
|---|---|
| `noskill/auto` | The orchestrator picks the model; skills are selected automatically |
| `noskill/fast`, `noskill/best` | Same as `noskill/auto`, routed under the `fast` or `best` preset. With a rule limited to a preset (`when = { needs = ["tools"], preset = "best" }`, [CONFIG.md](CONFIG.md)) one model is the fast default and another the quality choice |
| `noskill/<skill>` (e.g. `noskill/research`) | Forces that skill; model still routed. Only skills listed in `server.expose_skills` appear in model lists |
| `noskill/<model>` (e.g. `noskill/qwen3:8b`) | Pins the model; skills on. A name containing `:` is always a model, because skill names can't contain `:` |
| `qwen3:8b` (plain) | Straight to the backend, no skills, no routing (`server.passthrough_plain_names = true`) |

Resolution for `noskill/<x>` without `:`: exposed skill → `fast` or `best` → installed model (`:latest`) → 404 with a list of valid names.

## What happens to a request

1. **Checks:** Host header, Origin, token ([below](#security)). Token scopes limit which skills that client may use.
2. **Conversation fingerprint:** a hash of the system prompt and the messages. Chat apps resend the whole history every turn, so the fingerprint links turns to earlier runs, pending approvals and cached state, without the client knowing about noskill.
3. **Classify and route** ([ORCHESTRATOR.md](ORCHESTRATOR.md)). Images in the request (`image_url` parts or Ollama `images`) are a hard `needs_vision`; a pinned model without vision gets a 400 that says which model would work.
4. **Fit the context:** each model runs at a fixed `num_ctx`. If history doesn't fit, older turns are summarised (cached by fingerprint) and the system prompt and last turns are kept verbatim. The response header `X-Noskill-Trimmed: true` says when this happened.
5. **Run the agent loop** with the selected skills. The client's system prompt is kept; skill instructions go after it.
6. **Stream** the progress and the answer (below).
7. **Record** the run: trace, outcome signals, `X-Noskill-Run-Id` header.

Client sampling options (`temperature`, `top_p`, `max_tokens`, `stop`, `seed`) apply to the final answer; internal steps use noskill's settings. `response_format` / Ollama `format` constrains the final answer.

## Streaming

| Part | OpenAI (`/v1`) | Ollama (`/api`) |
|---|---|---|
| Progress ("Searching: …", "Reading 3 pages…", "Switching to qwen2.5-coder:7b…") | `delta.reasoning_content` | `message.thinking` |
| Answer | `delta.content` | `message.content` |
| Keepalive every 10 s during long tool steps or queueing | SSE comment line `: keepalive` | Empty-content chunk with `done: false` |
| End | `finish_reason` + `data: [DONE]` | `done: true` with timing fields |

`server.progress`:
- `thinking` (default): as above. Most clients show it collapsed.
- `think_tags`: progress wrapped in `<think>…</think>` at the start of `content`, for clients that only parse tags.
- `content`: progress as plain text lines before the answer.
- `off`: nothing until the answer starts (keepalives still sent).

**Thinking fields.** Ollama's own `/v1` stream names thinking `reasoning`; other servers use `reasoning_content`. The proxy reads both from the backend and always sends `reasoning_content` (OpenAI) or `message.thinking` (Ollama). Open WebUI shows it as a collapsed "Thought" block. Internal steps (tool steps, background tasks) send `reasoning_effort: "none"` to the backend unless the client asked for thinking, because a thinking model can spend minutes on a one-line tool question (S2: `qwen3.5:4b`). Client `reasoning_effort`, `max_tokens`, `temperature`, `top_p`, `seed`, `stop`, Ollama `think: false` and `options.num_predict` are passed on to the final answer.

**Citations:** skills that use sources (research, web-search, local-docs) end the answer with a `Sources:` list of numbered links or file paths, matching the `[1]` markers in the text.

**Cancel:** when the client disconnects or presses stop, the run's context is cancelled, the backend request is closed (Ollama stops generating), and running tools are stopped. File effects already applied stay in the undo log.

> **Built so far (block 1b) and where it differs from this page**
> - Client-side tools: `merge` is not implemented. Skills still run inside noskill; the client's own tool definitions are dropped for the request and the response says so in `X-Noskill-Tools-Dropped`. `client_only` behaves the same.
> - `response_format` / `format` is parsed but not applied to agent runs (plain-name passthrough forwards it untouched).
> - Approvals in chat read only the user's latest message with a strict yes/no rule (short replies starting with yes or no; anything longer or mixed is "other"), not the `user_approved` decision question. A parked run continues from its stored continuation: the approved call runs first, then the loop carries on with the same model and skills ([ADR-0009](adr/0009-scheduler-through-the-database.md)).
> - `/noskill/v1/events` reads the database every 2 seconds, so it also sees changes made by the CLI.
> - Concurrency: a job waits for queued interactive requests before it starts a run, but doesn't yield between steps yet. Once started, it holds its slot until the run ends or waits on an approval.

## Client-side tools
Some clients (editor agents, scripts) send their own `tools`. `server.client_tools`:

| Mode | Behaviour |
|---|---|
| `merge` (default) | The model sees skill tools **and** client tools. Skill tool calls run inside noskill. A client tool call ends the response with `finish_reason: "tool_calls"` like a normal model would |
| `client_only` | Skills are off for requests that carry `tools`; noskill only routes |
| `server_only` | Client tools are dropped (logged) |

When a client tool call returns, the client sends back the history it knows, which doesn't include noskill's internal steps. noskill puts the run ID in the tool call ID it hands out (`call_nsk_<run>_<n>`), restores the internal state from a cache (1 hour), and continues the same run. If the cache has expired, it continues from the visible history alone.

### Measured with Open WebUI (S2)
- Open WebUI attaches its own built-in tools (`get_current_timestamp`, `calculate_timestamp`, …) to **every** chat request, on both connections. A one-word message produced a 45 KB request, roughly 11,000 tokens, which overflows an 8k context. So `merge` has a budget: `server.client_tools_budget_tokens` (default 1,500). When the client's tool definitions exceed it, the proxy keeps the ones most relevant to the request (embedding rank) up to the budget, and says so in the run trace and the `X-Noskill-Tools-Trimmed` header (`7 of 9`). The rest are dropped for that request, so the model can't call them. A budget of 0 forwards everything.
- Open WebUI also sends background requests to the same model name: a title, follow-up suggestions and tags (3 requests after the first message, then follow-ups after every turn). Each starts with `### Task:`. The proxy recognises these and runs them straight on the cheapest installed model, with no skills, tools, routing or learning, and doesn't count them as runs. Otherwise each would run the full agent loop and queue behind the chat.

<a id="approvals-in-chat"></a>
## Approvals in chat
Actions marked `confirm` (moving files, adding a qBittorrent rule, pulling a model) need a yes from you, even when asked from a chat app.

Example in Open WebUI:
> **You:** tidy up the PDFs in my Downloads
>
> **noskill/auto:** I'd move 14 PDFs from Downloads to Documents/PDFs (3 renamed to avoid clashes): *list…* Reply **yes** to go ahead or **no** to cancel. *(approval a7f3, also in the tray)*
>
> **You:** yes go ahead
>
> **noskill/auto:** Moved 14 files. Undo with `noskill undo 01JB…`.

How it works:
1. The run pauses, the approval is stored with the conversation fingerprint and a resume token, and it also appears in the tray, OS notification and `noskill approvals`.
2. On the next request in that conversation, noskill asks the decision model `user_approved` (choice: yes / no / unclear) about **your latest message only**. Tool output, web pages and the model's own text are never judged, so injected content can't approve anything.
3. Yes → the run resumes and streams its result. No → skipped. Unclear → asks once more, then leaves it to the tray/CLI. Something unrelated → the approval stays pending and your new message is handled normally.
4. Editable fields: if the approval has a `folder` field (e.g. anime-tracker's "Save to"), an absolute path in your latest message sets it (parsed from your text, not guessed by a model): "yes, save it to /Volumes/RED/ANIME2026". The reply repeats the folder used. A folder that doesn't exist yet gets one more question before it's created.
5. Approved in the tray meanwhile → the next reply starts with what was done.
6. An approval can only be answered from the same client (token) that triggered it, or from the tray/CLI.

Per request, the header `X-Noskill-Approval` overrides the mode: `chat` (default), `wait` (hold the stream open up to `server.approval_wait_seconds`, 120 s, with keepalives, waiting for the tray/CLI), or `deny` (skip all confirm actions; useful for scripts). Permissions with `exec` and skill installs are never approvable from chat; they need the tray or CLI.

## Feedback
Learning needs outcomes ([LEARNING.md](LEARNING.md)). From chat apps they come from:

- **Implicit:** the next message is scored with `decide("user_satisfied")` ("thanks, that worked" vs "no, that's wrong"); a regenerate (same fingerprint, same last message, sent again) is a weak negative for the earlier run.
- **Explicit:** `POST /noskill/v1/feedback`. Open WebUI keeps its thumbs up/down to itself, so noskill ships an Open WebUI **Action function** that adds 👍/👎 buttons and posts the conversation (noskill finds the run by fingerprint). Sketch, to be checked against the current Open WebUI version in S2:

```python
# Open WebUI → Admin → Functions → New (type: Action)
import requests
from pydantic import BaseModel

class Action:
    class Valves(BaseModel):
        noskill_url: str = "http://host.docker.internal:11435"
        token: str = ""

    def __init__(self):
        self.valves = self.Valves()

    async def action(self, body: dict, __user__=None, __event_emitter__=None, __event_call__=None):
        rating = "up"  # one function per button, or ask with __event_call__
        requests.post(f"{self.valves.noskill_url}/noskill/v1/feedback",
                      headers={"Authorization": f"Bearer {self.valves.token}"},
                      json={"messages": body.get("messages", []), "rating": rating}, timeout=5)
```

## Concurrency
- On machines with ≤ 16 GB, **one generation at a time** (`OLLAMA_NUM_PARALLEL=1` is what `doctor` recommends, since each parallel slot multiplies KV-cache memory).
- Interactive requests go before background jobs. A job yields between steps; a model call already running isn't interrupted.
- Queued streaming requests get keepalives and a progress line ("Waiting for the anime-tracker job to finish its step…"). Non-streaming requests wait up to 60 s, then get 503 with `Retry-After`.

## Errors
OpenAI-format errors on `/v1` (`{"error": {"message", "type", "code"}}`), Ollama-format on `/api` (`{"error": "..."}`).

| Status | When |
|---|---|
| 400 | Bad request, or a pinned model lacks a needed capability |
| 401 / 403 | Missing or wrong token / rejected Host or Origin |
| 404 | Unknown model name (message lists valid ones) |
| 502 | Backend returned an error |
| 503 | Backend down, or busy past the queue timeout (`Retry-After`) |
| 504 | Run exceeded its time budget (partial answer + run ID in the message) |

Every response carries `X-Noskill-Run-Id`, `X-Noskill-Model` (the model that answered) and `X-Noskill-Skills`. `noskill traces show <run-id>` explains any answer.

## Security
Details in [SECURITY.md](SECURITY.md#local-api-proxy-protection).
- Bound to `127.0.0.1`. Binding anywhere else requires at least one token.
- Host header must be `localhost`, `127.0.0.1`, `[::1]`, or listed in `server.allowed_hosts` (e.g. `host.docker.internal`).
- Requests with an `Origin` header must match `server.allowed_origins` (e.g. `chrome-extension://<id>` for Page Assist). Server-side clients like Open WebUI's backend send no Origin. The `/ui` page uses its own origin plus a CSRF token.
- Tokens: `Authorization: Bearer <token>`, each with a name (shown in traces) and an allowed skill list. Tokens must be at least 16 characters long and are compared in constant time. When no tokens are configured, `Authorization` is ignored and the client is `local` with all skills.
- Browser requests with no `Origin` header but `Sec-Fetch-Site: cross-site` or `same-site` are rejected. This covers no-cors GETs from `<img>` or `<script>` tags on other sites. The `Origin` value `null` (sandboxed frames, `file://`) is rejected unless it's listed.
- An allowed Origin gets CORS headers, and `OPTIONS` preflights are answered directly.
- Checks run in this order: Host (403), Origin (403), token (401 with `WWW-Authenticate: Bearer`).

## Setup

### Open WebUI
| Open WebUI runs… | Connection URL | noskill config |
|---|---|---|
| Natively (pip / uv) | Ollama: `http://127.0.0.1:11435`, or OpenAI: `http://127.0.0.1:11435/v1` | none |
| Docker Desktop (macOS, Windows) | `http://host.docker.internal:11435` (`/v1` for OpenAI) | `allowed_hosts = ["host.docker.internal"]` |
| Docker on Linux, `--network=host` (simplest) | `http://127.0.0.1:11435` | none |
| Docker on Linux, bridge network | `--add-host=host.docker.internal:host-gateway`, URL as above | `bind = "172.17.0.1"` (docker0), a token, `allowed_hosts = ["host.docker.internal"]` |

`noskill doctor --client openwebui` checks the setup from both sides and prints the exact values.

### Others
| Client | How |
|---|---|
| `ollama` CLI | `OLLAMA_HOST=127.0.0.1:11435 ollama run noskill/auto` |
| OpenAI SDK | `base_url="http://127.0.0.1:11435/v1"`, `api_key` = token or any string |
| LibreChat | Custom endpoint in `librechat.yaml` with `baseURL: http://host.docker.internal:11435/v1` |
| Continue / editors | Provider `ollama` with `apiBase: http://127.0.0.1:11435`, or OpenAI-compatible; `client_tools = "merge"` keeps agent mode working |
| Page Assist (browser extension) | Ollama URL `http://127.0.0.1:11435`; add the extension's origin to `allowed_origins` |
| Enchanted, Msty | Ollama server URL → port 11435 |

## Compatibility matrix (S2 results, then CI)
"Tested" = scripted or by hand in [S2](spikes/S2-results.md). Approvals in chat and client-tool round trips were not tested in S2.

| Client | Connection | Models list | Streaming | Progress shown | Cancel | Approvals in chat | Client tools |
|---|---|---|---|---|---|---|---|
| Open WebUI 0.12.2 | Ollama | ✓ (`/api/tags`, `/api/ps`) | ✓ | ✓ "Thought" block | ✓ Stop reaches the backend at once | not tested | sends ~11k tokens of its own tools: needs the budget |
| Open WebUI 0.12.2 | OpenAI | ✓ (`/v1/models`) | ✓ | ✓ "Thought" block | ✓ | not tested | as above |
| `ollama` CLI 0.40.3 | Ollama | ✓ (`list`, `show`) | ✓ (`run` uses `/api/generate`) | not checked | not tested | not tested | n/a |
| OpenAI Python SDK | OpenAI | ✓ | ✓ | `reasoning_content` available | ✓ (`stream.close()`) | n/a | not tested |
| Continue | Ollama | P1 | | | | | |
| LibreChat, Msty, Enchanted, Page Assist | | P1 | | | | | |

The release pipeline replays recorded sessions from each verified client against the proxy, so a protocol regression fails CI ([PACKAGING.md](PACKAGING.md#ci-pipelines-github-actions)).

## Using noskill from opencode and other tools

noskill is an OpenAI-compatible provider, so any tool that takes a custom OpenAI endpoint can use it: opencode, Continue, Open WebUI, the OpenAI SDKs. Use `http://127.0.0.1:11435/v1` as the base URL (the `ollama` CLI and Ollama-only tools use `http://127.0.0.1:11435`). If you set `[[server.tokens]]`, pass a token as the API key.

**opencode** (`opencode.json`, from opencode's custom-provider format):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "noskill": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "noskill (local)",
      "options": { "baseURL": "http://127.0.0.1:11435/v1", "apiKey": "{env:NOSKILL_TOKEN}" },
      "models": {
        "noskill/fast": { "name": "noskill fast (granite4:3b)", "limit": { "context": 8192, "output": 4096 } },
        "noskill/best": { "name": "noskill best (qwen3.5:4b)", "limit": { "context": 8192, "output": 4096 } },
        "noskill/auto": { "name": "noskill auto", "limit": { "context": 8192, "output": 4096 } }
      }
    }
  }
}
```

- **Which model answers:** `noskill/fast` and `noskill/best` use the models your preset rules name. The labels above are the measured choice on the project's machine (2026-10-18): `granite4:3b` for speed (9 of 10 checked `files` tasks at 9 s, 2.9 GB), `qwen3.5:4b` for performance (10 of 10 at 14 s, 4.2 GB), `gemma3:1b` as the low-memory backup for summarise and extract (10 of 11, 957 MB, no tools). Change the rules, not the client, to swap one.
- **Context:** noskill sends 8k to local models by default (`orchestrator.default_ctx`), so the limits above are 8192 and not what a hosted model offers. A coding agent that needs more should raise `default_ctx` and the model's limit together, and check the memory budget first.
- **Tools:** the client's own tool definitions are dropped inside noskill for now (the `X-Noskill-Tools-Dropped` header says so); noskill runs its skills' tools itself.
- **Other providers:** noskill talks to Ollama, LM Studio and other OpenAI-compatible servers as backends (`[[backends]]` in [CONFIG.md](CONFIG.md)); it isn't a way to reach a hosted provider, and no cloud fallback is built.
