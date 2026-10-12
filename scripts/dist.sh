#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${t%/*}; arch=${t#*/}; ext=""; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "dist/loadmeter-$os-$arch$ext" ./cmd/loadmeter
done
# windowless Windows build: no console window, errors shown in a dialog, double-click opens the app window
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H=windowsgui -X main.guiMode=1" -o dist/LoadMeter-App.exe ./cmd/loadmeter
(cd dist && sha256sum loadmeter-* LoadMeter-App.exe > SHA256SUMS)
ls -l dist
