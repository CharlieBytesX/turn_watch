#!/bin/sh
# turn_watch installer.
#
#   curl -fsSL https://raw.githubusercontent.com/CharlieBytesX/turn_watch/main/install.sh | sh
#
# Downloads the release binary for this OS/arch, verifies its SHA-256 against the
# release's SHA256SUMS, puts it in ~/.local/bin and runs `turn_watch install`.
#
# Environment:
#   TURN_WATCH_VERSION=v0.1.0   install that tag instead of the latest release
#   TURN_WATCH_NO_SETUP=1       only install the binary (skip `turn_watch install`)
#   TURN_WATCH_SIN_KACHE=1      passed through: `turn_watch install` skips kache
set -eu

REPO="CharlieBytesX/turn_watch"
BIN_DIR="$HOME/.local/bin"

say() { printf 'turn_watch: %s\n' "$*"; }
die() { printf 'turn_watch: error: %s\n' "$*" >&2; exit 1; }

# --- platform --------------------------------------------------------------
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "no release binary for OS '$os' (only Linux and macOS)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "no release binary for architecture '$arch' (only amd64 and arm64)" ;;
esac
# A shell running under Rosetta reports x86_64 on Apple Silicon: use the native one.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
  [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
  arch=arm64
fi

# --- tools -----------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "needs sha256sum or shasum to verify the download"
fi

command -v tar >/dev/null 2>&1 || die "needs tar"

# --- download --------------------------------------------------------------
asset="turn_watch-$os-$arch.tar.gz"
version="${TURN_WATCH_VERSION:-}"
if [ -n "$version" ]; then
  base="https://github.com/$REPO/releases/download/$version"
  say "installing $version for $os/$arch"
else
  base="https://github.com/$REPO/releases/latest/download"
  say "installing the latest release for $os/$arch"
fi

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t turn_watch)
trap 'rm -rf "$tmp"' EXIT INT TERM

fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" ||
  die "could not download SHA256SUMS from $base (does the release exist?)"
want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "the release has no binary for $os/$arch ($asset)"

fetch "$base/$asset" "$tmp/$asset" || die "could not download $base/$asset"
got=$(sha256 "$tmp/$asset")
[ "$got" = "$want" ] || die "SHA-256 mismatch for $asset (got $got, want $want)"
say "SHA-256 verified"

mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x" turn_watch || die "could not unpack $asset"

# --- install ---------------------------------------------------------------
mkdir -p "$BIN_DIR"
# Copy next to the destination and rename: replacing a running binary is safe.
cp "$tmp/x/turn_watch" "$BIN_DIR/.turn_watch.new"
chmod 755 "$BIN_DIR/.turn_watch.new"
mv -f "$BIN_DIR/.turn_watch.new" "$BIN_DIR/turn_watch"
say "installed $("$BIN_DIR/turn_watch" version) in $BIN_DIR/turn_watch"

if [ -n "${TURN_WATCH_NO_SETUP:-}" ]; then
  say "TURN_WATCH_NO_SETUP is set: run '$BIN_DIR/turn_watch install' when ready"
  exit 0
fi

# `turn_watch install` sets up the cargo wrapper, the background service and kache.
"$BIN_DIR/turn_watch" install </dev/null

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "add $BIN_DIR to your PATH (before ~/.cargo/bin) so the cargo wrapper is used" ;;
esac
