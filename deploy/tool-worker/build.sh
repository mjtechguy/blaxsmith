#!/usr/bin/env bash
# Build one credential-free AX runner + pinned CLI image on native Linux/amd64.
set -euo pipefail
test "$#" -ge 2 && test "$#" -le 3 || {
  echo 'usage: build.sh VERIFIED_AX_BUILD NEW_OUTPUT_DIRECTORY [--check|--buildah]' >&2
  exit 2
}
test "$#" -eq 2 || test "$3" = --check || test "$3" = --buildah || exit 2
root=$(cd "$(dirname "$0")/../.." && pwd)
ax_build=$(cd "$1" && pwd)
output=$2
test ! -e "$output" || { echo 'output directory already exists' >&2; exit 2; }
test -z "$(git -C "$root" status --porcelain)" || {
  echo 'commit the build inputs before producing an image' >&2
  exit 2
}
python3 - "$root" "$ax_build" <<'PY'
import hashlib, json, pathlib, sys
root, build = map(pathlib.Path, sys.argv[1:])
record = json.loads((build / 'provenance.json').read_text())
if record['upstream_commit'] != 'd8ed0fe38bceb7842d3c47817d53d16ccdfcb601' or record['platform'] != 'linux/amd64':
    sys.exit('AX source or platform differs from the supported pin')
patches = {
    'patch_sha256': 'fail-closed.patch',
    'egress_patch_sha256': 'egress-policy.patch',
    'bootstrap_patch_sha256': 'bootstrap-gate.patch',
    'platform_key_patch_sha256': 'platform-bootstrap-key.patch',
    'encrypted_git_patch_sha256': 'encrypted-git-bootstrap.patch',
    'command_exit_patch_sha256': 'command-exit-readback.patch',
    'task_tombstones_patch_sha256': 'task-tombstones.patch',
    'redis_ha_patch_sha256': 'redis-ha.patch',
    'consumer_recovery_patch_sha256': 'consumer-recovery.patch',
    'provider_credential_patch_sha256': 'provider-credential.patch',
    'askpass_sha256': 'blaxsmith-git-askpass',
}
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
for field, filename in patches.items():
    if record.get(field) != sha(root / 'integrations/ax' / filename):
        sys.exit(f'AX material changed: {filename}')
if record['binaries']['ax-task-runner'] != sha(build / 'ax-task-runner'):
    sys.exit('AX runner differs from verified binary')
PY
if [ "${3:-}" = --check ]; then
  echo 'AX provenance and committed build inputs match'
  exit 0
fi
test "$(uname -s)/$(uname -m)" = Linux/x86_64 && grep -qw avx2 /proc/cpuinfo || {
  echo 'native Linux/amd64 with AVX2 is required for this runtime proof' >&2
  exit 2
}
mkdir -p "$output/bin"
output=$(cd "$output" && pwd)
cp "$ax_build/ax-task-runner" "$output/bin/ax-task-runner"
cp "$root/integrations/ax/blaxsmith-git-askpass" "$output/bin/blaxsmith-git-askpass"
(
  cd "$root"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
    -o "$output/bin/blaxsmith-tool-worker" ./cmd/blaxsmith-tool-worker
)
chmod 0755 "$output/bin/"*
fingerprint=$(python3 - "$root" "$output" <<'PY'
import hashlib, pathlib, sys
root, output = map(pathlib.Path, sys.argv[1:])
h = hashlib.sha256()
for path in [root / 'deploy/runtime-proof/package-lock.json', root / 'deploy/runtime-proof/Dockerfile',
             root / 'deploy/tool-worker/Dockerfile', root / 'deploy/tool-worker/manifest.mjs',
             *sorted((output / 'bin').iterdir())]:
    h.update(path.name.encode() + b'\0' + path.read_bytes())
print(h.hexdigest()[:16])
PY
)
runtime_image="blaxsmith-runtime-proof:${fingerprint}"
worker_image="blaxsmith-tool-worker:proof-${fingerprint}"
if [ "${3:-}" = --buildah ]; then
  buildah bud --format oci --arch amd64 --network host \
    -t "$runtime_image" -f "$root/deploy/runtime-proof/Dockerfile" "$root"
  buildah bud --format oci --arch amd64 --network host \
    --build-arg "RUNTIME_PROOF_IMAGE=localhost/$runtime_image" \
    --build-context "binaries=$output/bin" \
    -t "$worker_image" -f "$root/deploy/tool-worker/Dockerfile" "$root"
  buildah_container=$(buildah from "$worker_image")
  trap 'if [ -n "${buildah_container:-}" ]; then buildah rm "$buildah_container" >/dev/null; fi' EXIT
  buildah run --network none "$buildah_container" -- node /opt/blaxsmith/manifest.mjs > "$output/cli-manifest.json"
  buildah rm "$buildah_container" >/dev/null
  buildah_container=
  buildah inspect --type image --format '{{.FromImageID}}' "$runtime_image" > "$output/runtime-image-id.txt"
  buildah inspect --type image --format '{{.FromImageID}}' "$worker_image" > "$output/worker-image-id.txt"
  buildah inspect --type image --format '{{.FromImageDigest}}' "$runtime_image" > "$output/runtime-buildah-digest.txt"
  buildah inspect --type image --format '{{.FromImageDigest}}' "$worker_image" > "$output/worker-buildah-digest.txt"
  python3 - "$output" <<'PY'
