#!/bin/sh
# Reproducible build for the TV (ARMv7). The gzip is what the Shard downloads;
# pin its SHA-256 in ../index.js (BINARY.sha256) before publishing a release.
set -eu
cd "$(dirname "$0")"
out=${1:-/tmp/surfsharktv-armv7}
GOTOOLCHAIN=go1.26.0 CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-mod=readonly \
  go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o "$out" .
gzip -9 -n -c "$out" > "$out.gz"
echo "built $out.gz"
shasum -a 256 "$out.gz" || sha256sum "$out.gz"
