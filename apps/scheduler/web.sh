#!/bin/sh
# Launches the sched web UI in the foreground. Run from a terminal; Ctrl-C to stop.
# Any arguments are passed through to `sched serve`, e.g. `./web.sh --port 9000`.
# Open http://127.0.0.1:8787 in a browser (or whatever --host/--port you passed;
# the exact URL is printed on startup). Tasks only run on schedule while
# ./start.sh (the daemon) is also running.
# Creates or repairs .venv on the way if needed (see _venv.sh).
set -eu

# cd to the project root -- following symlinks, so this works from any cwd,
# through a symlink (e.g. ~/bin/sched-web), and as `sh/bash/zsh web.sh`.
self=$0
while [ -L "$self" ]; do
    link=$(readlink "$self")
    case $link in
        /*) self=$link ;;
        *) self=$(dirname "$self")/$link ;;
    esac
done
cd "$(dirname "$self")"
. ./_venv.sh

echo "Starting sched web UI (Ctrl-C to stop)..."
# `python -m` rather than .venv/bin/sched, whose shebang hardcodes the venv's
# absolute path and breaks if the project folder is moved.
exec "$VENV_PY" -m schedlm.cli serve "$@"
