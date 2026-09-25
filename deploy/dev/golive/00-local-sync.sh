#!/usr/bin/env bash
# WORKSTATION. Usage: bash 00-local-sync.sh [BUNDLE]   (no BUNDLE: build one from the branch)
set -euo pipefail
REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
KEY=${KEY:-~/.ssh/mj_hetzner_091123} NODE=${NODE:-root@135.181.34.21}
REF=${BUNDLE_REF:-refs/heads/main}
BUNDLE=${1:-}
if [ -z "$BUNDLE" ]; then
  SHA=$(git -C "$REPO" rev-parse "$REF"); S=${SHA:0:7}
  BUNDLE=/tmp/blaxsmith-mvp-$S.bundle
  git -C "$REPO" bundle create "$BUNDLE" 5472952.."$REF"
else
  SHA=$(git bundle list-heads "$BUNDLE" "$REF" | cut -d' ' -f1); S=${SHA:0:7}
fi
test -n "$SHA" || { echo "bundle has no $REF" >&2; exit 2; }
git -C "$REPO" bundle verify "$BUNDLE"
scp -i "$KEY" "$BUNDLE" "$NODE:/opt/blaxsmith-dev/blaxsmith-mvp-$S.bundle"
ssh -i "$KEY" "$NODE" "mkdir -p /opt/blaxsmith-dev/golive-$S"
scp -i "$KEY" "$(dirname "$0")"/*.sh "$(dirname "$0")"/*.yaml "$NODE:/opt/blaxsmith-dev/golive-$S/"
echo "S=$S  (node scripts: ssh ... 'cd /opt/blaxsmith-dev/golive-$S && S=$S BUNDLE_REF=$REF bash 01-node-sync.sh')"
