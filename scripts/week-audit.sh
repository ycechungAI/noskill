#!/bin/sh
# The week run's morning check (docs/WEEK_RUN.md), for an installed noskill that doesn't have
# `noskill anime audit` yet. It copies state.db and config.toml to a scratch folder, runs the audit
# from this source tree against the copy, and appends the result to the real logs/week-audit.jsonl.
# It changes nothing in ~/.noskill except that log, and it doesn't touch the installed binary.
#
#   scripts/week-audit.sh [--since 24h] [--job anime-nightly]
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
home=${NOSKILL_HOME:-$HOME/.noskill}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/noskill-audit.XXXXXX")
trap 'rm -rf "$tmp"' EXIT   # our own scratch copy
sqlite3 "$home/state.db" ".backup $tmp/state.db"
cp "$home/config.toml" "$tmp/config.toml"
status=0
(cd "$here" && go run ./cmd/noskill --home "$tmp" anime audit "$@") || status=$?
if [ -f "$tmp/logs/week-audit.jsonl" ]; then
  mkdir -p "$home/logs"
  cat "$tmp/logs/week-audit.jsonl" >> "$home/logs/week-audit.jsonl"
fi
exit $status
