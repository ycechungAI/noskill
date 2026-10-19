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
install|Setup|Build and install noskill (and Go, Ollama, models if missing)|@install||
ollama|Setup|Install Ollama if missing and download the recommended models|@ollama||
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

# Scratch folders this script makes (mktemp, ours alone) are removed when it exits.
TMPS=""
add_tmp() { TMPS="$TMPS $1"; }
trap 'for d in $TMPS; do rm -rf "$d"; done' EXIT

# Does this computer have Go? If not, offer the official build from go.dev in ~/.local/go
# (NOSKILL_GO_DIR to change it): no sudo, SHA-256 checked against dl.google.com, nothing else touched.
# On success `go` is on PATH for the rest of this script. $1 = 1 to skip the question.
ensure_go() {
  command -v go >/dev/null 2>&1 && return 0
  gd=${NOSKILL_GO_DIR:-$HOME/.local/go}
  if [ -x "$gd/bin/go" ]; then PATH="$gd/bin:$PATH"; export PATH; return 0; fi
  for t in curl tar uname; do command -v "$t" >/dev/null 2>&1 || { echo "to fetch Go this needs '$t'; or install Go yourself: https://go.dev/dl/" >&2; return 1; }; done
  case "$(uname -s)" in Darwin) gos=darwin ;; Linux) gos=linux ;; *) echo "install Go yourself: https://go.dev/dl/" >&2; return 1 ;; esac
  case "$(uname -m)" in x86_64|amd64) goarch=amd64 ;; arm64|aarch64) goarch=arm64 ;; *) echo "install Go yourself: https://go.dev/dl/" >&2; return 1 ;; esac
  ver=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -n 1)
  case "$ver" in go1.*) ;; *) echo "couldn't find the current Go version; install Go yourself: https://go.dev/dl/" >&2; return 1 ;; esac
  if [ "${1:-0}" != 1 ]; then
    [ -t 0 ] || { echo "Go isn't installed. Run from a terminal, or add -y to fetch it." >&2; return 2; }
    printf "Go isn't installed. Download %s (about 70 MB, from go.dev, checksum checked) into %s? [y/N] " "$ver" "$gd" >&2
    IFS= read -r a || a=n
    case "$a" in y|Y|yes) ;; *) echo "not installed; get Go from https://go.dev/dl/ and run this again" >&2; return 1 ;; esac
  fi
  [ ! -e "$gd" ] || { echo "$gd exists but has no bin/go; move it away or set NOSKILL_GO_DIR" >&2; return 1; }
  file="$ver.$gos-$goarch.tar.gz"
  gt=$(mktemp -d "${TMPDIR:-/tmp}/noskill-go.XXXXXX"); add_tmp "$gt"
  echo "downloading $file …" >&2
  fetch_checked "https://dl.google.com/go/$file" "$gt/$file" "$(curl -fsSL "https://dl.google.com/go/$file.sha256" | awk '{print $1}')" || return 1
  tar -xzf "$gt/$file" -C "$gt" && mkdir -p "$(dirname "$gd")" && mv "$gt/go" "$gd" || return 1
  PATH="$gd/bin:$PATH"; export PATH
  echo "installed Go ($ver) in $gd. To use it yourself, add $gd/bin to your PATH." >&2
}

# fetch_checked <url> <file> <sha256>: download, then refuse anything that doesn't match the checksum.
fetch_checked() {
  curl -fL --progress-bar -o "$2" "$1" || { echo "download failed: $1" >&2; return 1; }
  if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$2" | awk '{print $1}'); else got=$(shasum -a 256 "$2" | awk '{print $1}'); fi
  if [ -z "$3" ] || [ "$got" != "$3" ]; then echo "the download doesn't match its checksum; nothing was installed" >&2; return 1; fi
}

# The ollama command, wherever this computer keeps it (empty when there is none).
ollama_bin() {
  c=$(command -v ollama 2>/dev/null || true); [ -n "$c" ] && { echo "$c"; return; }
  for c in "$HOME/.local/bin/ollama" "/Applications/Ollama.app/Contents/Resources/ollama" "$HOME/Applications/Ollama.app/Contents/Resources/ollama"; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  return 0
}

