#!/usr/bin/env bash
# Build the unqualified AX migration candidate without changing the default pin.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: build-candidate.sh AX_SOURCE OUTPUT_DIRECTORY' >&2; exit 2; }
ax_source=$(cd "$1" && pwd)
output=$2
integration=$(cd "$(dirname "$0")" && pwd)
expected=d0bc38bcf90bb2ad9c012ff1be9d68ff05347ba9
test "$(git -C "$ax_source" rev-parse HEAD)" = "$expected" || {
  echo 'Candidate AX revision changed; review the port before building.' >&2; exit 1;
}
test -z "$(git -C "$ax_source" status --porcelain)" || {
  echo 'AX source must be clean.' >&2; exit 1;
}
mkdir "$output"
output=$(cd "$output" && pwd)
ax_build=$(mktemp -d)
trap 'rm -rf "$ax_build"' EXIT
git -C "$ax_source" archive "$expected" | tar -x -C "$ax_build"
cd "$ax_build"
git apply --check --whitespace=error-all "$integration/candidate-d0bc38b.patch"
git apply "$integration/candidate-d0bc38b.patch"
go test ./...
go vet ./...
go test -race ./internal/server ./internal/store/redis
for component in ax ax-server ax-controller ax-task-runner; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags='-s -w' -o "$output/$component" "./cmd/$component"
done
python3 - "$output" "$integration/candidate-d0bc38b.patch" "$expected" <<'PY'
import hashlib, json, pathlib, subprocess, sys
output, patch = map(pathlib.Path, sys.argv[1:3])
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
record = {
    'qualification': 'local-candidate-only',
    'upstream_commit': sys.argv[3],
    'patch_sha256': sha(patch),
    'go_version': subprocess.check_output(['go', 'version'], text=True).strip(),
    'platform': 'linux/amd64',
    'binaries': {name: sha(output / name) for name in ['ax', 'ax-server', 'ax-controller', 'ax-task-runner']},
}
(output / 'provenance.json').write_text(json.dumps(record, indent=2) + '\n')
PY
echo "Unqualified AX candidate binaries and provenance: $output"
