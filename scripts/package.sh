#!/bin/sh
set -eu
wxbridge_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
wxbridge_dist="$wxbridge_root/dist"
mkdir -p "$wxbridge_dist"
wxbridge_stage=$(mktemp -d "$wxbridge_dist/.stage-XXXXXX")
trap 'rm -rf "$wxbridge_stage"' EXIT HUP INT TERM
wxbridge_package="$wxbridge_stage/wxbridge"
mkdir -p "$wxbridge_package/bin"
cd "$wxbridge_root"
for wxbridge_arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$wxbridge_arch" go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "$wxbridge_package/bin/wxbridge-linux-$wxbridge_arch" ./cmd/wechat-flow
done
for wxbridge_item in README.md API.md PROVENANCE.md go.mod go.sum start.sh Dockerfile compose.yaml .dockerignore .gitignore api client clientsdk cmd common db debug go-redis internal protobuf srv deploy scripts examples; do
  cp -R "$wxbridge_root/$wxbridge_item" "$wxbridge_package/"
done
chmod 755 "$wxbridge_package/start.sh" "$wxbridge_package/scripts/package.sh"
cd "$wxbridge_package"
sha256sum bin/wxbridge-linux-amd64 bin/wxbridge-linux-arm64 > SHA256SUMS
tar -czf "$wxbridge_dist/wxbridge-linux.tar.gz" -C "$wxbridge_stage" wxbridge
printf '%s\n' "$wxbridge_dist/wxbridge-linux.tar.gz"
