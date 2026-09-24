#!/usr/bin/env bash
set -euo pipefail
test "$#" -eq 2 || { echo 'usage: build-model-probe-image.sh TOOL_WORKER_BUILD_DIRECTORY NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
root=$(cd "$(dirname "$0")/../.." && pwd)
base=$(cd "$1" && pwd)
output=$2
test ! -e "$output" || { echo 'output directory already exists' >&2; exit 2; }
test -z "$(git -C "$root" status --porcelain)" || { echo 'commit the probe inputs first' >&2; exit 2; }
mkdir -p "$output"
output=$(cd "$output" && pwd)
python3 - "$root" "$base" <<'PY'
import hashlib, json, pathlib, subprocess, sys
root, base = map(pathlib.Path, sys.argv[1:])
proof = json.loads((base / 'proof.json').read_text())
if proof['schema'] != 'blaxsmith.tool-worker-build/v1alpha1':
    sys.exit('invalid tool-worker build proof')
if proof['source_commit'] != subprocess.check_output(['git', '-C', str(root), 'rev-parse', 'HEAD'], text=True).strip():
    sys.exit('tool-worker base was built from another source commit')
PY
base_tag=$(python3 - "$base" <<'PY'
import json, pathlib, sys
print(json.loads((pathlib.Path(sys.argv[1]) / 'proof.json').read_text())['local_tag'])
PY
)
cli_sha=$(sha256sum "$root/deploy/dev/model-probe-cli" | cut -d' ' -f1)
tag="blaxsmith-tool-worker:model-probe-${cli_sha:0:16}"
buildah bud --format oci --arch amd64 --network host \
  --build-arg "TOOL_WORKER_IMAGE=localhost/$base_tag" \
  -t "$tag" -f "$root/deploy/dev/model-probe-image.Dockerfile" "$root"
container=$(buildah from "$tag")
trap 'buildah rm "$container" >/dev/null 2>&1 || true' EXIT
buildah run --network none "$container" -- /usr/local/bin/blaxsmith-model-probe-cli --version > "$output/model-probe-version.txt"
test "$(cat "$output/model-probe-version.txt")" = 'codex-cli 0.156.1'
image_cli_sha=$(buildah run --network none "$container" -- sha256sum /usr/local/bin/blaxsmith-model-probe-cli | cut -d' ' -f1)
test "$image_cli_sha" = "$cli_sha"
buildah rm "$container" >/dev/null
trap - EXIT
image_id=$(buildah inspect --type image --format '{{.FromImageID}}' "$tag")
digest=$(buildah inspect --type image --format '{{.FromImageDigest}}' "$tag")
python3 - "$root" "$base" "$output" "$tag" "$image_id" "$digest" "$cli_sha" <<'PY'
import hashlib, json, pathlib, sys
root, base, output = map(pathlib.Path, sys.argv[1:4])
tag, image_id, digest, cli_sha = sys.argv[4:]
base_proof = json.loads((base / 'proof.json').read_text())
if not digest.startswith('sha256:') or len(digest) != 71:
    sys.exit('Buildah did not return an immutable probe image digest')
proof = dict(base_proof)
proof.update({'local_tag': tag, 'local_manifest_digest': digest, 'worker_image_id': image_id,
    'base_tool_worker_proof_sha256': hashlib.sha256((base / 'proof.json').read_bytes()).hexdigest(),
    'model_probe': {'binary': '/usr/local/bin/blaxsmith-model-probe-cli', 'sha256': cli_sha,
        'version': (output / 'model-probe-version.txt').read_text().strip(),
        'provider_calls': False}})
(output / 'proof.json').write_text(json.dumps(proof, indent=2) + '\n')
print(f'Verified model-probe image: {tag} ({digest})')
print(f'Proof: {output / "proof.json"}')
PY
