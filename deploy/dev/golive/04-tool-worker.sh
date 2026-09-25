#!/usr/bin/env bash
# NODE. Build + prove + publish the tool-worker (tmux, script, bx) against the live AX build.
source "$(dirname "$0")/env.sh"
cd "$TREE"
bash deploy/tool-worker/build.sh "$AX" "$STATE/tool-worker-check-unused" --check
wb=$DEV/tool-worker-mvp-$S-$(date +%s)
bash deploy/tool-worker/build.sh "$AX" "$wb" --buildah
tag=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["local_tag"])' "$wb/proof.json")
c=$(buildah from "$tag"); trap 'buildah rm "$c" >/dev/null' EXIT
buildah run --network none "$c" -- sh -c 'tmux -V && script --version && test "$(readlink /usr/local/bin/bx)" = blaxsmith-tool-worker && git --version'
bash deploy/tool-worker/publish-buildah.sh "$wb" "127.0.0.1:5001/blaxsmith-tool-worker:${tag##*:}"
python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["image"])' "$wb/registry-proof.json" > "$STATE/worker.image"
echo "$wb" > "$STATE/worker-build.txt"
echo "WORKER=$(cat "$STATE/worker.image")"
