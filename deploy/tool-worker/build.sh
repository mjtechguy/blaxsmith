#!/usr/bin/env bash
# Build one credential-free AX runner + pinned CLI image on native Linux/amd64.
set -euo pipefail
test "$#" -ge 2 && test "$#" -le 7 || {
  echo 'usage: build.sh VERIFIED_AX_BUILD NEW_OUTPUT_DIRECTORY [--check|--buildah] [--git-ca PEM] [--variant NAME]' >&2
  exit 2
}
root=$(cd "$(dirname "$0")/../.." && pwd)
ax_build=$(cd "$1" && pwd)
output=$2
mode=
git_ca=
variant=
shift 2
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check|--buildah)
      test -z "$mode" || { echo 'choose one build mode' >&2; exit 2; }
      mode=$1
      ;;
    --git-ca)
      test -z "$git_ca" && test "$#" -ge 2 || { echo '--git-ca requires one PEM path' >&2; exit 2; }
      git_ca=$2
      shift
      ;;
    --variant)
      test -z "$variant" && test "$#" -ge 2 || { echo '--variant requires one name' >&2; exit 2; }
      variant=$2
      shift
      ;;
    *) echo "unknown build option: $1" >&2; exit 2 ;;
  esac
  shift
done
if [ -n "$git_ca" ]; then
  test -f "$git_ca" && test ! -L "$git_ca" || { echo 'Git CA must be a regular PEM file' >&2; exit 2; }
  python3 - "$git_ca" <<'PY'
import pathlib, ssl, sys
try:
    ssl.create_default_context(cadata=pathlib.Path(sys.argv[1]).read_text())
except (OSError, ssl.SSLError) as exc:
    sys.exit(f'invalid Git CA PEM: {exc}')
PY
fi
if [ -n "$variant" ]; then
  # A variant layers extension runtimes (deploy/tool-worker/<name>.Dockerfile)
  # onto the worker image; its pins must match the extension's declaration.
  python3 "$root/deploy/tool-worker/check-variant.py" "$variant"
fi
test ! -e "$output" || { echo 'output directory already exists' >&2; exit 2; }
test -z "$(git -C "$root" status --porcelain)" || {
  echo 'commit the build inputs before producing an image' >&2
  exit 2
}
python3 - "$root" "$ax_build" <<'PY'
import hashlib, json, pathlib, sys
root, build = map(pathlib.Path, sys.argv[1:])
record = json.loads((build / 'provenance.json').read_text())
if record['upstream_commit'] != 'f009cc81c9a571073bc1dd58cd2ed934bf2d5b1c' or record['platform'] != 'linux/amd64':
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
    'post_ready_model_patch_sha256': 'post-ready-model.patch',
    'task_resources_patch_sha256': 'task-resources.patch',
    'upstream_refresh_patch_sha256': 'upstream-refresh-f009cc8.patch',
    'opencode_provider_patch_sha256': 'opencode-provider.patch',
    'askpass_sha256': 'blaxsmith-git-askpass',
}
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
for field, filename in patches.items():
    if record.get(field) != sha(root / 'integrations/ax' / filename):
        sys.exit(f'AX material changed: {filename}')
if record['binaries']['ax-task-runner'] != sha(build / 'ax-task-runner'):
    sys.exit('AX runner differs from verified binary')
PY
if [ "$mode" = --check ]; then
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
if [ -n "$git_ca" ]; then cp "$git_ca" "$output/bin/blaxsmith-git-ca.pem"; else : > "$output/bin/blaxsmith-git-ca.pem"; fi
(
  cd "$root"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
    -o "$output/bin/blaxsmith-tool-worker" ./cmd/blaxsmith-tool-worker
)
chmod 0755 "$output/bin/"*
fingerprint=$(python3 - "$root" "$output" "$variant" <<'PY'
import hashlib, pathlib, sys
root, output = map(pathlib.Path, sys.argv[1:3])
variant = sys.argv[3]
h = hashlib.sha256()
extra = [root / f'deploy/tool-worker/{variant}.Dockerfile', root / f'deploy/tool-worker/{variant}-runtimes.json'] if variant else []
for path in [root / 'deploy/runtime-proof/package-lock.json', root / 'deploy/runtime-proof/Dockerfile',
             root / 'deploy/tool-worker/Dockerfile', root / 'deploy/tool-worker/manifest.mjs',
             root / 'deploy/tool-worker/runtimes.mjs', *extra,
             *sorted((output / 'bin').iterdir())]:
    h.update(path.name.encode() + b'\0' + path.read_bytes())
print(h.hexdigest()[:16])
PY
)
runtime_image="blaxsmith-runtime-proof:${fingerprint}"
base_image="blaxsmith-tool-worker:proof-${fingerprint}"
worker_image=$base_image
worker_metadata=worker-build.json
if [ -n "$variant" ]; then
  worker_image="blaxsmith-tool-worker:proof-${fingerprint}-${variant}"
  worker_metadata=base-worker-build.json
