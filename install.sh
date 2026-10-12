#!/bin/sh
# Installs the loadmeter binary into ~/.local/bin (or $PREFIX/bin).
# Uses a prebuilt binary from ./dist when present, otherwise builds from source (needs Go 1.24+).
set -eu
PREFIX="${PREFIX:-$HOME/.local}"
BIN="$PREFIX/bin"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac
case "$os" in linux|darwin) ;; *) echo "unsupported OS: $os (on Windows use dist/loadmeter-windows-$arch.exe)" >&2; exit 1 ;; esac
here=$(cd "$(dirname "$0")" && pwd)
src="$here/dist/loadmeter-$os-$arch"
mkdir -p "$BIN"
if [ -f "$src" ]; then
  if [ -f "$here/dist/SHA256SUMS" ] && command -v sha256sum >/dev/null 2>&1; then
    (cd "$here/dist" && grep " loadmeter-$os-$arch\$" SHA256SUMS | sha256sum -c -) || { echo "checksum mismatch" >&2; exit 1; }
  fi
  install -m 0755 "$src" "$BIN/loadmeter"
elif command -v go >/dev/null 2>&1; then
  (cd "$here" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN/loadmeter" ./cmd/loadmeter)
else
  echo "no prebuilt binary for $os/$arch in dist/ and Go is not installed" >&2; exit 1
fi
echo "installed: $BIN/loadmeter"
case ":$PATH:" in *":$BIN:"*) ;; *) echo "add to PATH: export PATH=\"$BIN:\$PATH\"" ;; esac
"$BIN/loadmeter" version
