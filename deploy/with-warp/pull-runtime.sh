#!/bin/bash
# Copy live CPA state from the old host. This includes account files and config keys.
set -euo pipefail
OLD=${1:?usage: pull-runtime.sh user@old-host}
DEST=${CPA_DIR:-/opt/cli-proxy-api}
install -d "$DEST/auths" "$DEST/plugins" "$DEST/static" "$DEST/data"
rsync -a --chmod=F600 "$OLD:/opt/cli-proxy-api/config.yaml" "$DEST/config.yaml"
rsync -a "$OLD:/opt/cli-proxy-api/auths/" "$DEST/auths/"
rsync -a "$OLD:/opt/cli-proxy-api/plugins/" "$DEST/plugins/"
rsync -a "$OLD:/opt/cli-proxy-api/static/" "$DEST/static/"
rsync -a "$OLD:/opt/cli-proxy-api/data/" "$DEST/data/"
echo "runtime copied to $DEST (logs were not copied)"
