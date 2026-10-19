#!/bin/sh
# ask.sh: run the noskill status page (Overview, Approvals, Downloads, History) from this source tree
# and open it in your browser.
#
#   ./ask.sh [overview|approvals|downloads|history]     (default: approvals)
#
# Why a script: the installed noskill (and the daemon on :11435) predates the Downloads tab, and
# rebuilding it would lose the macOS Full Disk Access grant mid-week (docs/WEEK_RUN.md). This builds
# the current source into a scratch folder and serves just the page and API on another port with
# `serve --no-jobs`: nothing in this process runs scheduled jobs, learning or approval sweeps, so
# it can sit next to the daemon. It reads and writes the same state as the daemon: an approval you
# answer here is picked up by the daemon, and "Add a download" / "Stop seeding and file now" act
# on your torrent app straight away.
#
# Running it again stops its earlier server and starts one from the current source.
#
# Settings (environment):
#   NOSKILL_HOME      the noskill folder to use          (default ~/.noskill)
#   NOSKILL_UI_PORT   the port for the page              (default 11436)
#   NOSKILL_NO_OPEN   set to anything: don't open a browser, just print the address
# If [[server.tokens]] is set in config.toml the page asks for an admin token. Stop with Ctrl-C.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
home=${NOSKILL_HOME:-$HOME/.noskill}
port=${NOSKILL_UI_PORT:-11436}
tab=${1:-approvals}
case "$tab" in overview|approvals|downloads|history) ;; *)
  echo "usage: ./ask.sh [overview|approvals|downloads|history]" >&2; exit 2 ;;
esac
url="http://127.0.0.1:$port/ui#$tab"

open_page() {
  [ -n "${NOSKILL_NO_OPEN:-}" ] && return 0
  if command -v open >/dev/null 2>&1; then open "$url"
  elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$url" >/dev/null 2>&1 || true
  elif command -v wslview >/dev/null 2>&1; then wslview "$url" || true
  fi
}

# An earlier ./ask.sh still serving on this port runs the code it was built from, so it would
# miss anything added since (a new skill, a new tab). Stop that one (only a server this script
# started, found by its recorded process ID) and start fresh. Anything else on the port is left alone.
pidfile="${TMPDIR:-/tmp}/noskill-ask-$port.pid"
if [ -f "$pidfile" ]; then
  old=$(cat "$pidfile" 2>/dev/null || true)
  if [ -n "$old" ] && ps -p "$old" -o command= 2>/dev/null | grep -q "noskill-ask\..*serve --no-jobs"; then
    echo "stopping the earlier ask.sh server ($old) so this one runs the current source"
    kill "$old" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$pidfile"
fi
if curl -sf -m 2 "http://127.0.0.1:$port/" >/dev/null 2>&1; then
  echo "port $port is used by something that isn't an ask.sh server; set NOSKILL_UI_PORT to another port" >&2
  exit 1
fi

command -v go >/dev/null 2>&1 || { echo "ask.sh builds from source and needs Go (https://go.dev/dl/)" >&2; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/noskill-ask.XXXXXX")
pid=""
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true   # only the server this script started
  rm -f "$pidfile"
  rm -rf "$tmp"                                       # our own scratch folder
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "building noskill from $here …"
(cd "$here" && go build -o "$tmp/noskill" ./cmd/noskill)

"$tmp/noskill" --home "$home" serve --no-jobs --port "$port" &
pid=$!
echo "$pid" > "$pidfile"

i=0
until curl -sf -m 2 "http://127.0.0.1:$port/" >/dev/null 2>&1; do
  i=$((i + 1))
  if ! kill -0 "$pid" 2>/dev/null; then echo "the server stopped; see the message above" >&2; pid=""; exit 1; fi
  if [ "$i" -gt 60 ]; then echo "the server didn't come up on port $port" >&2; exit 1; fi
  sleep 0.5
done
echo "status page: $url   (Ctrl-C stops it)"
open_page
wait "$pid" || true
pid=""
