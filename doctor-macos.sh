#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -m)" in
  arm64|aarch64) BINARY="$SCRIPT_DIR/bin/darwin-arm64/codex-router" ;;
  x86_64|amd64) BINARY="$SCRIPT_DIR/bin/darwin-amd64/codex-router" ;;
  *) printf 'Unsupported Mac architecture: %s\n' "$(uname -m)"; exit 1 ;;
esac
chmod 700 "$BINARY"
exec "$BINARY" doctor --codex-home "${CODEX_HOME_FLAG:-$HOME/.codex}"
