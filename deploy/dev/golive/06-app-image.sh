#!/usr/bin/env bash
# NODE. Build + push the app image (Dockerfile target app) and record its digest.
source "$(dirname "$0")/env.sh"
cd "$TREE"
buildah bud --format oci --arch amd64 --network host --target app -t "127.0.0.1:5001/blaxsmith-app:$S" .
buildah push --tls-verify=false --digestfile "$STATE/app-digest.txt" \
  "127.0.0.1:5001/blaxsmith-app:$S" "docker://127.0.0.1:5001/blaxsmith-app:$S"
digest=$(cat "$STATE/app-digest.txt"); [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]]
curl -sfI -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://127.0.0.1:5001/v2/blaxsmith-app/manifests/$digest" >/dev/null
echo "127.0.0.1:5001/blaxsmith-app@$digest" > "$STATE/app.image"
echo "APP=$(cat "$STATE/app.image")"
