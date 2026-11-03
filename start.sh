#!/usr/bin/env bash
# One-command launcher: starts Ollama (if needed), installs deps (if needed),
# runs the Orcastrator web UI, and opens it in your browser.
#
#   ./start.sh                 # serve on http://127.0.0.1:8400
#   ./start.sh --port 9000     # any orcastrator-web flag is passed through
#   ./start.sh --setup         # only make or repair .venv, then exit (the noskill app uses this)
#
# Ctrl-C stops the web server, and also Ollama if this script started it.
set -euo pipefail
cd "$(dirname "$0")"

OLLAMA_URL="${OLLAMA_HOST:-http://localhost:11434}"

# --- Python environment ------------------------------------------------------
# Homebrew is deliberately never used: its Pythons live under a versioned
# Cellar path that `brew upgrade` deletes (orphaning any venv built on them),
# and a Homebrew prefix owned by another macOS user may not run for this one.
is_homebrew() {
    case "$1|$(readlink "$1" 2>/dev/null || true)" in
        */homebrew/* | */Homebrew/* | */Cellar/* | */linuxbrew/*) return 0 ;;
    esac
    return 1
}

# Run "$@" but kill it after $1 seconds (macOS ships no `timeout`).
with_timeout() {
    local secs=$1 pid watcher rc
    shift
    "$@" </dev/null &
    pid=$!
    (sleep "$secs" && kill "$pid" 2>/dev/null) &
    watcher=$!
    disown "$watcher" # no "Terminated" job notice when we kill it below
    wait "$pid" && rc=0 || rc=$?
    kill "$watcher" 2>/dev/null || true
    return "$rc"
}

# Meets pyproject's requires-python (>=3.10) and actually starts.
python_ok() {
    [ -x "$1" ] && ! is_homebrew "$1" &&
        with_timeout 10 "$1" -c 'import sys; sys.exit(sys.version_info < (3, 10))' 2>/dev/null
}

# First usable non-Homebrew python: PATH, python.org installers, pyenv.
find_python() {
    local cand
    for cand in python3 python3.15 python3.14 python3.13 python3.12 python3.11 python3.10; do
        cand="$(command -v "$cand" 2>/dev/null)" || continue
        if python_ok "$cand"; then echo "$cand"; return 0; fi
    done
    for cand in /Library/Frameworks/Python.framework/Versions/3.*/bin/python3 \
        "$HOME"/.pyenv/versions/3.*/bin/python3; do
        if python_ok "$cand"; then echo "$cand"; return 0; fi
    done
    return 1
}

find_uv() {
    local cand
    for cand in "$HOME/.local/bin/uv" "$HOME/.cargo/bin/uv" "$(command -v uv 2>/dev/null || true)"; do
        if [ -n "$cand" ] && [ -x "$cand" ] && ! is_homebrew "$cand"; then
            echo "$cand"
            return 0
        fi
    done
    return 1
}

# No suitable Python on this machine: fetch a standalone one with uv, which
# installs into ~/.local without Homebrew or sudo.
uv_python() {
    local uv
    if ! uv="$(find_uv)"; then
        echo "==> No Python 3.10+ found; installing uv into ~/.local/bin to fetch one…" >&2
        curl -LsSf https://astral.sh/uv/install.sh | env UV_NO_MODIFY_PATH=1 sh >&2
        uv="$(find_uv)" || return 1
    fi
    echo "==> Fetching a standalone Python 3.12 with $uv…" >&2
    UV_PYTHON_PREFERENCE=only-managed "$uv" python install 3.12 >&2
    UV_PYTHON_PREFERENCE=only-managed "$uv" python find 3.12
}

# A venv copied from another user/machine or left behind by a removed Python
# has interpreter symlinks or script shebangs that point nowhere — rebuild it.
venv_ok() {
    ! grep -qE '^home = .*([Hh]omebrew|Cellar|linuxbrew)' .venv/pyvenv.cfg 2>/dev/null &&
        with_timeout 10 .venv/bin/python -c 'import sys; sys.exit(sys.version_info < (3, 10))' >/dev/null 2>&1 &&
        head -1 .venv/bin/orcastrator-web 2>/dev/null | grep -qF "$(cd .venv && pwd -P)/bin/python"
}

