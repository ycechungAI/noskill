#!/bin/sh
# Signs checksums.txt with the ed25519 release key; `noskill update` verifies this signature offline
# with the public key embedded in the binary. Usage: NOSKILL_RELEASE_KEY=<base64 seed> sign-checksums.sh <file>
# Writes <file>.ed25519 (base64). Uses the Go toolchain only, so it needs nothing else installed.
set -eu
file="${1:?usage: sign-checksums.sh <checksums.txt>}"
: "${NOSKILL_RELEASE_KEY:?set NOSKILL_RELEASE_KEY (base64 ed25519 seed)}"
cd "$(dirname "$0")/.."
go run ./internal/update/cmd/sign "$file" > "$file.ed25519"
echo "signed $file -> $file.ed25519"
