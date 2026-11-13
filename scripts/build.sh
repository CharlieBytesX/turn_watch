#!/bin/sh
# Compila los 4 binarios estáticos (sin CGO) en dist/ y, con --package, los
# empaqueta como turn_watch-<os>-<arch>.tar.gz con su SHA256SUMS.
#
#   scripts/build.sh [--package] [versión]
set -eu

package=
if [ "${1:-}" = "--package" ]; then
  package=1
  shift
fi
version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  out="dist/turn_watch-$os-$arch"
  mkdir -p "$out"
  echo "→ $os/$arch ($version)"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "$out/turn_watch" .
  if [ -n "$package" ]; then
    cp LICENSE README.md "$out/"
    tar -czf "dist/turn_watch-$os-$arch.tar.gz" -C "$out" turn_watch LICENSE README.md
  fi
done

if [ -n "$package" ]; then
  (
    cd dist
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum ./*.tar.gz | sed 's| \./| |' >SHA256SUMS
    else
      shasum -a 256 ./*.tar.gz | sed 's| \./| |' >SHA256SUMS
    fi
    cat SHA256SUMS
  )
fi
