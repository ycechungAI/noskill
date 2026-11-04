#!/bin/bash
# Runs the unit tests. Works with only the Command Line Tools installed (no Xcode),
# which ship Swift Testing outside the default search path.
set -euo pipefail
cd "$(dirname "$0")"
DEV="$(xcode-select -p)"
F="$DEV/Library/Developer/Frameworks"
L="$DEV/Library/Developer/usr/lib"
SDK=()
[[ -n "${NOSKILL_SDK:-}" ]] && SDK=(--sdk "$NOSKILL_SDK")
exec swift test --build-system native "${SDK[@]}" \
  -Xswiftc -F -Xswiftc "$F" -Xlinker -F -Xlinker "$F" \
  -Xlinker -rpath -Xlinker "$F" -Xlinker -rpath -Xlinker "$L" "$@"
