#!/bin/sh
# noskill installer for macOS and Linux (docs/PACKAGING.md "Install scripts"). Per-user: no sudo.
#
#   NOSKILL_REPO=owner/name sh install.sh [--no-init] [--version v0.1.0] [--dir ~/.local/bin]
#
# It downloads the release archive and checksums.txt over HTTPS, checks the SHA-256 (and the signature
# when cosign or gh is installed), installs the binary, and runs `noskill init`. It never installs
# Ollama: `noskill init` tells you where to get it.
set -eu

repo="${NOSKILL_REPO:-}"
version="${NOSKILL_VERSION:-latest}"
dir="${NOSKILL_INSTALL_DIR:-$HOME/.local/bin}"
init=1
while [ $# -gt 0 ]; do
  case "$1" in
    --no-init) init=0 ;;
    --version) shift; version="${1:?--version needs a value}" ;;
    --dir) shift; dir="${1:?--dir needs a value}" ;;
    --repo) shift; repo="${1:?--repo needs a value}" ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done
[ -n "$repo" ] || { echo "set NOSKILL_REPO=owner/name (the GitHub repository to install from)" >&2; exit 2; }

say() { printf '%s\n' "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || { say "this installer needs '$1'"; exit 1; }; }
need curl; need tar; need uname

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) say "unsupported system: $(uname -s). On Windows use install.ps1."; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) say "unsupported CPU: $(uname -m)"; exit 1 ;;
esac
[ "$os" = darwin ] && arch=universal   # one macOS binary for both CPUs

if [ "$version" = latest ]; then
  version="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$version" ] || { say "couldn't find the latest release of $repo"; exit 1; }
fi
ver="${version#v}"
name="noskill_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
say "downloading $name ($version)"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want="$(grep " $name\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$want" ] || { say "$name isn't listed in checksums.txt"; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/$name" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then got="$(shasum -a 256 "$tmp/$name" | awk '{print $1}')"
else say "need sha256sum or shasum to verify the download"; exit 1; fi
[ "$got" = "$want" ] || { say "checksum mismatch: the download is not what the release lists. Nothing was installed."; exit 1; }
say "checksum ok"

if command -v cosign >/dev/null 2>&1 && curl -fsSL -o "$tmp/checksums.txt.sigstore.json" "$base/checksums.txt.sigstore.json" 2>/dev/null; then
  cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "https://github.com/$repo/" --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$tmp/checksums.txt" >/dev/null 2>&1 && say "signature ok (cosign)" || { say "signature check FAILED; nothing was installed."; exit 1; }
else
  say "to check the signature yourself: gh attestation verify <file> --repo $repo   (or cosign verify-blob)"
fi

tar -xzf "$tmp/$name" -C "$tmp" noskill
mkdir -p "$dir"
mv "$tmp/noskill" "$dir/noskill.new"
chmod 755 "$dir/noskill.new"
mv "$dir/noskill.new" "$dir/noskill"      # atomic replace
say "installed $dir/noskill"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "add $dir to your PATH, for example:  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
if [ "$init" = 1 ]; then
  "$dir/noskill" init || say "run 'noskill init' later to finish setup"
fi
