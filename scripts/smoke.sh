#!/bin/sh
# Prueba de humo de un binario ya compilado, en un HOME temporal (no toca la
# instalación de quien la corre): help, version, el servicio de verdad, once, on,
# limit, una compilación de cargo por el envoltorio y off. Con --install (para el
# CI de macOS) también `turn_watch install` / `uninstall` con el servicio del sistema.
#
#   scripts/smoke.sh ./turn_watch [--install]
set -eu

bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
with_install=${2:-}

# rustup y cargo siguen usando los de siempre aunque cambie HOME.
export CARGO_HOME="${CARGO_HOME:-$HOME/.cargo}"
export RUSTUP_HOME="${RUSTUP_HOME:-$HOME/.rustup}"
export PATH="$CARGO_HOME/bin:$PATH"

tmp=$(mktemp -d)
export HOME="$tmp/home"
export XDG_RUNTIME_DIR="$tmp/run"
export XDG_CONFIG_HOME="$HOME/.config"
export TURN_WATCH_SIN_KACHE=1
mkdir -p "$HOME" "$XDG_RUNTIME_DIR"
chmod 700 "$XDG_RUNTIME_DIR"

daemon_pid=
cleanup() {
  [ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }

wait_for() { # wait_for <descripción> <comando…>
  what=$1
  shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ $i -ge 60 ]; then
      echo "timeout esperando: $what" >&2
      "$bin" once >&2 || true
      return 1
    fi
    sleep 0.5
  done
}

service_up() { "$bin" once | grep -q '^CPU '; }
limit_on() { "$bin" limit | grep -q 'límite activo'; }
limit_off() { "$bin" limit | grep -q 'límite desactivado'; }

step "help y version"
"$bin" help | head -3
"$bin" version

step "once sin servicio"
"$bin" once
# Nunca el ayudante de root (sudo sin contraseña en el CI): solo este usuario.
"$bin" sistema off

step "el servicio (turn_watch daemon)"
"$bin" daemon 2>"$tmp/daemon.log" &
daemon_pid=$!
wait_for "el servicio" service_up
"$bin" once

step "on y limit"
"$bin" on
wait_for "el límite activo" limit_on
"$bin" limit 2
"$bin" limit

step "cargo por el envoltorio"
mkdir -p "$tmp/bin"
ln -s "$bin" "$tmp/bin/cargo"
(
  export PATH="$tmp/bin:$PATH"
  cd "$tmp"
  cargo new --vcs none --quiet demo
  cd demo
  cargo build --quiet
  ./target/debug/demo
)

step "off"
"$bin" off
wait_for "el límite desactivado" limit_off
kill "$daemon_pid"
wait "$daemon_pid" 2>/dev/null || true
daemon_pid=
cat "$tmp/daemon.log"

if [ "$with_install" = "--install" ]; then
  step "install (servicio del sistema)"
  "$bin" install </dev/null
  wait_for "el servicio instalado" service_up
  "$HOME/.local/bin/turn_watch" once
  "$bin" on
  wait_for "el límite activo (servicio instalado)" limit_on
  "$bin" off
  step "uninstall"
  "$bin" uninstall
fi

printf '\nok\n'
