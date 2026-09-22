#!/usr/bin/env bash
# Export the supported upstream revision, apply our reviewed patch, test, build.
# The input checkout is never edited. OUTPUT_DIRECTORY must not already exist.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: build.sh AX_SOURCE OUTPUT_DIRECTORY' >&2; exit 2; }
ax_source=$(cd "$1" && pwd)
output=$2
integration=$(cd "$(dirname "$0")" && pwd)
expected=d8ed0fe38bceb7842d3c47817d53d16ccdfcb601
test "$(git -C "$ax_source" rev-parse HEAD)" = "$expected" || {
  echo 'AX revision changed; review and revalidate the compatibility patch.' >&2
  exit 1
}
test -z "$(git -C "$ax_source" status --porcelain)" || {
  echo 'AX source must be clean.' >&2; exit 1
}
mkdir "$output"
output=$(cd "$output" && pwd)
ax_build=$(mktemp -d)
trap 'rm -rf "$ax_build"' EXIT
git -C "$ax_source" archive "$expected" | tar -x -C "$ax_build"
cd "$ax_build"
git apply --check --whitespace=error-all "$integration/fail-closed.patch"
git apply "$integration/fail-closed.patch"
git apply --check --whitespace=error-all "$integration/egress-policy.patch"
git apply "$integration/egress-policy.patch"
git apply --check --whitespace=error-all "$integration/bootstrap-gate.patch"
git apply "$integration/bootstrap-gate.patch"
go test ./...
go vet ./...
for component in ax-controller ax-task-runner; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags='-s -w' -o "$output/$component" "./cmd/$component"
done
python3 - "$output" "$integration/fail-closed.patch" "$integration/egress-policy.patch" "$integration/bootstrap-gate.patch" "$expected" <<'PY'
import hashlib, json, pathlib, subprocess, sys
output, patch, egress_patch, bootstrap_patch, revision = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3]), pathlib.Path(sys.argv[4]), sys.argv[5]
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
record = {
    'upstream_commit': revision,
    'patch_sha256': sha(patch),
    'egress_patch_sha256': sha(egress_patch),
    'bootstrap_patch_sha256': sha(bootstrap_patch),
    'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
    'platform': 'linux/amd64',
    'binaries': {name: sha(output / name) for name in ['ax-controller', 'ax-task-runner']},
}
(output / 'provenance.json').write_text(json.dumps(record, indent=2) + '\n')
PY
echo "Verified AX binaries and provenance: $output"
