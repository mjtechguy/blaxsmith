#!/usr/bin/env bash
# Publish a verified atenet binary to the dedicated dev node's private registry.
set -euo pipefail
test "$#" -eq 1 || { echo 'usage: publish-atenet.sh VERIFIED_BUILD_DIRECTORY' >&2; exit 2; }
build=$(cd "$1" && pwd)
python3 - "$build" <<'PY'
import hashlib, json, pathlib, sys
build = pathlib.Path(sys.argv[1])
record = json.loads((build / 'provenance.json').read_text())
if hashlib.sha256((build / 'atenet').read_bytes()).hexdigest() != record['binary_sha256']:
    sys.exit('Binary differs from the verified build')
PY
image_build=$(mktemp -d)
trap 'rm -rf "$image_build"' EXIT
mkdir -p "$image_build/usr/local/bin"
cp "$build/atenet" "$image_build/usr/local/bin/atenet"
tar -C "$image_build" -cf "$image_build/layer.tar" usr/local/bin/atenet
base=alpine:3.24.2@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395
go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 append \
  --base "$base" --new_layer "$image_build/layer.tar" \
  --new_tag '127.0.0.1:5001/blaxsmith-atenet:egress-guarded' \
  > "$build/atenet.image"
echo "Published image digest: $(cat "$build/atenet.image")"
