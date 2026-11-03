#!/bin/sh
set -eu

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' 'This script is for macOS.'
  exit 1
fi
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CODEX_HOME_FLAG="${CODEX_HOME_FLAG:-$HOME/.codex}"
case "$(uname -m)" in
  arm64|aarch64) BINARY="$SCRIPT_DIR/bin/darwin-arm64/codex-router" ;;
  x86_64|amd64) BINARY="$SCRIPT_DIR/bin/darwin-amd64/codex-router" ;;
  *) printf '%s\n' 'Unsupported Mac architecture.'; exit 1 ;;
esac
if [ ! -f "$BINARY" ]; then
  BINARY="$CODEX_HOME_FLAG/codex-model-router/codex-router"
fi
if [ ! -f "$BINARY" ]; then
  printf '%s\n' 'Router binary missing. Extract the full package and install it first.'
  exit 1
fi
if [ ! -r /dev/tty ]; then
  printf '%s\n' 'A terminal is required for hidden input. For automation use codex-router set-key --stdin.'
  exit 1
fi

saved_stty=$(stty -g </dev/tty)
cleanup() {
  stty "$saved_stty" </dev/tty
  unset new_key
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
printf 'New DeepSeek API Key (hidden; Enter cancels): ' >/dev/tty
stty -echo </dev/tty
IFS= read -r new_key </dev/tty || new_key=""
stty "$saved_stty" </dev/tty
printf '\n' >/dev/tty
if [ -z "$new_key" ]; then
  printf '%s\n' 'Cancelled. The existing key was kept.'
  exit 0
fi
printf '%s' "$new_key" | "$BINARY" set-key --stdin --codex-home "$CODEX_HOME_FLAG"
