#!/bin/sh
set -eu
wxbridge_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -m)" in
  x86_64|amd64) wxbridge_arch=amd64 ;;
  aarch64|arm64) wxbridge_arch=arm64 ;;
  *) echo "Unsupported architecture; build ./cmd/wechat-flow with Go." >&2; exit 1 ;;
esac
wxbridge_binary="$wxbridge_root/bin/wxbridge-linux-$wxbridge_arch"
if [ ! -x "$wxbridge_binary" ]; then wxbridge_binary="$wxbridge_root/bin/wxbridge"; fi
if [ ! -x "$wxbridge_binary" ]; then
  echo "Binary missing. Build with: CGO_ENABLED=0 go build -o bin/wxbridge ./cmd/wechat-flow" >&2
  exit 1
fi
exec "$wxbridge_binary" -state "${WECHAT_STATE_DIR:-$wxbridge_root/state}" serve \
  -listen "${WECHAT_LISTEN:-127.0.0.1:8787}" \
  -auto-download="${WECHAT_AUTO_DOWNLOAD:-true}" "$@"
