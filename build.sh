#!/bin/bash
# noskill — root build, for macOS. One command from a fresh clone:
#
#   git clone https://github.com/ycechungAI/noskill && cd noskill && ./build.sh
#
# What it does (in order):
#   1. heal    — delete AppleDouble (._*) / .DS_Store junk that broke or noised
#                 up earlier builds, and stale build outputs.
#   2. check   — verify the build dependencies (Go, Swift/Command Line Tools,
#                 macOS packaging tools). If one is missing it prints the exact
#                 install command and stops; it never installs system-wide
#                 software by itself.
#   3. build   — the Go agent to build/noskill, then the app + DMG by running
#                 apps/macos/make-dmg.sh (which runs the app's test suite first,
#                 builds the agent into the app, packages the DMG, verifies the
#                 signature, and starts the app from the mounted DMG as a real
#                 double-click would).
#
# Usage:
#   ./build.sh            full build: heal + check + Go agent + app + DMG (default)
#   ./build.sh --no-dmg    stop after the app bundle (no DMG)
#   ./build.sh --skip-tests  skip the app's test suite (not recommended)
#   ./build.sh --deps      first-pull setup only: check deps and set up the
#                          sched and orcastrator .venvs (needs network once;
#                          delegates to their own self-healing setup scripts)
#   ./build.sh --heal      only run the repo-local fix-ups, build nothing
#   ./build.sh --help
#
# Notes:
#   - `./start.sh install` is the *user* installer (installs noskill to
#     ~/.local/bin, plus Go / Ollama / models if missing). This script is the
#     *developer* build: it produces the app bundle and the release DMG, and
#     never installs anything system-wide.
#   - A first pull needs network once (Go modules, SwiftPM, pip). Everything
#     after that is offline except for model downloads and git itself.
#   - The DMG is ad hoc signed, not notarized; see docs/APP.md "Install from
#     the DMG". Its SHA-256 is written next to it as noskill-<version>.dmg.sha256.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

BUILD_DMG=1
SKIP_TESTS=0
DO_DEPS=0
DO_HEAL_ONLY=0

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() { local end; end=$(grep -n '^set -euo pipefail' "$0" | head -1 | cut -d: -f1); sed -n "2,$((end - 1))p" "$0" | sed 's/^# \{0,1\}//'; }

for arg in "$@"; do
  case "$arg" in
    --no-dmg)     BUILD_DMG=0 ;;
    --skip-tests) SKIP_TESTS=1 ;;
    --deps)       DO_DEPS=1 ;;
    --heal)       DO_HEAL_ONLY=1 ;;
    --help|-h)    usage; exit 0 ;;
    *) echo "unknown flag: $arg (try --help)" >&2; exit 2 ;;
  esac
done

# --- 1. Heal repo-local breakage ---------------------------------------------
heal() {
  log "Healing: AppleDouble / .DS_Store junk"
  find . -path ./.git -prune -o -type f \( -name '._*' -o -name '.DS_Store' \) -print0 \
    | xargs -0 rm -f
  log "Healing: stale build outputs"
  rm -rf build/noskill apps/macos/build/noskill.app
}

# --- 2. Dependency check ------------------------------------------------------
need() { # need <cmd> <install hint>
  if command -v "$1" >/dev/null 2>&1; then
    printf '  ok   %s (%s)\n' "$1" "$(command -v "$1")"
  else
    printf '  MISS %s — %s\n' "$1" "$2" | tee -a /tmp/noskill-build-missing >&2
    return 1
  fi
}

