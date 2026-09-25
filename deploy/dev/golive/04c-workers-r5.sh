#!/usr/bin/env bash
# NODE. Release 5: build + prove + publish BOTH runner variants (default, guild)
# against the OpenCode-enabled AX build. Usage: S=<sha> bash 04b-workers-r4.sh
set -euo pipefail
: "${S:?}"; DEV=/opt/blaxsmith-dev; TREE=$DEV/blaxsmith-mvp-$S; STATE=$DEV/mvp-state-$S
AX=$DEV/ax-build-f009-lanep-1790281634
mkdir -p "$STATE"; chmod 700 "$STATE"; cd "$TREE"
for variant in default guild; do
  flag=(); [ "$variant" = guild ] && flag=(--variant guild)
  bash deploy/tool-worker/build.sh "$AX" "$STATE/check-$variant" --check "${flag[@]}"
  wb=$DEV/tool-worker-r4-$variant-$S-$(date +%s)
  bash deploy/tool-worker/build.sh "$AX" "$wb" --buildah "${flag[@]}"
  tag=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["local_tag"])' "$wb/proof.json")
  c=$(buildah from "$tag")
  buildah run --network none "$c" -- sh -c 'tmux -V && script --version && test "$(readlink /usr/local/bin/bx)" = blaxsmith-tool-worker && git --version'
  [ "$variant" = guild ] && buildah run --network none --user 10001 "$c" -- sh -c 'uv --version && python3.12 --version 2>/dev/null || uv run --no-project python --version; serena --help >/dev/null && echo serena-ok'
  buildah rm "$c" >/dev/null
  bash deploy/tool-worker/publish-buildah.sh "$wb" "127.0.0.1:5001/blaxsmith-tool-worker:${tag##*:}"
  python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["image"])' "$wb/registry-proof.json" > "$STATE/worker-$variant.image"
  echo "WORKER_$variant=$(cat "$STATE/worker-$variant.image")"
done
