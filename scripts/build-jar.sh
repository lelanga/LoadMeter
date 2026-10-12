#!/bin/sh
# Builds dist/LoadMeter.jar: a Java Swing launcher that bundles the Load Meter engine
# for every platform in dist/ (run scripts/dist.sh first). Needs a JDK (8+) only to BUILD;
# running the jar needs a Java 8+ runtime.
# Usage: scripts/build-jar.sh [all|windows]   (default: all)
set -eu
cd "$(dirname "$0")/.."
which="${1:-all}"
out=dist/LoadMeter.jar
[ "$which" = windows ] && out=dist/LoadMeter-windows.jar
b=$(mktemp -d)
trap 'rm -rf "$b"' EXIT
mkdir -p "$b/classes" "$b/jar/natives"
javac --release 8 -Xlint:all -d "$b/classes" launcher/src/com/loadmeter/Launcher.java
cp -r "$b/classes/." "$b/jar/"
n=0
for t in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64; do
  case "$which:$t" in windows:windows-*|all:*) ;; *) continue ;; esac
  ext=""; case "$t" in windows-*) ext=.exe ;; esac
  [ -f "dist/loadmeter-$t$ext" ] || { echo "missing dist/loadmeter-$t$ext (run scripts/dist.sh)" >&2; exit 1; }
  mkdir -p "$b/jar/natives/$t"
  cp "dist/loadmeter-$t$ext" "$b/jar/natives/$t/loadmeter$ext"
  n=$((n+1))
done
git rev-parse --short HEAD > "$b/jar/natives/BUILD" 2>/dev/null || echo dev > "$b/jar/natives/BUILD"
printf 'Main-Class: com.loadmeter.Launcher\nImplementation-Title: Load Meter\n' > "$b/manifest.txt"
jar cfm "$out" "$b/manifest.txt" -C "$b/jar" .
echo "built $out with $n engine(s): $(du -h "$out" | cut -f1)"
