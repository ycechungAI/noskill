#!/bin/bash
# Build the noskill project from the repo root:
#   1. the Go agent binary  -> build/noskill
#   2. the macOS app         -> apps/macos/build-app.sh (which itself runs the
#                               app's test suite first, then builds the Go agent
#                               into the app bundle)
# Use --skip-app-tests to build the app without its test suite.
#
# The macOS app can also package itself as a DMG with apps/macos/make-dmg.sh
# (which calls build-app.sh again and then verifies + self-tests the DMG).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

mkdir -p build
echo "==> Building noskill Go agent -> build/noskill"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o build/noskill ./cmd/noskill

if [[ "${1:-}" == "--skip-app-tests" ]]; then
  echo "==> Building macOS app (app tests skipped)"
  ( cd apps/macos && NOSKILL_APP_SKIP_TESTS=1 ./build-app.sh )
else
  echo "==> Building macOS app (runs its test suite first)"
  ( cd apps/macos && ./build-app.sh )
fi

echo "==> Build complete: build/noskill and apps/macos/build/noskill.app"