import json, pathlib, sys
output = pathlib.Path(sys.argv[1])
for prefix in ('runtime', 'worker'):
    digest = (output / f'{prefix}-buildah-digest.txt').read_text().strip()
    (output / f'{prefix}-build.json').write_text(json.dumps({'containerimage.digest': digest}) + '\n')
PY
else
  docker buildx build --platform linux/amd64 --load --metadata-file "$output/runtime-build.json" \
    -t "$runtime_image" -f "$root/deploy/runtime-proof/Dockerfile" "$root"
  docker buildx build --platform linux/amd64 --load --metadata-file "$output/worker-build.json" \
    --build-arg "RUNTIME_PROOF_IMAGE=$runtime_image" \
    --build-context "binaries=$output/bin" \
    -t "$worker_image" -f "$root/deploy/tool-worker/Dockerfile" "$root"
  docker run --rm --network none --platform linux/amd64 --entrypoint node "$worker_image" \
    /opt/blaxsmith/manifest.mjs > "$output/cli-manifest.json"
  docker image inspect --format '{{.Id}}' "$runtime_image" > "$output/runtime-image-id.txt"
  docker image inspect --format '{{.Id}}' "$worker_image" > "$output/worker-image-id.txt"
fi
python3 - "$root" "$ax_build" "$output" "$worker_image" <<'PY'
import hashlib, json, pathlib, subprocess, sys
root, ax, output = map(pathlib.Path, sys.argv[1:4])
tag = sys.argv[4]
read = lambda name: json.loads((output / name).read_text())
manifest, metadata = read('cli-manifest.json'), read('worker-build.json')
digest = metadata.get('containerimage.digest') or metadata.get('containerimage.descriptor', {}).get('digest')
if not isinstance(digest, str) or not digest.startswith('sha256:') or len(digest) != 71:
    sys.exit('builder did not report an immutable image manifest digest')
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
if manifest['ax_runner_sha256'] != sha(ax / 'ax-task-runner') or len(manifest['tools']) != 3:
    sys.exit('image contents differ from verified AX build or CLI set')
proof = {
    'schema': 'blaxsmith.tool-worker-build/v1alpha1',
    'source_commit': subprocess.check_output(['git', '-C', str(root), 'rev-parse', 'HEAD'], text=True).strip(),
    'package_lock_sha256': sha(root / 'deploy/runtime-proof/package-lock.json'),
    'ax_provenance_sha256': sha(ax / 'provenance.json'),
    'runtime_image_id': (output / 'runtime-image-id.txt').read_text().strip(),
    'worker_image_id': (output / 'worker-image-id.txt').read_text().strip(),
    'local_tag': tag,
    'local_manifest_digest': digest,
    'cli_manifest': manifest,
}
(output / 'proof.json').write_text(json.dumps(proof, indent=2) + '\n')
print(f'Verified local image: {tag} ({digest})')
print(f'Proof: {output / "proof.json"}')
PY
