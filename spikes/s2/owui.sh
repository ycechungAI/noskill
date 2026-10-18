#!/bin/sh
# Run Open WebUI (no sign-in, no telemetry, no model downloads) against the S2 proxy.
#   owui.sh openai   → only the OpenAI connection (http://127.0.0.1:11435/v1)
#   owui.sh ollama   → only the Ollama connection (http://127.0.0.1:11435)
cd "$(dirname "$0")" || exit 1
MODE=${1:-openai}
# one data folder for both modes: its secret key signs the browser cookie
export DATA_DIR="$PWD/out/owui-data" PORT=8091 WEBUI_AUTH=False
export ANONYMIZED_TELEMETRY=False SCARF_NO_ANALYTICS=true DO_NOT_TRACK=true HF_HUB_OFFLINE=1 OFFLINE_MODE=true
export OLLAMA_BASE_URL=http://127.0.0.1:11435 OPENAI_API_BASE_URL=http://127.0.0.1:11435/v1 OPENAI_API_KEY=spike-token
if [ "$MODE" = openai ]; then export ENABLE_OLLAMA_API=False ENABLE_OPENAI_API=True; else export ENABLE_OLLAMA_API=True ENABLE_OPENAI_API=False; fi
exec uvx --python 3.12 open-webui@latest serve --port 8091
