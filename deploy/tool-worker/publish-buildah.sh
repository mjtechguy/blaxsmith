#!/usr/bin/env bash
# Publish a proved local image to the AX node's loopback development registry.
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: publish-buildah.sh BUILD_OUTPUT REGISTRY_TAG' >&2; exit 2; }
output=$(cd "$1" && pwd)
target=$2
case "$target" in
  127.0.0.1:5001/blaxsmith-tool-worker:*|localhost:5001/blaxsmith-tool-worker:*) ;;
  *) echo 'only the AX node loopback development registry is supported' >&2; exit 2 ;;
esac
test "${target##*:}" != latest || { echo 'moving latest tag is forbidden' >&2; exit 2; }
local_tag=$(python3 - "$output" <<'PY'
import json, pathlib, sys
proof = json.loads((pathlib.Path(sys.argv[1]) / 'proof.json').read_text())
if proof['schema'] != 'blaxsmith.tool-worker-build/v1alpha1' or proof['cli_manifest']['schema'] != 'blaxsmith.tool-worker-image/v1alpha1':
    sys.exit('invalid local image proof')
print(proof['local_tag'])
PY
)
test "${target##*:}" = "${local_tag##*:}" || {
  echo 'registry tag must match the proved local tag' >&2
  exit 2
}
local_digest=$(buildah inspect --type image "$local_tag" --format '{{.FromImageDigest}}')
python3 - "$output" "$local_digest" <<'PY'
import json, pathlib, sys
proof = json.loads((pathlib.Path(sys.argv[1]) / 'proof.json').read_text())
if proof['local_manifest_digest'] != sys.argv[2]:
    sys.exit('local image changed after proof')
PY
test ! -e "$output/registry-digest.txt" && test ! -e "$output/registry-proof.json" || {
  echo 'registry proof already exists; use a fresh build output' >&2
  exit 2
}
buildah push --tls-verify=false --digestfile "$output/registry-digest.txt" \
  "$local_tag" "docker://$target"
python3 - "$output" "$target" <<'PY'
import json, pathlib, re, sys
output, target = pathlib.Path(sys.argv[1]), sys.argv[2]
digest = (output / 'registry-digest.txt').read_text().strip()
if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
    sys.exit('registry did not return a valid image digest')
proof = json.loads((output / 'proof.json').read_text())
record = {'schema': 'blaxsmith.tool-worker-registry/v1alpha1',
          'image': target.rsplit(':', 1)[0] + '@' + digest,
          'local_manifest_digest': proof['local_manifest_digest'],
          'cli_manifest': proof['cli_manifest']}
(output / 'registry-proof.json').write_text(json.dumps(record, indent=2) + '\n')
print(record['image'])
PY
