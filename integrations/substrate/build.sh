#!/usr/bin/env bash
# Export the pinned Substrate revision, apply the reviewed patches, test, build.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: build.sh SUBSTRATE_SOURCE OUTPUT_DIRECTORY' >&2; exit 2; }
source=$(cd "$1" && pwd)
output=$2
integration=$(cd "$(dirname "$0")" && pwd)
expected=672533541dbfcd29084e4de2475267088bda3651
test "$(git -C "$source" rev-parse HEAD)" = "$expected" || {
  echo 'Substrate revision changed; review and revalidate the patches.' >&2
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
git apply --check --whitespace=error-all "$integration/actor-attestation.patch"
git apply "$integration/actor-attestation.patch"
git apply --check --whitespace=error-all "$integration/bootstrap-router-auth.patch"
git apply "$integration/bootstrap-router-auth.patch"
git apply --check --whitespace=error-all "$integration/bootstrap-actor-fence.patch"
git apply "$integration/bootstrap-actor-fence.patch"
git apply --check --whitespace=error-all "$integration/command-exit-router-auth.patch"
git apply "$integration/command-exit-router-auth.patch"
go test ./cmd/atenet/... ./internal/atunnel ./cmd/ateom-gvisor
go vet ./cmd/atenet/... ./internal/atunnel ./cmd/ateom-gvisor
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$output/atenet" ./cmd/atenet
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o "$output/ateom-gvisor" ./cmd/ateom-gvisor
python3 - "$output" "$integration/egress-policy.patch" "$integration/actor-attestation.patch" "$integration/bootstrap-router-auth.patch" "$integration/bootstrap-actor-fence.patch" "$integration/command-exit-router-auth.patch" "$expected" <<'PY'
import hashlib, json, pathlib, subprocess, sys
output, patch, attestation_patch, router_patch, fence_patch, exit_patch, revision = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3]), pathlib.Path(sys.argv[4]), pathlib.Path(sys.argv[5]), pathlib.Path(sys.argv[6]), sys.argv[7]
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
record = {
    'upstream_commit': revision,
    'patch_sha256': sha(patch),
    'attestation_patch_sha256': sha(attestation_patch),
    'router_auth_patch_sha256': sha(router_patch),
    'actor_fence_patch_sha256': sha(fence_patch),
    'command_exit_router_auth_patch_sha256': sha(exit_patch),
    'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
    'platform': 'linux/amd64',
    'binary_sha256': sha(output / 'atenet'),
    'ateom_gvisor_sha256': sha(output / 'ateom-gvisor'),
}
(output / 'provenance.json').write_text(json.dumps(record, indent=2) + '\n')
PY
echo "Verified Substrate egress/worker binaries and provenance: $output"
