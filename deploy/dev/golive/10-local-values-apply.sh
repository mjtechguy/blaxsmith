#!/usr/bin/env bash
# WORKSTATION. Edit main-checkout values (keeping the user's uncommitted lines), render the
# chart from the MVP branch, diff, apply. Usage: S=<sha7> bash 10-local-values-apply.sh
set -euo pipefail
: "${S:?}"
REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
KEY=${KEY:-~/.ssh/mj_hetzner_091123} NODE=${NODE:-root@135.181.34.21}
REF=${BUNDLE_REF:-refs/heads/main}
GUEST_KEY=${GUEST_KEY:-guestRouter}   # match the B2 chart value name
GUEST=atenet-router.ate-system.svc:443   # router HTTPS; host = its cert SAN. Run 12 first.
VALUES=$REPO/deploy/dev/preview-app-values.yaml
APP=${APP:-$(ssh -i "$KEY" "$NODE" cat /opt/blaxsmith-dev/mvp-state-$S/app.image)}
WORKER=${WORKER:-$(ssh -i "$KEY" "$NODE" cat /opt/blaxsmith-dev/mvp-state-$S/worker.image)}
[[ $APP =~ ^127\.0\.0\.1:5001/blaxsmith-app@sha256:[0-9a-f]{64}$ ]]
[[ $WORKER =~ ^127\.0\.0\.1:5001/blaxsmith-tool-worker@sha256:[0-9a-f]{64}$ ]]
cp "$VALUES" "/tmp/preview-app-values.before-$S.yaml"
python3 - "$VALUES" "$APP" "$WORKER" "$GUEST_KEY" "$GUEST" <<'PY'
import pathlib, re, sys
p, app, worker, key, guest = pathlib.Path(sys.argv[1]), *sys.argv[2:]
t = p.read_text()
t, a = re.subn(r'(?m)^image: .*$', lambda m: 'image: ' + app, t)
t, w = re.subn(r'(?m)^  workerImage: .*$', lambda m: '  workerImage: ' + worker, t)
line = f'  {key}: {guest}'
t, g = re.subn(rf'(?m)^  {key}: .*$', lambda m: line, t)
if not g:
    t, g = re.subn(r'(?m)^(  workerPool: .*)$', lambda m: line + '\n' + m.group(1), t)
assert (a, w, g) == (1, 1, 1), (a, w, g)
p.write_text(t)
PY
grep -q 'dispatch-tools@sha256:f5824b7b21e60327e9d336a7023d054e818f0c2c85fb9f78813cec1dad191079' "$VALUES"
grep -q 'cliSHA256: 91435374643d5b3a294c7b02bef04ce05d706087b13bd8ed4e5ffa15d5fd1992' "$VALUES"
git -C "$REPO" diff -- deploy/dev/preview-app-values.yaml
chart=$(mktemp -d); trap 'rm -rf "$chart"' EXIT
git -C "$REPO" archive "$REF" deploy/charts/blaxsmith-app | tar -x -C "$chart"
helm lint "$chart/deploy/charts/blaxsmith-app" -f "$VALUES"
helm template preview "$chart/deploy/charts/blaxsmith-app" --namespace blaxsmith-preview -f "$VALUES" > "/tmp/preview-$S.yaml"
grep -A1 'name: BLAXSMITH_GUEST_ROUTER' "/tmp/preview-$S.yaml" | grep -qF "$GUEST"
grep -qF "$APP" "/tmp/preview-$S.yaml" && grep -qF "$WORKER" "/tmp/preview-$S.yaml"
remote="KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl -n blaxsmith-preview"
ssh -i "$KEY" "$NODE" "$remote diff -f -" < "/tmp/preview-$S.yaml" || true
read -r -p "apply? [y/N] " ok; [ "$ok" = y ]
ssh -i "$KEY" "$NODE" "$remote apply -f -" < "/tmp/preview-$S.yaml"
ssh -i "$KEY" "$NODE" "$remote rollout status deploy/preview-app --timeout=300s"
