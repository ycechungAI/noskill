# Configuration reference

One human-editable file: `config.toml` in the noskill config folder (`~/.noskill/` on macOS, `%APPDATA%\noskill\` on Windows, `$XDG_CONFIG_HOME/noskill/` on Linux; `NOSKILL_HOME` overrides). `noskill init` writes it with only the values that differ from the defaults; everything below is the full set.

## Precedence (lowest → highest)
1. Built-in defaults
2. `config.toml`
3. Project file `./.noskill/config.toml`: **only** `[skills] paths` and per-skill settings; it can never widen permissions, change the server, or turn off the sandbox
4. Environment `NOSKILL_<SECTION>_<KEY>` (e.g. `NOSKILL_SERVER_PORT=11500`)
5. CLI flags

`noskill config check` validates types, unknown keys (typos), paths and secret references. The daemon reloads on file change; settings that need a restart say so.

## Secrets
Never written in this file. Reference them as `"secret:NAME"` and store with `noskill secrets set NAME` (OS keychain).

## Full reference (defaults shown)

```toml
[general]
home = "auto"                    # data dir; "auto" = per-OS default
log_level = "info"               # debug | info | warn | error
telemetry = false                # opt-in anonymous crash reports only

[server]                         # see PROXY.md
port = 11435
bind = "127.0.0.1"               # "0.0.0.0" or a bridge IP requires a token
allowed_hosts = []               # extra Host headers, e.g. ["host.docker.internal"]
allowed_origins = []             # browser-based clients, e.g. ["chrome-extension://<id>"]
tokens = []                      # [{ name = "openwebui", token = "secret:NOSKILL_TOKEN_OWUI", skills = ["*"] }]; token values ≥ 16 characters
                                 # the tray uses a token named "tray" with admin = true; admin tokens also see all approvals and /noskill/v1/status
expose_skills = ["research", "local-docs", "watcher"]   # listed as noskill/<skill> models
progress = "thinking"            # thinking | think_tags | content | off
client_tools = "merge"           # merge | client_only | server_only
client_tools_budget_tokens = 1500 # merge: keep only the most relevant client tools up to this size (Open WebUI sends ~11k tokens). 0 = no limit
passthrough_plain_names = true   # plain model names = no skills, straight to backend
allow_model_management = true    # pass /api/pull, /api/delete through
approvals_in_chat = true
approval_wait_seconds = 120

[[backends]]                     # init fills these in; order = preference
kind = "ollama"                  # ollama | openai | mlx | vllm
url = "http://127.0.0.1:11434"
# [[backends]]
# kind = "openai"
# url = "http://127.0.0.1:1234/v1"   # LM Studio
# api_key = "secret:LMSTUDIO_KEY"

[orchestrator]                   # see ORCHESTRATOR.md
preset = "balanced"              # fast | balanced | best
memory_budget_gb = "auto"       # auto = 6 on 16 GB Apple Silicon (ADR-0012), ⅔ of RAM on other Apple Silicon, VRAM − 0.5 on discrete GPUs, 50% CPU-only
memory_burst_gb = "auto"        # grow the budget up to this while memory is free and pressure is normal; auto = ⅔ of RAM on Apple Silicon, ¾ CPU-only, none on discrete GPUs; 0 = off
decision_model = "tev1:0.8b"     # "" = emulate with chat model; "tev1" recommended above 16 GB
decision_cache_gb = 1            # unload any of noskill's models whose prompt cache grows past this (ADR-0006, ADR-0012); 0 = never
embed_model = "nomic-embed-text"
min_swap_gain = 0.15
exclude = []                     # models the router never picks, even when installed and memory is free, e.g. ["qwen3.5:4b"]; a rule naming one is skipped. `run --model` can still name it (FR-O2)
hold_seconds = 120
explore = 0.05
default_ctx = 8192
auto_tune = false                # let calibration move decision thresholds from outcomes (FR-B6c); `noskill calibration` reports either way

[[orchestrator.rules]]
when = { task_type = "code" }
use = "qwen2.5-coder:7b"
# A rule's `when` takes task_type, needs and preset (fast | balanced | best), all of which must match; the first rule that matches and
# whose model is installed and fits wins. `preset` is the one the run uses (`noskill run --preset best`, or orchestrator.preset):
# [[orchestrator.rules]]
# when = { needs = ["tools"] }
# use = "granite4:3b"

[agent]
max_steps = 25
max_tokens = 60000
small_model_mode = "auto"        # auto | on | off
tool_strategy = "auto"           # auto | native | constrained | prompted

[learning]                       # see LEARNING.md
enabled = true
promote_mode = "ask"             # ask | shadow | auto (shadow behaves like ask for now)
promote_after = 3
min_tool_calls = 5
novelty_threshold = 0.75
idle_minutes = 10
require_ac = true
max_drafts_per_night = 3
max_refines_per_night = 3
author_model = "auto"
eval_schedule = "0 4 * * 0"      # weekly eval, run by the learning worker when idle after this time; the first one is a week after install
regression_success_drop = 5      # points overall (a type also regresses when it falls 25+ points or two of its tasks flip)
regression_steps_rise = 0.20

[learning.lora]                  # weight learning, `noskill train` (FR-L8, ADR-0015)
min_runs = 200                   # good runs a dataset needs before `train lora` runs
mlx_lm = "0.26.0"                # the mlx-lm version uv installs (not yet checked by a real run)
mlx_models = { "qwen3:4b" = "mlx-community/Qwen3-4B-4bit", "qwen3:8b" = "mlx-community/Qwen3-8B-4bit", "llama3.2:3b" = "mlx-community/Llama-3.2-3B-Instruct-4bit" }  # Ollama base → MLX repository; 4-bit, so QLoRA

[memory]
auto_extract = true              # learn preferences from conversations (shown in `noskill memory list`)
max_items = 2000

[traces]
retention_days = 90              # pinned, eval and skill-source traces are kept
redact = ["secrets", "emails"]   # + any custom regex: { regex = "..." }

[scheduler]
max_concurrent_jobs = 1          # 16 GB: one model-using job at a time
missed_policy = "once"           # skip | once
default_timeout = "30m"
retries = 2

[approvals]
default_timeout = "24h"
default_on_timeout = "skip"      # always skip; listed for clarity
channels = ["cli", "notification", "tray"]

[notify]
os = true
# ntfy = { url = "https://ntfy.sh/my-topic", token = "secret:NTFY_TOKEN" }
# telegram = { bot_token = "secret:TG_BOT", chat_id = "123" }
# discord = { webhook = "secret:DISCORD_WEBHOOK" }
# email = { smtp = "smtp.example.com:587", user = "me@example.com", password = "secret:SMTP_PASS", to = "me@example.com" }

[sandbox]                        # see SECURITY.md
mode = "enforce"                 # enforce | warn (warn = run unsandboxed with a warning; not recommended)
script_timeout = "60s"
script_memory_mb = 1024          # watchdog-enforced on macOS (no memory rlimit there); keep headroom (ADR-0008)
script_max_procs = 64            # RLIMIT_NPROC on top of your current process count; only matters with `subprocess: true`
script_max_file_mb = 256         # RLIMIT_FSIZE: largest single file a script may write

[skills]
paths = []                       # extra skill folders
auto_update = false              # installed community skills; updates always show a permission diff

[skills.anime-tracker]           # per-skill settings live under [skills.<name>]
# [[skills.anime-tracker.follow]]  # a show followed by name: the newest episode each week, smallest file (SKILL.md "Followed shows")
#   title = "Renegade Immortal"; match = ["renegade immortal", "xian ni"]; dest = "volume:RED/@DONG+"; from = 162
mode = "approve"                 # approve | notify-only (no automatic mode: every title needs your OK)
default_destination = "/Volumes/RED/ANIME2026"   # prefilled in each approval; you can change it per title
download_dir = "$DOWNLOADS"      # the torrent app downloads here first
client = "auto"                  # auto | transmission | qbittorrent. auto = Transmission when it answers, qBittorrent only as a backup when it doesn't (`noskill anime detect` shows what's found)
ask_before_download = "unsure"   # unsure (default) | true | false. unsure: a release whose name isn't exactly the show's waits for your yes; true: every episode of an approved show does; false: none does. The question is an approval (status page, tray, `noskill approvals`); a no is remembered. Followed shows are never asked about, and new shows always are
nyaa_url = "https://nyaa.si"     # English-translated anime; https only; a mirror (e.g. https://nyaa.media) where it's blocked
max_age = "2400h"                # only uploads this recent; only this season's new shows are suggested anyway
library_dirs = []                # other anime folders: read only, so shows and episodes you have are skipped
quality = { prefer = ["480p", "720p", "1080p"], fallback_after = "12h", avoid = ["HEVC 10bit"], prefer_groups = ["Erai-raws"], hd_max_size_mb = 500 }
                                 # prefer_groups: tried first at the same resolution; hd_max_size_mb: 720p+ only if the file is this small (0 = no limit)
naming = "simple"                # "<Title> S02E03 [480p].mkv"
episode_style = "sxxexx"         # sxxexx (S02E03) | ep (EP 3) | episode (Episode 3)
mode = "approve"                 # approve | notify-only (send the list of new shows; no approval cards)
replace_versions = true          # a fixed release (v2) of an episode noskill filed replaces it; the old file goes to the trash
after_filing = "trash"           # OS trash, only after the copy is verified
# ...see skills/anime-tracker/SKILL.md for all keys

[tray]
enabled = true

[update]                         # see PACKAGING.md
repo = ""                        # owner/name of the GitHub repository releases come from (empty until the project org is chosen)
check = true                     # weekly check, notify only
auto = false
channel = "stable"               # stable | beta
```

## Per-skill settings
Each skill documents its keys in its `SKILL.md` implementation spec. `noskill skills setup <name>` writes them interactively. Skill settings can reference other settings (`${config.download_dir}`) and secrets (`secret:NAME`).