check_deps() {
  local missing=0
  log "Checking build dependencies"
  rm -f /tmp/noskill-build-missing

  [[ "$(uname)" == "Darwin" ]] || \
    die "the app and DMG only build on macOS. For the Go agent alone: go build -o noskill ./cmd/noskill"

  GO="$(command -v go || true)"
  [[ -z "$GO" && -x "$HOME/.local/go/bin/go" ]] && GO="$HOME/.local/go/bin/go"
  if [[ -n "$GO" ]]; then
    printf '  ok   go (%s)\n' "$("$GO" version | awk '{print $3}')"
  else
    printf '  MISS go — install with ./start.sh install (fetches it), https://go.dev/dl/, or: brew install go\n' | tee -a /tmp/noskill-build-missing >&2
    missing=1
  fi

  # Command Line Tools alone ship a macOS 27 SDK whose SwiftUI needs a macro plugin they don't
  # include, so the app fails to compile. The 26.x SDK they also ship builds fine: use it.
  if [[ -z "${NOSKILL_SDK:-}" ]] && [[ "$(xcode-select -p 2>/dev/null)" == */CommandLineTools ]]; then
    NOSKILL_SDK="$(ls -d /Library/Developer/CommandLineTools/SDKs/MacOSX26.*.sdk /Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk 2>/dev/null | sort -V | tail -1 || true)"
    [[ -n "$NOSKILL_SDK" ]] && export NOSKILL_SDK
  fi
  [[ -n "${NOSKILL_SDK:-}" ]] && printf '  ok   SDK for the app: %s\n' "$NOSKILL_SDK"

  need swift "Command Line Tools: xcode-select --install" || missing=1
  need git   "xcode-select --install (or https://git-scm.com)" || missing=1
  need python3 "not strictly required to build; needed at runtime for sched/orcastrator (./build.sh --deps)" || true
  for tool in codesign hdiutil /usr/libexec/PlistBuddy; do
    if [[ -x "$tool" ]] || command -v "$tool" >/dev/null 2>&1; then
      printf '  ok   %s\n' "$tool"
    else
      printf '  MISS %s — macOS packaging tool, should exist; cannot continue\n' "$tool" | tee -a /tmp/noskill-build-missing >&2
      missing=1
    fi
  done

  if (( missing )); then
    echo >&2
    echo "Missing dependencies (see above). Nothing was installed system-wide." >&2
    echo "First pull also needs network once (Go modules, SwiftPM, pip)." >&2
    exit 1
  fi
}

# --- First-pull runtime setup (delegates to the subprojects' own healers) ----
setup_runtime_deps() {
  log "Setting up the scheduler .venv (its own self-healing setup)"
  ( cd apps/scheduler && . ./_venv.sh )
  log "Setting up the orcastrator .venv (its own self-healing setup)"
  ( cd apps/orcastrator && ./start.sh --setup )
  log "Runtime deps OK. (Ollama and the models are the app's Setup step on first launch.)"
}

# --- 3. Build ------------------------------------------------------------------
build_go() {
  mkdir -p build
  log "Building noskill Go agent -> build/noskill"
  CGO_ENABLED=0 "${GO:-$(command -v go)}" build -trimpath -ldflags "-s -w" -o build/noskill ./cmd/noskill
}

if (( DO_DEPS )); then
  check_deps
  setup_runtime_deps
  exit 0
fi

heal
if (( DO_HEAL_ONLY )); then
  log "Heal done (--heal); nothing built."
  exit 0
fi
check_deps
build_go

if (( BUILD_DMG )); then
  log "Building app + DMG (this runs the app's tests first)"
  if (( SKIP_TESTS )); then
    ( cd apps/macos && NOSKILL_APP_SKIP_TESTS=1 ./make-dmg.sh )
  else
    ( cd apps/macos && ./make-dmg.sh )
  fi
  log "Done. Artifacts:"
  ls -la apps/macos/build/noskill-*.dmg apps/macos/build/noskill-*.dmg.sha256
else
  log "Building macOS app (--no-dmg)"
  if (( SKIP_TESTS )); then
    ( cd apps/macos && NOSKILL_APP_SKIP_TESTS=1 ./build-app.sh )
  else
    ( cd apps/macos && ./build-app.sh )
  fi
  log "Done. Artifact: apps/macos/build/noskill.app"
fi
