#!/bin/sh
# Build the Surf Control Tizen widget. Optionally sign it with apps2samsung-cli.
#   ./build.sh                                  # build surf-control.wgt
#   A2S=apps2samsung-cli DUID=... ./build.sh     # also write surf-control-signed.wgt
set -eu
cd "$(dirname "$0")"

rm -f surf-control.wgt
zip -q surf-control.wgt config.xml index.html icon.png
echo "built surf-control.wgt"

if [ -n "${A2S:-}" ] && [ -n "${DUID:-}" ]; then
    rm -f surf-control-signed.wgt
    "$A2S" sign surf-control.wgt -o surf-control-signed.wgt --duids "$DUID" --partner
    echo "built surf-control-signed.wgt"
fi
