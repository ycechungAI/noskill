#!/bin/bash
# Builds the noskill app and packages it as apps/macos/build/noskill-<version>.dmg (drag the app
# to Applications), then checks the DMG: mounts it, verifies the signatures and starts the app
# from it with --self-test, which looks at this Mac without changing anything and quits.
# Ad-hoc signed, not notarized (docs/APP.md "Install from the DMG").
set -euo pipefail
cd "$(dirname "$0")"

./build-app.sh
VER=$(/usr/libexec/PlistBuddy -c "Print CFBundleShortVersionString" Support/Info.plist)
DMG=build/noskill-$VER.dmg
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/noskill-dmg.XXXXXX")
MNT=$(mktemp -d "${TMPDIR:-/tmp}/noskill-mnt.XXXXXX")
OUT="$STAGE.selftest"
cleanup() {
  hdiutil detach -quiet "$MNT" 2>/dev/null || true
  rm -rf "$STAGE" "$OUT"
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

cp -R build/noskill.app "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$DMG"
hdiutil create -quiet -volname "noskill $VER" -srcfolder "$STAGE" -ov -format UDZO "$DMG"
echo "Built apps/macos/$DMG ($(du -h "$DMG" | cut -f1))"

echo "Checking the DMG…"
hdiutil verify -quiet "$DMG"
hdiutil attach -quiet -nobrowse -readonly -noautoopen -mountpoint "$MNT" "$DMG"
[[ -L "$MNT/Applications" ]] || { echo "the DMG has no Applications link" >&2; exit 1; }
codesign --verify --strict --deep "$MNT/noskill.app"
codesign -dv "$MNT/noskill.app" 2>&1 | grep -E "^(Identifier|Signature|CodeDirectory)" | sed 's/^/  /'
[[ -x "$MNT/noskill.app/Contents/Resources/noskill" ]] && codesign --verify --strict "$MNT/noskill.app/Contents/Resources/noskill"

# Start it through Launch Services, as a double-click would (-n: even if noskill is already open).
open -n -W --stdout "$OUT" --stderr "$OUT" "$MNT/noskill.app" --args --self-test
sed 's/^/  /' "$OUT"
grep -q "^noskill app $VER started" "$OUT" || { echo "the app didn't start from the DMG" >&2; exit 1; }
grep -q "^bundled agent: .* runs$" "$OUT" || { echo "the agent inside the app doesn't run" >&2; exit 1; }
echo "OK: apps/macos/$DMG mounts, is signed (ad hoc) and starts."
