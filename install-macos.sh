#!/bin/sh
set -eu

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' 'This installer is for macOS.'
  exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -m)" in
  arm64|aarch64) BINARY="$SCRIPT_DIR/bin/darwin-arm64/codex-router" ;;
  x86_64|amd64) BINARY="$SCRIPT_DIR/bin/darwin-amd64/codex-router" ;;
  *) printf 'Unsupported Mac architecture: %s\n' "$(uname -m)"; exit 1 ;;
esac

if [ ! -f "$BINARY" ]; then
  printf 'Missing router binary: %s\n' "$BINARY"
  exit 1
fi
chmod 700 "$BINARY"

CODEX_HOME_FLAG="${CODEX_HOME_FLAG:-$HOME/.codex}"
DATA_DIR="$CODEX_HOME_FLAG/codex-model-router"
KEY_FILE="$DATA_DIR/deepseek.key"
PORT="${CODEX_ROUTER_PORT:-49627}"
KEY_WAS_SET=0

if [ -z "${DEEPSEEK_API_KEY:-}" ] && [ ! -f "$KEY_FILE" ]; then
  if [ -r /dev/tty ]; then
    printf 'DeepSeek API Key (input hidden; Enter reuses config.toml): ' >/dev/tty
    saved_stty=$(stty -g </dev/tty)
    trap 'stty "$saved_stty" </dev/tty' EXIT HUP INT TERM
    stty -echo </dev/tty
    IFS= read -r DEEPSEEK_API_KEY </dev/tty || DEEPSEEK_API_KEY=""
    stty "$saved_stty" </dev/tty
    trap - EXIT HUP INT TERM
    printf '\n' >/dev/tty
    if [ -n "$DEEPSEEK_API_KEY" ]; then
      export DEEPSEEK_API_KEY
      KEY_WAS_SET=1
    fi
  else
    printf '%s\n' 'Set DEEPSEEK_API_KEY for first-time installation.'
    exit 1
  fi
fi

cleanup() {
  if [ "$KEY_WAS_SET" -eq 1 ]; then
    unset DEEPSEEK_API_KEY
  fi
}
trap cleanup EXIT HUP INT TERM

"$BINARY" install --codex-home "$CODEX_HOME_FLAG" --port "$PORT"
"$BINARY" status --codex-home "$CODEX_HOME_FLAG"

printf '\n%s\n' 'Installed. Finish active Codex work, restart the desktop app, and start a NEW chat.'
printf '%s\n' 'The native model menu will show OpenAI and DeepSeek models together.'
