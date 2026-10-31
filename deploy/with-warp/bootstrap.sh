#!/bin/bash
# Build the 217 CPA image and start it on the warp-lb network.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
DEST=${CPA_DIR:-/opt/cli-proxy-api}
WARP_SRC=${WARP_SRC:-}

if [ -z "$WARP_SRC" ] && [ -f "$ROOT/../warp-lb-rotate-agent/bootstrap.sh" ]; then
  WARP_SRC=$(cd "$ROOT/../warp-lb-rotate-agent" && pwd)
fi
if [ -z "$WARP_SRC" ] && [ -d /opt/src/warp-lb-rotate-agent ]; then
  WARP_SRC=/opt/src/warp-lb-rotate-agent
fi
if [ ! -d "$WARP_SRC" ]; then
  echo "set WARP_SRC to a checkout of llmdph/warp-lb-rotate-agent" >&2
  exit 1
fi

sudo bash "$WARP_SRC/bootstrap.sh"

echo "building cli-proxy-api:http-pool"
docker build -t cli-proxy-api:http-pool "$ROOT"

install -d "$DEST/auths" "$DEST/logs" "$DEST/plugins" "$DEST/data" "$DEST/static"
install -m 0644 "$HERE/docker-compose.yml" "$DEST/docker-compose.yml"
if [ ! -f "$DEST/config.yaml" ]; then
  install -m 0600 "$HERE/config.runtime.example.yaml" "$DEST/config.yaml"
  echo "wrote $DEST/config.yaml from the example; replace api-keys and management.secret-key"
fi
if [ ! -f "$DEST/static/management.html" ]; then
  install -m 0644 "$HERE/static/management.html" "$DEST/static/management.html"
fi

docker compose -f "$DEST/docker-compose.yml" up -d
docker compose -f "$DEST/docker-compose.yml" ps
echo "CPA_READY dir=$DEST"
