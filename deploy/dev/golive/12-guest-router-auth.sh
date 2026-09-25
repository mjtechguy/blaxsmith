#!/usr/bin/env bash
# NODE. Build/publish atenet with guest-router-auth.patch and turn on --guest-client-auth:
# guest gRPC (/ateenv.*: exec, files) then needs HTTPS + the connector TokenReview on every
# router port. Run BEFORE the app switches to the HTTPS guest router (10), so the router
# already strips the connector token when the app starts sending it. Idempotent: reuses
# the build, prints the Deployment diff, asks before patching.
source "$(dirname "$0")/env.sh"
BUILD=${GUEST_ROUTER_BUILD:-$DEV/substrate-guest-router-auth}
cd "$TREE"
want=$(sha256sum integrations/substrate/guest-router-auth.patch | cut -d' ' -f1)
test -e "$BUILD/provenance.json" || bash integrations/substrate/build.sh "$DEV/substrate" "$BUILD"
python3 - "$BUILD" "$want" <<'PY'
import json, pathlib, sys
p = json.loads((pathlib.Path(sys.argv[1]) / 'provenance.json').read_text())
for k in ('router_auth_patch_sha256', 'actor_fence_patch_sha256', 'command_exit_router_auth_patch_sha256'):
    assert p.get(k), 'router build lacks ' + k
assert p.get('guest_router_auth_patch_sha256') == sys.argv[2], 'stale build: remove ' + sys.argv[1] + ' and rerun'
PY
test -e "$BUILD/atenet.image" || bash deploy/dev/publish-atenet.sh "$BUILD"
kubectl -n ate-system get deploy atenet-router -o json > "$STATE/atenet-router-before-guest-auth.json"
ROUTER_AUTH_DIFF=1 python3 deploy/dev/enable-bootstrap-router-auth.py "$BUILD"
read -r -p "patch atenet-router? [y/N] " ok; [ "$ok" = y ]
python3 deploy/dev/enable-bootstrap-router-auth.py "$BUILD"
test "$(kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="atenet-router")].image}')" \
  = "$(cat "$BUILD/atenet.image")"
kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="atenet-router")].args}' \
  | grep -q -- '--guest-client-auth'
# Refused before actor resume. The method does not exist, so even a regression reaches
# only Unimplemented in the guest daemon.
grpc=/ateenv.v1alpha.ProcessService/BlaxsmithAuthProbe
hdr=(-H "ate-target-actor: $GATE_SPACE/$GATE_TASK" -H 'content-type: application/grpc' -H 'te: trailers')
code=$(curl -s -o /dev/null -w '%{http_code}' --http2-prior-knowledge -X POST "${hdr[@]}" "http://$ROUTER_IP$grpc")
echo "plaintext guest gRPC -> $code"; test "$code" = 426
code=$(curl -s -o /dev/null -w '%{http_code}' --http2 -X POST "${hdr[@]}" \
  --cacert "$DEV/dispatch-router-ca.pem" --resolve "atenet-router.ate-system.svc:443:$ROUTER_IP" \
  "https://atenet-router.ate-system.svc$grpc")
echo "HTTPS guest gRPC without token -> $code"; test "$code" = 401
# With the connector token (from the app's Secret, via an owner-only header file, never argv):
# the router admits it and the guest answers Unimplemented (HTTP 200, grpc-status 12).
umask 077; t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
printf 'Authorization: Bearer %s\n' "$(kubectl -n blaxsmith-preview get secret preview-app-dispatch \
  -o jsonpath='{.data.bootstrap-token}' | base64 -d)" > "$t/h"
curl -s -o /dev/null -D - --http2 -X POST "${hdr[@]}" -H @"$t/h" \
  --cacert "$DEV/dispatch-router-ca.pem" --resolve "atenet-router.ate-system.svc:443:$ROUTER_IP" \
  "https://atenet-router.ate-system.svc$grpc" | grep -i -E '^(HTTP|grpc-status)' || true
rm -rf "$t"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "ate-target-actor: $GATE_SPACE/$GATE_TASK" "http://$ROUTER_IP/blaxsmith/command-exit")
echo "plaintext command-exit -> $code"; test "$code" = 426
kubectl -n ate-system logs deploy/atenet-router -c atenet-router --since=2m | grep -i -E 'error|denied' | tail -5 || true
# Next: S=$S bash 10-local-values-apply.sh (workstation; guestRouter :443), then 08-netpol.sh.
# Rollback: first clear dispatch.guestRouter on the app (terminals 503), or the app keeps
# sending the connector token to a router that would forward it into guests; then kubectl -n ate-system patch deploy atenet-router with the image/args from
# $STATE/atenet-router-before-guest-auth.json (enable-bootstrap-router-auth.py refuses to drop the flag).