fi
if [ "$mode" = --buildah ]; then
  buildah bud --format oci --arch amd64 --network host \
    -t "$runtime_image" -f "$root/deploy/runtime-proof/Dockerfile" "$root"
  buildah bud --format oci --arch amd64 --network host \
    --build-arg "RUNTIME_PROOF_IMAGE=localhost/$runtime_image" \
    --build-context "binaries=$output/bin" \
    -t "$base_image" -f "$root/deploy/tool-worker/Dockerfile" "$root"
  if [ -n "$variant" ]; then
    buildah bud --format oci --arch amd64 --network host \
      --build-arg "TOOL_WORKER_IMAGE=localhost/$base_image" \
      -t "$worker_image" -f "$root/deploy/tool-worker/$variant.Dockerfile" "$root"
  fi
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
  docker buildx build --platform linux/amd64 --load --metadata-file "$output/$worker_metadata" \
    --build-arg "RUNTIME_PROOF_IMAGE=$runtime_image" \
    --build-context "binaries=$output/bin" \
    -t "$base_image" -f "$root/deploy/tool-worker/Dockerfile" "$root"
  if [ -n "$variant" ]; then
    docker buildx build --platform linux/amd64 --load --metadata-file "$output/worker-build.json" \
      --build-arg "TOOL_WORKER_IMAGE=$base_image" \
      -t "$worker_image" -f "$root/deploy/tool-worker/$variant.Dockerfile" "$root"
  fi
  docker run --rm --network none --platform linux/amd64 --entrypoint node "$worker_image" \
    /opt/blaxsmith/manifest.mjs > "$output/cli-manifest.json"
  docker image inspect --format '{{.Id}}' "$runtime_image" > "$output/runtime-image-id.txt"
  docker image inspect --format '{{.Id}}' "$worker_image" > "$output/worker-image-id.txt"
fi
python3 - "$root" "$ax_build" "$output" "$worker_image" "$variant" <<'PY'
import hashlib, json, pathlib, subprocess, sys
root, ax, output = map(pathlib.Path, sys.argv[1:4])
tag, variant = sys.argv[4:6]
read = lambda name: json.loads((output / name).read_text())
manifest, metadata = read('cli-manifest.json'), read('worker-build.json')
digest = metadata.get('containerimage.digest') or metadata.get('containerimage.descriptor', {}).get('digest')
if not isinstance(digest, str) or not digest.startswith('sha256:') or len(digest) != 71:
    sys.exit('builder did not report an immutable image manifest digest')
sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
if manifest['ax_runner_sha256'] != sha(ax / 'ax-task-runner') or len(manifest['tools']) != 3:
    sys.exit('image contents differ from verified AX build or CLI set')
if variant:
    pins = json.loads((root / f'deploy/tool-worker/{variant}-runtimes.json').read_text())
    layers = {l['id']: l['version'] for l in manifest.get('runtimes', {}).get('layers', [])}
    if manifest.get('runtimes', {}).get('variant') != variant or set(layers) != set(pins['runtimes']) or any(
            not layers[k].startswith(v['version']) for k, v in pins['runtimes'].items()):
        sys.exit(f'image runtimes differ from the {variant} variant pins')
elif 'runtimes' in manifest:
    sys.exit('base image unexpectedly carries variant runtimes')
proof = {
    'schema': 'blaxsmith.tool-worker-build/v1alpha1',
    'source_commit': subprocess.check_output(['git', '-C', str(root), 'rev-parse', 'HEAD'], text=True).strip(),
    'package_lock_sha256': sha(root / 'deploy/runtime-proof/package-lock.json'),
    'ax_provenance_sha256': sha(ax / 'provenance.json'),
    'git_ca_sha256': sha(output / 'bin/blaxsmith-git-ca.pem') if (output / 'bin/blaxsmith-git-ca.pem').stat().st_size else None,
    'runtime_image_id': (output / 'runtime-image-id.txt').read_text().strip(),
    'worker_image_id': (output / 'worker-image-id.txt').read_text().strip(),
    'variant': variant or None,
    'local_tag': tag,
    'local_manifest_digest': digest,
    'cli_manifest': manifest,
}
(output / 'proof.json').write_text(json.dumps(proof, indent=2) + '\n')
print(f'Verified local image: {tag} ({digest})')
print(f'Proof: {output / "proof.json"}')
PY
