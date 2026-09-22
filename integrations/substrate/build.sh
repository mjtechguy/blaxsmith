#!/usr/bin/env bash
# Export the pinned Substrate revision, apply the egress patch, test, build.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: build.sh SUBSTRATE_SOURCE OUTPUT_DIRECTORY' >&2; exit 2; }
source=$(cd "$1" && pwd)
output=$2
integration=$(cd "$(dirname "$0")" && pwd)
expected=672533541dbfcd29084e4de2475267088bda3651
test "$(git -C "$source" rev-parse HEAD)" = "$expected" || {
  echo 'Substrate revision changed; review and revalidate the egress patch.' >&2
  exit 1
}
mkdir "$output"
output=$(cd "$output" && pwd)
build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT
git -C "$source" archive "$expected" | tar -x -C "$build"
cd "$build"
git apply --check --whitespace=error-all "$integration/egress-policy.patch"
git apply "$integration/egress-policy.patch"
go test ./cmd/atenet/...
go vet ./cmd/atenet/...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$output/atenet" ./cmd/atenet
python3 - "$output" "$integration/egress-policy.patch" "$expected" <<'PY'
import hashlib, json, pathlib, subprocess, sys
output, patch, revision = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
record = {
    'upstream_commit': revision,
    'patch_sha256': sha(patch),
    'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
    'platform': 'linux/amd64',
    'binary_sha256': sha(output / 'atenet'),
}
(output / 'provenance.json').write_text(json.dumps(record, indent=2) + '\n')
PY
echo "Verified Substrate egress binary and provenance: $output"
