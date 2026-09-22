#!/usr/bin/env bash
# Replace only the verified ateom-gvisor binary in the pinned dev worker image.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: publish-ateom.sh VERIFIED_BUILD_DIRECTORY BASE_IMAGE_DIGEST' >&2; exit 2; }
build=$(cd "$1" && pwd)
base=$2
case "$base" in *@sha256:*) ;; *) echo 'base image must be pinned by digest' >&2; exit 2 ;; esac
python3 - "$build" <<'PY'
import hashlib, json, pathlib, sys
build = pathlib.Path(sys.argv[1])
record = json.loads((build / 'provenance.json').read_text())
if hashlib.sha256((build / 'ateom-gvisor').read_bytes()).hexdigest() != record['ateom_gvisor_sha256']:
    sys.exit('Worker binary differs from verified build')
PY
entrypoint=$(go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 config "$base" | python3 -c 'import json,sys; print(json.load(sys.stdin)["config"]["Entrypoint"][0])')
test "$entrypoint" = /ko-app/ateom-gvisor || { echo "unexpected base entrypoint: $entrypoint" >&2; exit 1; }
image_build=$(mktemp -d)
trap 'rm -rf "$image_build"' EXIT
mkdir -p "$image_build/ko-app"
cp "$build/ateom-gvisor" "$image_build/ko-app/ateom-gvisor"
tar -C "$image_build" -cf "$image_build/layer.tar" ko-app/ateom-gvisor
go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 append \
  --base "$base" --new_layer "$image_build/layer.tar" \
  --new_tag '127.0.0.1:5001/blaxsmith-ateom-gvisor:actor-attestation' \
  > "$build/ateom-gvisor.image"
echo "Published worker image digest: $(cat "$build/ateom-gvisor.image")"
