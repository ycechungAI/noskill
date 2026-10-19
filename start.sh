#!/bin/sh
# start.sh: one place to run noskill's everyday commands from a menu.
#
#   ./start.sh                  the menu (type a number or a name; q quits)
#   ./start.sh <name> [words]   run one item straight away, e.g.  ./start.sh research "heat pumps"
#   ./start.sh -l | --list      list every item, one per line
#   ./start.sh -h | --help      this text
#   ./start.sh readme           the guide (docs/START.md)
#
# Options (before the name):  --source  use a noskill built from this source tree, even if one is installed
#                             -n | --dry  print the command instead of running it
# Settings (environment):     NOSKILL_BIN  the noskill to use   NOSKILL_HOME  the noskill folder (default ~/.noskill)
#
# It uses the installed noskill. When that one doesn't know a command yet (it was installed before the
# command existed), it builds the current source into a scratch folder instead and uses that; it never
# replaces the installed one. Items starting with @ are for working on noskill itself and need Go.
# Windows: start.cmd (the same menu).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
from_source=0
dry=0

# key|group|title|what it runs ({} = the words you type; @ = a source-tree task)|what to ask for|default
items() {
  cat <<'TABLE'
ui|Use|Status page: approvals, downloads, history|ui||
approvals|Use|What is waiting for your yes|approvals||
run|Use|Run one task|run {}|What should noskill do?|
chat|Use|Chat in the terminal|chat||
research|Use|STORM research report on a topic|run --skill storm {}|Topic to research|
ask-docs|Use|Ask your own documents|run --skill local-docs {}|Your question|
docs-add|Use|Let noskill read a folder|docs add {}|Folder to add|
anime|Anime|Tracker status|anime status||
anime-dry|Anime|Try a run without downloading|anime run --dry-run||
anime-add|Anime|Add a download by link|anime add {}|Magnet, .torrent or Nyaa link|
anime-audit|Anime|Check the last 24 hours|anime audit --since 24h||
doctor|Check|Check the setup|doctor||
models|Check|Models and their speed|models||
jobs|Check|Running jobs and recent runs|jobs||
schedule|Check|Scheduled jobs|schedule list||
skills|Check|Installed skills|skills list||
bench|Test|Benchmark a model: speed, memory, quality|models bench {}|Model to benchmark|granite4:3b
bench-speed|Test|Benchmark speed and memory only|models bench {} --perf-only|Model to benchmark|granite4:3b
eval|Test|Run the checked tasks on a model|eval run --model {}|Model to test|granite4:3b
optimise|Test|Quick trial of a better wording for a skill|skills optimise {} --model granite4:3b --dry-run|Skill to try|files
test|Dev|Go tests|@go test ./...||
vet|Dev|Vet all six targets|@vet||
site|Dev|Build the docs site|@go run ./scripts/docsite||
readme|Help|Show the guide|@readme||
TABLE
}

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }

list() {
  items | while IFS='|' read -r key group title _; do printf '%-12s %-6s %s\n' "$key" "$group" "$title"; done
}

# ---- which noskill ----
installed() {
  if [ -n "${NOSKILL_BIN:-}" ]; then echo "$NOSKILL_BIN"; return; fi
  c=$(command -v noskill 2>/dev/null || true)
  [ -n "$c" ] && { echo "$c"; return; }
  [ -x "$HOME/.noskill/bin/noskill" ] && echo "$HOME/.noskill/bin/noskill"
  return 0
}

source_bin() {
  command -v go >/dev/null 2>&1 || { echo "this needs Go to build noskill from source (https://go.dev/dl/), or install noskill" >&2; exit 1; }
  dir="${TMPDIR:-/tmp}/noskill-start"
  mkdir -p "$dir"
  out="$dir/noskill"
  if [ ! -x "$out" ] || [ -n "$(find "$here/cmd" "$here/internal" "$here/skills" "$here/go.mod" -newer "$out" \( -name '*.go' -o -name '*.md' -o -name '*.yaml' -o -name '*.html' -o -name go.mod \) 2>/dev/null | head -n 1)" ]; then
    echo "building noskill from $here …" >&2
    (cd "$here" && go build -o "$out" ./cmd/noskill)
  fi
  echo "$out"
}

