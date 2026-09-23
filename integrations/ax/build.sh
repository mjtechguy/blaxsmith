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
git apply --check --whitespace=error-all "$integration/platform-bootstrap-key.patch"
git apply "$integration/platform-bootstrap-key.patch"
git apply --check --whitespace=error-all "$integration/encrypted-git-bootstrap.patch"
git apply "$integration/encrypted-git-bootstrap.patch"
git apply --check --whitespace=error-all "$integration/command-exit-readback.patch"
git apply "$integration/command-exit-readback.patch"
git apply --check --whitespace=error-all "$integration/task-tombstones.patch"
git apply "$integration/task-tombstones.patch"
git apply --check --whitespace=error-all "$integration/redis-ha.patch"
git apply "$integration/redis-ha.patch"
go test ./...
go vet ./...
for component in ax-server ax-controller ax-task-runner; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags='-s -w' -o "$output/$component" "./cmd/$component"
done
python3 - "$output" "$integration/fail-closed.patch" "$integration/egress-policy.patch" "$integration/bootstrap-gate.patch" "$integration/platform-bootstrap-key.patch" "$integration/encrypted-git-bootstrap.patch" "$integration/command-exit-readback.patch" "$integration/task-tombstones.patch" "$integration/redis-ha.patch" "$integration/blaxsmith-git-askpass" "$expected" <<'PY'
import hashlib, json, pathlib, subprocess, sys
output, patch, egress_patch, bootstrap_patch, platform_key_patch, encrypted_git_patch, command_exit_patch, tombstone_patch, redis_ha_patch, askpass, revision = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3]), pathlib.Path(sys.argv[4]), pathlib.Path(sys.argv[5]), pathlib.Path(sys.argv[6]), pathlib.Path(sys.argv[7]), pathlib.Path(sys.argv[8]), pathlib.Path(sys.argv[9]), pathlib.Path(sys.argv[10]), sys.argv[11]
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
record = {
    'upstream_commit': revision,
    'patch_sha256': sha(patch),
    'egress_patch_sha256': sha(egress_patch),
    'bootstrap_patch_sha256': sha(bootstrap_patch),
    'platform_key_patch_sha256': sha(platform_key_patch),
    'encrypted_git_patch_sha256': sha(encrypted_git_patch),
    'command_exit_patch_sha256': sha(command_exit_patch),
    'task_tombstones_patch_sha256': sha(tombstone_patch),
    'redis_ha_patch_sha256': sha(redis_ha_patch),
    'askpass_sha256': sha(askpass),
    'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
    'platform': 'linux/amd64',
    'binaries': {name: sha(output / name) for name in ['ax-server', 'ax-controller', 'ax-task-runner']},
}
(output / 'provenance.json').write_text(json.dumps(record, indent=2) + '\n')
PY
echo "Verified AX binaries and provenance: $output"
