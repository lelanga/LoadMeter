#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "dist/loadmeter-$os-$arch$ext" ./cmd/loadmeter
done
(cd dist && sha256sum loadmeter-* > SHA256SUMS)
ls -l dist