# Total memory in GB (0 when unknown); the models offered depend on it.
mem_gb() {
  case "$(uname -s)" in
    Darwin) echo $(( $(/usr/sbin/sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824 )) ;;
    Linux) echo $(( $(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0) / 1048576 )) ;;
    *) echo 0 ;;
  esac
}

# Is Ollama installed? If not, offer to install it (the official release, SHA-256 checked) and
# start it; then offer the models noskill recommends for this much memory. $1 = 1 to skip the
# questions. Models are fetched one at a time and skipped when already there.
setup_ollama() {
  yes=${1:-0}
  ob=$(ollama_bin)
  if [ -z "$ob" ]; then
    [ "$yes" = 1 ] || [ -t 0 ] || { echo "Ollama isn't installed. Run from a terminal, or add -y to install it." >&2; return 2; }
    for t in curl uname; do command -v "$t" >/dev/null 2>&1 || { echo "to install Ollama this needs '$t'; or get it from https://ollama.com/download" >&2; return 1; }; done
    rel=https://github.com/ollama/ollama/releases/latest/download
    sums=$(curl -fsSL "$rel/sha256sum.txt") || { echo "couldn't reach the Ollama release; get it from https://ollama.com/download" >&2; return 1; }
    ot=$(mktemp -d "${TMPDIR:-/tmp}/noskill-ollama.XXXXXX"); add_tmp "$ot"
    case "$(uname -s)" in
      Darwin)
        ask_yes "$yes" "Ollama isn't installed. Download Ollama (about 210 MB, from ollama.com's GitHub release, checksum checked) into $HOME/Applications and start it?" || return 0
        fetch_checked "$rel/Ollama-darwin.zip" "$ot/Ollama-darwin.zip" "$(echo "$sums" | awk '$2 == "./Ollama-darwin.zip" {print $1}')" || return 1
        mkdir -p "$HOME/Applications"
        [ ! -e "$HOME/Applications/Ollama.app" ] || { echo "$HOME/Applications/Ollama.app is already there but isn't usable; move it away and run this again" >&2; return 1; }
        ditto -x -k "$ot/Ollama-darwin.zip" "$HOME/Applications" || return 1
        open -a "$HOME/Applications/Ollama.app" ;;
      Linux)
        command -v zstd >/dev/null 2>&1 || { echo "Ollama's Linux download is .tar.zst: install zstd, or use Ollama's own installer (https://ollama.com/download)" >&2; return 1; }
        case "$(uname -m)" in x86_64|amd64) la=amd64 ;; arm64|aarch64) la=arm64 ;; *) echo "get Ollama from https://ollama.com/download" >&2; return 1 ;; esac
        ask_yes "$yes" "Ollama isn't installed. Download Ollama (about 1.5 GB with its GPU libraries, checksum checked) into $HOME/.local and start it? (no sudo)" || return 0
        fetch_checked "$rel/ollama-linux-$la.tar.zst" "$ot/o.tar.zst" "$(echo "$sums" | awk -v f="./ollama-linux-$la.tar.zst" '$2 == f {print $1}')" || return 1
        mkdir -p "$HOME/.local" && tar --zstd -xf "$ot/o.tar.zst" -C "$HOME/.local" || return 1
        nohup "$HOME/.local/bin/ollama" serve >/dev/null 2>&1 & ;;
      *) echo "get Ollama from https://ollama.com/download" >&2; return 1 ;;
    esac
    ob=$(ollama_bin)
    [ -n "$ob" ] || { echo "Ollama didn't install where expected; get it from https://ollama.com/download" >&2; return 1; }
    echo "installed Ollama: $ob" >&2
  fi
  # Wait for the server (the app or service takes a moment to start).
  i=0; until "$ob" list >/dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -gt 40 ] && { echo "Ollama isn't answering yet: start it, then run this again to fetch the models" >&2; return 1; }
    sleep 2
  done
  gb=$(mem_gb)
  have=$("$ob" list 2>/dev/null | awk 'NR > 1 {print $1}')
  want=""
  if [ "$gb" -ge 8 ] || [ "$gb" = 0 ]; then want="granite4:3b gemma3:1b"; else want="gemma3:1b"; echo "This computer has about $gb GB of memory, so only the small model (gemma3:1b) is suggested." >&2; fi
  todo=""
  for m in $want; do echo "$have" | grep -qx "$m" || todo="$todo $m"; done
  if [ -n "$todo" ]; then
    ask_yes "$yes" "Download the recommended models:$todo (granite4:3b about 2 GB = main, gemma3:1b about 0.8 GB = backup)?" "Y" && for m in $todo; do echo "→ ollama pull $m" >&2; "$ob" pull "$m" || echo "couldn't fetch $m: try  ollama pull $m  later" >&2; done
  else
    echo "the recommended models are already installed ($want)" >&2
  fi
  if [ "$gb" -ge 12 ] && ! echo "$have" | grep -qx "qwen3.5:4b"; then
    if ask_yes 0 "Also download qwen3.5:4b (about 4 GB), the slower, stronger model for hard tasks?"; then "$ob" pull qwen3.5:4b; fi
  fi
  return 0
}

