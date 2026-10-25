#!/bin/sh
# web.sh: the local web pages in one run.
#
#   ./web.sh                 build the docs site, then serve the docs site and the Phase 0 spike status
#   ./web.sh --no-build      serve the docs site already in site/ (skips `go run ./scripts/docsite`)
#   ./web.sh --open          also open the pages in your browser
#   ./web.sh -h | --help     this text
#
# Pages (127.0.0.1 only):
#   docs site            http://127.0.0.1:8767/         (set DOCS_PORT to change it)
#   Phase 0 spike status http://127.0.0.1:8765/         (set SPIKES_PORT to change it)
#   noskill status page  http://127.0.0.1:11435/ui#spikes  the Spikes tab shows both (the daemon serves it; ports 8765 and 8767 are
#                        what that tab expects, so keep the defaults for it)
#
# Ctrl-C stops the two servers it started, by process id, and nothing else.
# Needs python3; the docs site build needs Go (use --no-build without it). Windows: web.ps1.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
docs_port=${DOCS_PORT:-8767}
spikes_port=${SPIKES_PORT:-8765}
build=1
open_it=0
while [ $# -gt 0 ]; do
	case $1 in
	--no-build) build=0 ;;
	--open) open_it=1 ;;
	-h | --help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) echo "web.sh: unknown option $1 (try --help)" >&2; exit 2 ;;
	esac
	shift
done

command -v python3 >/dev/null 2>&1 || { echo "web.sh: python3 is needed to serve the pages" >&2; exit 1; }

busy() { python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)  # as the servers do: a port in TIME_WAIT isn't busy
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(0)
sys.exit(1)
PY
}
serving() { python3 - "$1" <<'PY'
import sys, urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:%s/" % sys.argv[1], timeout=2)
except Exception:
    sys.exit(1)
PY
}
start_docs=1
start_spikes=1
if busy "$docs_port"; then
	if serving "$docs_port"; then start_docs=0; echo "something already serves port $docs_port: using it for the docs site"; else echo "web.sh: port $docs_port is in use by something that isn't a web page (DOCS_PORT picks another port)" >&2; exit 1; fi
fi
if busy "$spikes_port"; then
	if serving "$spikes_port"; then start_spikes=0; echo "something already serves port $spikes_port: using it for the spike status"; else echo "web.sh: port $spikes_port is in use by something that isn't a web page (SPIKES_PORT picks another port)" >&2; exit 1; fi
fi

if [ "$build" -eq 1 ]; then
	if command -v go >/dev/null 2>&1; then
		echo "building the docs site…"
		(cd "$here" && go run ./scripts/docsite -out site) || { echo "web.sh: the docs site didn't build; serving what is in site/ if anything" >&2; }
	else
		echo "go isn't installed: serving the docs site as it is in site/ (run once with Go to rebuild it)" >&2
	fi
fi
[ -f "$here/site/index.html" ] || { echo "web.sh: there is no docs site in site/ yet; install Go and run ./web.sh again" >&2; exit 1; }

pids=""
stop() {
	trap - INT TERM EXIT
	for p in $pids; do kill "$p" 2>/dev/null || true; done
	exit 0
}
trap stop INT TERM EXIT

if [ "$start_docs" -eq 1 ]; then
	(cd "$here" && exec python3 -m http.server "$docs_port" --bind 127.0.0.1 --directory site >/dev/null 2>&1) &
	pids="$pids $!"
fi
if [ "$start_spikes" -eq 1 ]; then
	(cd "$here" && exec python3 spikes/status/server.py "$spikes_port" >/dev/null 2>&1) &
	pids="$pids $!"
fi
sleep 1
for p in $pids; do
	kill -0 "$p" 2>/dev/null || { echo "web.sh: a server stopped right after it started (port taken?)" >&2; exit 1; }
done

echo
echo "  docs site             http://127.0.0.1:$docs_port/"
echo "  Phase 0 spike status  http://127.0.0.1:$spikes_port/"
if curl -s -o /dev/null -m 2 http://127.0.0.1:11435/ui 2>/dev/null; then
	echo "  noskill status page   http://127.0.0.1:11435/ui#spikes   (the Spikes tab shows both)"
fi
echo
echo "Ctrl-C stops the servers this started."
if [ "$open_it" -eq 1 ]; then
	for u in "http://127.0.0.1:$docs_port/" "http://127.0.0.1:$spikes_port/"; do
		if command -v open >/dev/null 2>&1; then open "$u"; elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$u" >/dev/null 2>&1; fi
	done
fi
[ -n "$pids" ] && wait
