#!/bin/sh
# Builds out/NoskillNotify.app (smallest bundle that UNUserNotificationCenter accepts) and ad-hoc signs it.
set -e
cd "$(dirname "$0")"
OUT=../out/NoskillNotify.app
mkdir -p "$OUT/Contents/MacOS"
swiftc -O main.swift -o "$OUT/Contents/MacOS/noskill-notify"
cp Info.plist "$OUT/Contents/Info.plist"
codesign --force --sign - --identifier dev.noskill.s7.notify4 "$OUT"
codesign -dvv "$OUT" 2>&1 | grep -E "Identifier|Signature|TeamIdentifier|flags"