# ask_yes <skip: 1 = yes without asking> <question> [Y]: ask on the terminal; default no unless Y. Sets ASKED=y|n.
ask_yes() {
  ASKED=n
  [ "$1" = 1 ] && { ASKED=y; return 0; }
  [ -t 0 ] || return 1
  if [ "${3:-}" = Y ]; then printf '%s [Y/n] ' "$2" >&2; else printf '%s [y/N] ' "$2" >&2; fi
  IFS= read -r a || a=n
  case "${3:-}:$a" in Y:|Y:y|Y:Y|Y:yes|:y|:Y|:yes) ASKED=y; return 0 ;; esac
  return 1
}

# Build this source tree and install it for the current user (never sudo), then offer `noskill init`.
# It goes to ~/.local/bin (NOSKILL_INSTALL_DIR to change it), a different place from the daemon's
# ~/.noskill/bin. `-y` after the name skips the questions. Go and Ollama are fetched first if missing,
# then the recommended models; each step asks first.
install_noskill() {
  yes=0; [ "${1:-}" = "-y" ] && yes=1
  dir=${NOSKILL_INSTALL_DIR:-$HOME/.local/bin}
  if [ "$dry" = 1 ]; then echo "(in $here) go build -o $dir/noskill ./cmd/noskill, then: $dir/noskill init"; return; fi
  if [ "$yes" = 0 ]; then
    [ -t 0 ] || { echo "run it from a terminal, or add -y:  ./start.sh install -y" >&2; return 2; }
    printf 'Build noskill and install it to %s/noskill (replaces one already there)? [y/N] ' "$dir" >&2
    IFS= read -r a || a=n
    case "$a" in y|Y|yes) ;; *) echo "not installed" >&2; return 0 ;; esac
  fi
  ensure_go "$yes" || return 1
  mkdir -p "$dir"
  (cd "$here" && CGO_ENABLED=0 go build -o "$dir/noskill.new" ./cmd/noskill) || return 1
  mv "$dir/noskill.new" "$dir/noskill"   # one atomic swap
  echo "installed $dir/noskill" >&2
  case ":$PATH:" in *":$dir:"*) ;; *) echo "add $dir to your PATH, for example:  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" >&2 ;; esac
  setup_ollama "$yes" || echo "Ollama and the models can be set up later:  ./start.sh ollama" >&2
  if [ "$yes" = 1 ]; then a=y; else printf 'Run "noskill init" now (finds Ollama and your models, writes a starting config)? [Y/n] ' >&2; IFS= read -r a || a=n; fi
  case "$a" in n|N|no) echo "run it later with: noskill init" >&2 ;; *) "$dir/noskill" init ;; esac
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
    @install) install_noskill "$@"; return ;;
    @ollama) if [ "$dry" = 1 ]; then echo "check for Ollama (install it if missing), then ollama pull the recommended models"; return; fi; y=0; [ "${1:-}" = -y ] && y=1; setup_ollama "$y"; return ;;
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
