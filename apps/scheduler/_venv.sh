# Sourced by start.sh and web.sh from the project root -- not meant to be run directly.
# POSIX sh, so it also works when a launcher is run as `sh start.sh` or `zsh start.sh`.
#
# Makes sure .venv exists, actually runs, and has the current pyproject.toml
# installed, creating or repairing it as needed. This covers a fresh clone, a
# venv whose base Python was upgraded/uninstalled (the "bad interpreter" case),
# a project folder that was moved, and dependencies added to pyproject.toml.

# launchd/cron start us with PATH=/usr/bin:/bin, which has neither a new enough
# python3 nor ollama (the daemon spawns `ollama serve` itself). Append the usual
# install locations; appending keeps whatever the caller's PATH prefers.
PATH="$PATH:${HOME:-}/.pyenv/shims:${HOME:-}/.local/bin:/opt/homebrew/bin:/usr/local/bin"
export PATH

VENV_PY=.venv/bin/python
STAMP=.venv/.sched-pyproject

# Prints the first interpreter that is >= 3.11 and can create a venv.
# Set SCHED_PYTHON to force a specific one.
find_python() {
    for cand in ${SCHED_PYTHON:-} python3.13 python3.12 python3.14 python3.11 python3 python; do
        if command -v "$cand" >/dev/null 2>&1 &&
            "$cand" -c 'import sys, venv, ensurepip; sys.exit(sys.version_info < (3, 11))' >/dev/null 2>&1; then
            command -v "$cand"
            return 0
        fi
    done
    if command -v uv >/dev/null 2>&1; then
        uv python find '>=3.11' 2>/dev/null && return 0
    fi
    return 1
}

# True if the venv's interpreter starts at all and is new enough.
venv_runs() {
    [ -x "$VENV_PY" ] &&
        "$VENV_PY" -c 'import sys; sys.exit(sys.version_info < (3, 11))' >/dev/null 2>&1
}

# True if schedlm and its dependencies import, from *this* checkout's src/
# (an editable install made before the folder was moved points elsewhere).
venv_imports() {
    "$VENV_PY" -c '
import os, sys
import schedlm.cli, schedlm.webapi
src = os.path.realpath("src") + os.sep
sys.exit(not os.path.realpath(schedlm.__file__).startswith(src))
' >/dev/null 2>&1
}

# (Re)creates .venv from scratch; $1 says why, for the user.
make_venv() {
    if ! py=$(find_python); then
        echo "Error: no Python >= 3.11 found. Install one (e.g. 'brew install python@3.12')," >&2
        echo "or point SCHED_PYTHON at an interpreter: SCHED_PYTHON=/path/to/python3 $0" >&2
        exit 1
    fi
    echo "$1 with $py ..."
    "$py" -m venv --clear .venv
}

install_deps() {
    echo "Installing sched and its dependencies into .venv ..."
    "$VENV_PY" -m pip install --quiet --disable-pip-version-check -e '.[dev]'
}

if ! venv_runs; then
    if [ -e .venv ]; then
        make_venv "Existing .venv is broken (its Python is missing or too old); rebuilding it"
    else
        make_venv "Creating .venv"
    fi
fi

want=$(cksum <pyproject.toml)
have=$(cat "$STAMP" 2>/dev/null || true)
if [ "$want" != "$have" ] || ! venv_imports; then
    ok=false
    if install_deps; then
        if venv_imports; then
            ok=true
        else
            # pip thinks everything is installed but imports still fail, i.e.
            # files were deleted out from under it. Only a clean venv fixes that.
            make_venv "Packages in .venv are damaged; rebuilding it"
            install_deps && venv_imports && ok=true
        fi
    fi
    if $ok; then
        echo "$want" >"$STAMP"
    elif venv_imports; then
        echo "Warning: could not update dependencies (offline?); continuing with the installed ones." >&2
    else
        echo "Error: could not set up .venv (see output above). Check your network and run this again." >&2
        exit 1
    fi
fi

if ! command -v ollama >/dev/null 2>&1; then
    echo "Warning: 'ollama' not found -- install it from https://ollama.com/download" >&2
elif ! ollama list >/dev/null 2>&1; then
    echo "Note: Ollama isn't running yet; sched will start it ('ollama serve') when it needs it." >&2
fi
