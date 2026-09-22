#!/usr/bin/env bash
# Publish verified synthetic-test images to this node's private dev registry.
set -euo pipefail
test "$#" -eq 1 || { echo 'usage: publish-ax.sh VERIFIED_BUILD_DIRECTORY' >&2; exit 2; }
build=$(cd "$1" && pwd)
python3 - "$build" <<'PY'
import hashlib, json, pathlib, sys
build = pathlib.Path(sys.argv[1])
record = json.loads((build / 'provenance.json').read_text())
for name in ['ax-controller', 'ax-task-runner']:
    if hashlib.sha256((build / name).read_bytes()).hexdigest() != record['binaries'][name]:
        sys.exit('Binary differs from the verified build: ' + name)
PY
image_build=$(mktemp -d)
trap 'rm -rf "$image_build"' EXIT
mkdir -p "$image_build/usr/local/bin"
base=alpine:3.24.2@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395
for component in ax-controller ax-task-runner; do
  cp "$build/$component" "$image_build/usr/local/bin/$component"
  tar -C "$image_build" -cf "$image_build/layer.tar" "usr/local/bin/$component"
  go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 append \
    --base "$base" --new_layer "$image_build/layer.tar" \
    --new_tag "127.0.0.1:5001/blaxsmith-$component:fail-closed" \
    > "$build/$component.image"
done
# The controller Deployment must explicitly use /usr/local/bin/ax-controller;
# AX already selects /usr/local/bin/ax-task-runner for its task containers.
echo "Published image digests: $build/*.image"
