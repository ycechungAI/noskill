#!/bin/bash
# Builds the noskill app into apps/macos/build. Usage: apps/macos/build-app.sh [--install]
# Needs only the Command Line Tools, plus Go to put the noskill agent inside the app (Setup
# installs it from there on a Mac that doesn't have it). See docs/APP.md.
set -euo pipefail
cd "$(dirname "$0")"

# Test before build: a build that fails its tests never becomes an app
# (NOSKILL_APP_SKIP_TESTS=1 skips this).
if [[ -z "${NOSKILL_APP_SKIP_TESTS:-}" ]]; then
  LOG=$(mktemp "${TMPDIR:-/tmp}/noskill-app-test.XXXXXX")
  if ! ./test.sh >"$LOG" 2>&1; then
    tail -40 "$LOG"; rm -f "$LOG"
    echo "Tests failed; not building." >&2
    exit 1
  fi
  grep -E "Test run with" "$LOG" || true
  rm -f "$LOG"
fi

FLAGS=(-c release --build-system native -Xswiftc -Osize -Xlinker -dead_strip)
swift build "${FLAGS[@]}"
APP=build/noskill.app
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp -X "$(swift build "${FLAGS[@]}" --show-bin-path)/NoskillApp" "$APP/Contents/MacOS/NoskillApp"
strip -x "$APP/Contents/MacOS/NoskillApp"
cp -X Support/Info.plist "$APP/Contents/Info.plist"
cp -X Support/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"  # drawn by Support/make-icon.swift

# The agent itself, built from this source tree (as ./start.sh install builds it).
GO=$(command -v go || true)
[[ -z "$GO" && -x "$HOME/.local/go/bin/go" ]] && GO="$HOME/.local/go/bin/go"
if [[ -n "$GO" ]]; then
  (cd ../.. && CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o "apps/macos/$APP/Contents/Resources/noskill" ./cmd/noskill)
  codesign --force --options runtime --sign - "$APP/Contents/Resources/noskill"
else
  echo "warning: no Go here, so the app is built without the noskill agent inside it" >&2
fi

find "$APP" -name '._*' -delete
# Ad-hoc signature with the hardened runtime. No App Sandbox: the app starts other programs.
codesign --force --options runtime --sign - "$APP"
codesign --verify --strict --deep "$APP"

echo "Built apps/macos/$APP ($(du -sh "$APP" | cut -f1))"
if [[ "${1:-}" == "--install" ]]; then
  rm -rf /Applications/noskill.app
  cp -R "$APP" /Applications/
  echo "Installed to /Applications/noskill.app"
fi