if [ -e .venv ] && ! venv_ok; then
    stale=".venv.stale-$(date +%Y%m%d%H%M%S)"
    echo "==> Existing .venv is unusable here; moving it to $stale and rebuilding…"
    mv .venv "$stale"
fi
if [ ! -e .venv ]; then
    if ! PYTHON="$(find_python)" && ! PYTHON="$(uv_python)"; then
        echo "error: Python 3.10+ is required; install one from https://www.python.org/downloads/" >&2
        exit 1
    fi
    echo "==> Creating virtualenv with $PYTHON and installing orcastrator…"
    "$PYTHON" -m venv .venv
    .venv/bin/pip install --quiet --upgrade pip
    .venv/bin/pip install --quiet -e .
fi
# Catch a stale venv that predates the web dependencies.
if ! .venv/bin/python -c "import orcastrator, fastapi, uvicorn, ollama" >/dev/null 2>&1; then
    echo "==> Installing missing dependencies…"
    .venv/bin/pip install --quiet -e .
fi

if [ "${1:-}" = "--setup" ]; then
    echo "==> .venv is ready"
    exit 0
fi

# --- Ollama daemon -----------------------------------------------------------
ollama_up() { curl -fsS --max-time 2 "$OLLAMA_URL/api/version" >/dev/null 2>&1; }

STARTED_OLLAMA_PID=""
if ollama_up; then
    echo "==> Ollama already running at $OLLAMA_URL"
else
    # The Ollama.app download bundles its CLI even when it isn't linked onto PATH.
    OLLAMA_BIN="$(command -v ollama 2>/dev/null || true)"
    if [ -z "$OLLAMA_BIN" ] && [ -x /Applications/Ollama.app/Contents/Resources/ollama ]; then
        OLLAMA_BIN=/Applications/Ollama.app/Contents/Resources/ollama
    fi
    if [ -z "$OLLAMA_BIN" ]; then
        echo "error: Ollama is not installed. Get it from https://ollama.com/download" >&2
        exit 1
    fi
    OLLAMA_LOG="${TMPDIR:-/tmp}/ollama-serve.log"
    echo "==> Starting Ollama (log: $OLLAMA_LOG)…"
    "$OLLAMA_BIN" serve >"$OLLAMA_LOG" 2>&1 &
    STARTED_OLLAMA_PID=$!
    for i in $(seq 1 30); do
        ollama_up && break
        if [ "$i" -eq 30 ]; then
            echo "error: Ollama did not become ready after 30s — see $OLLAMA_LOG" >&2
            exit 1
        fi
        sleep 1
    done
    echo "==> Ollama ready at $OLLAMA_URL"
fi

if ! curl -fsS --max-time 5 "$OLLAMA_URL/api/tags" 2>/dev/null | grep -q '"name"'; then
    echo "warning: no models installed — the UI will not work until you run: ollama pull llama3.2" >&2
fi

# Stop Ollama on exit only if this script was the one that started it.
cleanup() {
    if [ -n "$STARTED_OLLAMA_PID" ]; then
        echo "==> Stopping Ollama (pid $STARTED_OLLAMA_PID)…"
        kill "$STARTED_OLLAMA_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# --- Web UI ------------------------------------------------------------------
# Honor a --port flag when deciding which URL to open in the browser.
PORT=8400
args=("$@")
for i in "${!args[@]}"; do
    if [ "${args[$i]}" = "--port" ] && [ $((i + 1)) -lt ${#args[@]} ]; then
        PORT="${args[$((i + 1))]}"
    fi
done

URL="http://127.0.0.1:${PORT}"
# Give uvicorn a moment to bind, then open the page (macOS `open` / Linux `xdg-open`).
(
    sleep 1.5
    if command -v open >/dev/null 2>&1; then open "$URL"
    elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$URL"
    fi
) &

echo "==> Starting Orcastrator web UI at $URL (Ctrl-C to stop)"
.venv/bin/orcastrator-web "$@"