# Does this noskill know the command (and the skill, if the item names one)?
supported() { # supported <bin> <template>
  set -f; set -- "$1" $2; set +f
  b=$1; first=$2
  "$b" help 2>&1 | grep -q "^  $first " || return 1
  while [ $# -gt 0 ]; do
    if [ "$1" = "--skill" ] && [ $# -gt 1 ]; then "$b" skills list 2>&1 | grep -q "^$2\b" || return 1; fi
    shift
  done
  return 0
}

pick_bin() { # pick_bin <template>; prints the noskill to use
  if [ "$from_source" = 1 ]; then if [ "$dry" = 1 ]; then echo "<noskill built from this source>"; else source_bin; fi; return; fi
  b=$(installed)
  if [ -n "$b" ] && supported "$b" "$1"; then echo "$b"; return; fi
  if [ -n "$b" ]; then echo "(the installed noskill doesn't have this yet: using the current source)" >&2; fi
  if [ "$dry" = 1 ]; then echo "<noskill built from this source>"; return; fi
  source_bin
}

# ---- run one item ----
run_item() { # run_item <key> [words…]
  key=$1; shift
  line=$(items | grep "^$key|" || true)
  [ -n "$line" ] || { echo "no item called '$key'. ./start.sh --list shows them." >&2; return 2; }
  IFS='|' read -r _ _ title tpl ask def <<EOF2
$line
EOF2
  case "$tpl" in
    @readme) ${PAGER:-cat} "$here/docs/START.md"; return ;;
    @vet)
      for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
        printf '%s: ' "$t"; (cd "$here" && GOOS=${t%/*} GOARCH=${t#*/} go vet ./... && echo ok) || return 1
      done
      return ;;
    @*) tpl=${tpl#@}; if [ "$dry" = 1 ]; then echo "(in $here) $tpl"; return; fi
        set -f; set -- $tpl "$@"; set +f; (cd "$here" && "$@"); return ;;
  esac
  answer=""
  case "$tpl" in
    *"{}"*)
      if [ $# -gt 0 ]; then answer="$*"
      elif [ -t 0 ] && [ -n "$ask" ]; then
        printf '%s%s: ' "$ask" "${def:+ [$def]}" >&2; IFS= read -r answer || answer=""
      fi
      [ -n "$answer" ] || answer=$def
      [ -n "$answer" ] || { echo "'$key' needs: ${ask:-something to work on}" >&2; return 2; } ;;
  esac
  bin=$(pick_bin "$tpl")
  # Build the command line: {} becomes the answer as one argument; extra words go on the end otherwise.
  set -f; set -- $tpl; set +f
  n=$#; i=0
  while [ $i -lt $n ]; do w=$1; shift; [ "$w" = "{}" ] && w=$answer; set -- "$@" "$w"; i=$((i + 1)); done
  if [ "$dry" = 1 ]; then printf '%s' "$bin"; for w in "$@"; do printf " '%s'" "$w"; done; echo; return; fi
  echo "→ noskill $*" >&2
  "$bin" "$@"
}

menu() {
  while :; do
    echo
    echo "noskill: what do you want to do?"
    n=0; g=""
    while IFS='|' read -r key group title _; do
      n=$((n + 1))
      [ "$group" = "$g" ] || { printf '\n  %s\n' "$group"; g=$group; }
      printf '   %2d  %-12s %s\n' "$n" "$key" "$title"
    done <<EOF3
$(items)
EOF3
    printf '\nNumber or name (h help, q quit): '
    IFS= read -r pick || return 0
    case "$pick" in
      q|quit|exit|"") return 0 ;;
      h|help|\?) usage; continue ;;
      *[!0-9]*) key=$pick ;;
      *) key=$(items | sed -n "${pick}p" | cut -d'|' -f1) ;;
    esac
    [ -n "$key" ] || { echo "no such item"; continue; }
    run_item "$key" || true
    printf '\nPress Enter for the menu. '
    IFS= read -r _ || return 0
  done
}

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help|help) usage; exit 0 ;;
    -l|--list) list; exit 0 ;;
    --source) from_source=1 ;;
    -n|--dry) dry=1 ;;
    -*) echo "unknown option: $1 (./start.sh --help)" >&2; exit 2 ;;
    *) break ;;
  esac
  shift
done

if [ $# -gt 0 ]; then run_item "$@"; exit $?; fi
if [ -t 0 ]; then menu; else usage; fi
