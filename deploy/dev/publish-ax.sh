#!/usr/bin/env bash
# Publish verified synthetic-test images to this node's private dev registry.
set -euo pipefail
test "$#" -ge 1 && test "$#" -le 2 || { echo 'usage: publish-ax.sh VERIFIED_BUILD_DIRECTORY [DEV_GIT_CA_PEM]' >&2; exit 2; }
build=$(cd "$1" && pwd)
git_ca=${2:-}
python3 - "$build" <<'PY'
import hashlib, json, pathlib, sys
build = pathlib.Path(sys.argv[1])
record = json.loads((build / 'provenance.json').read_text())
for name in ['ax-controller', 'ax-task-runner']:
    if hashlib.sha256((build / name).read_bytes()).hexdigest() != record['binaries'][name]:
        sys.exit('Binary differs from the verified build: ' + name)
askpass = pathlib.Path('integrations/ax/blaxsmith-git-askpass')
if hashlib.sha256(askpass.read_bytes()).hexdigest() != record['askpass_sha256']:
    sys.exit('Git askpass differs from the verified build')
PY
for component in ax-controller ax-task-runner; do
  test ! -e "$build/$component.image" || { echo 'Build directory already has published images; use a fresh verified build.' >&2; exit 2; }
done
image_build=$(mktemp -d)
trap 'rm -rf "$image_build"' EXIT
mkdir -p "$image_build/usr/local/bin"
controller_base=alpine:3.24.2@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395
runner_base=alpine/git@sha256:8c843da8f112867e5d713f3bce85fbe815ec5582bd76bddb2e9121f8c7af9e8f
for component in ax-controller ax-task-runner; do
  base=$controller_base
  cp "$build/$component" "$image_build/usr/local/bin/$component"
  if [ "$component" = ax-task-runner ]; then
    base=$runner_base
    cp "$(dirname "$0")/../../integrations/ax/blaxsmith-git-askpass" "$image_build/usr/local/bin/blaxsmith-git-askpass"
    chmod 755 "$image_build/usr/local/bin/blaxsmith-git-askpass"
    if [ -n "$git_ca" ]; then
      test -f "$git_ca" || { echo 'Git CA file missing' >&2; exit 2; }
      mkdir -p "$image_build/etc/ssl/certs"
      cp "$git_ca" "$image_build/etc/ssl/certs/blaxsmith-git-ca.pem"
      tar -C "$image_build" -cf "$image_build/layer.tar" "usr/local/bin/$component" usr/local/bin/blaxsmith-git-askpass etc/ssl/certs/blaxsmith-git-ca.pem
    else
      tar -C "$image_build" -cf "$image_build/layer.tar" "usr/local/bin/$component" usr/local/bin/blaxsmith-git-askpass
    fi
  else
    tar -C "$image_build" -cf "$image_build/layer.tar" "usr/local/bin/$component"
  fi
  go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 append \
    --base "$base" --new_layer "$image_build/layer.tar" \
    --new_tag "127.0.0.1:5001/blaxsmith-$component:egress-guarded" \
    > "$build/$component.image"
done
python3 - "$build" "$controller_base" "$runner_base" "$git_ca" <<'PY'
import hashlib, json, pathlib, sys
build, controller_base, runner_base, ca = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
record = json.loads((build / 'provenance.json').read_text())
materials = {
    'controller_base': controller_base,
    'runner_base': runner_base,
    'controller_image': (build / 'ax-controller.image').read_text().strip(),
    'runner_image': (build / 'ax-task-runner.image').read_text().strip(),
    'askpass_sha256': record['askpass_sha256'],
    'git_ca_sha256': hashlib.sha256(pathlib.Path(ca).read_bytes()).hexdigest() if ca else None,
}
(build / 'publish-materials.json').write_text(json.dumps(materials, indent=2) + '\n')
PY
# The controller Deployment must explicitly use /usr/local/bin/ax-controller;
# AX already selects /usr/local/bin/ax-task-runner for its task containers.
echo "Published image digests: $build/*.image"
